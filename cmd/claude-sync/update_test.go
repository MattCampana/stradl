package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tarEntry is one file in a test release archive.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
}

func makeTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0755, Size: int64(len(e.body)), Typeflag: e.typeflag}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag == tar.TypeSymlink {
			hdr.Linkname = e.body
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// setReleaseRepo points releaseRepo (and optionally the GitHub API) at test
// values for the duration of the test.
func setReleaseRepo(t *testing.T, repo, apiBase string) {
	t.Helper()
	oldRepo, oldAPI, oldQuiet := releaseRepo, githubAPIBase, quiet
	releaseRepo = repo
	if apiBase != "" {
		githubAPIBase = apiBase
	}
	quiet = true
	t.Cleanup(func() { releaseRepo, githubAPIBase, quiet = oldRepo, oldAPI, oldQuiet })
}

func TestReleaseAssetName(t *testing.T) {
	tests := []struct {
		goos, goarch string
		want         string
		wantErr      bool
	}{
		{"darwin", "arm64", "stradl_darwin_arm64.tar.gz", false},
		{"darwin", "amd64", "stradl_darwin_amd64.tar.gz", false},
		{"linux", "arm64", "stradl_linux_arm64.tar.gz", false},
		{"linux", "amd64", "stradl_linux_amd64.tar.gz", false},
		{"windows", "amd64", "", true},
		{"darwin", "386", "", true},
	}
	for _, tt := range tests {
		got, err := releaseAssetName(tt.goos, tt.goarch)
		if (err != nil) != tt.wantErr {
			t.Errorf("releaseAssetName(%s, %s) error = %v, wantErr %v", tt.goos, tt.goarch, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("releaseAssetName(%s, %s) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestCheckReleaseRepo(t *testing.T) {
	for _, repo := range []string{"REPLACE_OWNER/stradl", "", "stradl", "/stradl", "owner/"} {
		setReleaseRepo(t, repo, "")
		err := checkReleaseRepo()
		if err == nil {
			t.Errorf("checkReleaseRepo() with %q: expected error", repo)
			continue
		}
		if !strings.Contains(err.Error(), "-X main.releaseRepo=") {
			t.Errorf("checkReleaseRepo() with %q: error should explain the ldflag, got %v", repo, err)
		}
		if projectURL() != "" {
			t.Errorf("projectURL() with %q = %q, want empty", repo, projectURL())
		}
	}

	setReleaseRepo(t, "acme/stradl", "")
	if err := checkReleaseRepo(); err != nil {
		t.Errorf("checkReleaseRepo() with acme/stradl: %v", err)
	}
	if got := projectURL(); got != "https://github.com/acme/stradl" {
		t.Errorf("projectURL() = %q", got)
	}
}

func TestUpdateRefusesPlaceholderRepo(t *testing.T) {
	setReleaseRepo(t, "REPLACE_OWNER/stradl", "http://127.0.0.1:1") // must not be contacted
	if err := runUpdate(context.Background(), true, ""); err == nil || !strings.Contains(err.Error(), "doesn't know which GitHub repository") {
		t.Errorf("runUpdate() with placeholder repo: got %v", err)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("aaaa  stradl_darwin_arm64.tar.gz\n" +
		"bbbb *stradl_linux_amd64.tar.gz\n" +
		"malformed line here\n" +
		"\n")

	if got, err := checksumFor(sums, "stradl_darwin_arm64.tar.gz"); err != nil || got != "aaaa" {
		t.Errorf("checksumFor(darwin) = %q, %v", got, err)
	}
	if got, err := checksumFor(sums, "stradl_linux_amd64.tar.gz"); err != nil || got != "bbbb" {
		t.Errorf("checksumFor(linux, binary-mode marker) = %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "stradl_linux_arm64.tar.gz"); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Errorf("checksumFor(missing) error = %v, want 'no entry'", err)
	}
	if _, err := checksumFor(nil, "stradl_darwin_arm64.tar.gz"); err == nil {
		t.Error("checksumFor(empty) expected error")
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("release archive bytes")
	want := sha256Hex(data)

	if err := verifySHA256(data, want); err != nil {
		t.Errorf("verifySHA256(correct) = %v", err)
	}
	if err := verifySHA256(data, strings.ToUpper(want)); err != nil {
		t.Errorf("verifySHA256 should compare case-insensitively: %v", err)
	}
	if err := verifySHA256(data, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("verifySHA256(wrong) = %v, want mismatch", err)
	}
}

func TestExtractBinary(t *testing.T) {
	t.Run("top-level binary next to LICENSE", func(t *testing.T) {
		archive := makeTarGz(t, tarEntry{name: "LICENSE", body: "MIT"}, tarEntry{name: "stradl", body: "#!new-binary"})
		got, err := extractBinary(archive, "stradl")
		if err != nil || string(got) != "#!new-binary" {
			t.Fatalf("extractBinary() = %q, %v", got, err)
		}
	})

	t.Run("binary inside a directory", func(t *testing.T) {
		archive := makeTarGz(t, tarEntry{name: "stradl_darwin_arm64/", typeflag: tar.TypeDir}, tarEntry{name: "stradl_darwin_arm64/stradl", body: "nested"})
		got, err := extractBinary(archive, "stradl")
		if err != nil || string(got) != "nested" {
			t.Fatalf("extractBinary() = %q, %v", got, err)
		}
	})

	t.Run("symlink named stradl is ignored", func(t *testing.T) {
		archive := makeTarGz(t, tarEntry{name: "stradl", body: "/etc/passwd", typeflag: tar.TypeSymlink})
		if _, err := extractBinary(archive, "stradl"); err == nil || !strings.Contains(err.Error(), "does not contain") {
			t.Fatalf("extractBinary() error = %v, want 'does not contain'", err)
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		archive := makeTarGz(t, tarEntry{name: "LICENSE", body: "MIT"}, tarEntry{name: "stradl-helper", body: "x"})
		if _, err := extractBinary(archive, "stradl"); err == nil {
			t.Fatal("extractBinary() expected error for archive without stradl")
		}
	})

	t.Run("empty binary", func(t *testing.T) {
		archive := makeTarGz(t, tarEntry{name: "stradl", body: ""})
		if _, err := extractBinary(archive, "stradl"); err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("extractBinary() error = %v, want 'empty'", err)
		}
	})

	t.Run("not gzip", func(t *testing.T) {
		if _, err := extractBinary([]byte("plain text"), "stradl"); err == nil || !strings.Contains(err.Error(), "gzip") {
			t.Fatalf("extractBinary() error = %v, want gzip error", err)
		}
	})
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "stradl")
	if err := os.WriteFile(exe, []byte("old"), 0750); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(exe, []byte("new binary")); err != nil {
		t.Fatalf("replaceExecutable() = %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "new binary" {
		t.Fatalf("after replace: %q, %v", got, err)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0750 {
		t.Errorf("mode = %v, want original 0750 preserved", info.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover files after replace: %v", names)
	}
}

func TestReplaceExecutableReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only directories")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "stradl")
	if err := os.WriteFile(exe, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

	err := replaceExecutable(exe, []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "no permission") {
		t.Fatalf("replaceExecutable() in read-only dir = %v, want permission hint", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Errorf("binary changed despite failure: %q", got)
	}
}

// fakeReleaseServer serves a GitHub "latest release" response for
// owner/stradl plus its assets. checksums maps asset name to the hash listed
// in checksums.txt; omit checksums.txt entirely with withChecksums=false.
type fakeRelease struct {
	tag           string
	assets        map[string][]byte
	withChecksums bool
	checksumOver  map[string]string // asset -> hash to list instead of the real one
}

func (f fakeRelease) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/stradl/releases/latest":
			rel := GitHubRelease{TagName: f.tag}
			for name := range f.assets {
				rel.Assets = append(rel.Assets, ReleaseAsset{Name: name, BrowserDownloadURL: srv.URL + "/dl/" + name})
			}
			if f.withChecksums {
				rel.Assets = append(rel.Assets, ReleaseAsset{Name: checksumsAsset, BrowserDownloadURL: srv.URL + "/dl/" + checksumsAsset})
			}
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/dl/"+checksumsAsset && f.withChecksums:
			for name, data := range f.assets {
				sum := sha256Hex(data)
				if over, ok := f.checksumOver[name]; ok {
					sum = over
				}
				fmt.Fprintf(w, "%s  %s\n", sum, name)
			}
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			data, ok := f.assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReleaseBinary(t *testing.T) {
	archive := makeTarGz(t, tarEntry{name: "LICENSE", body: "MIT"}, tarEntry{name: "stradl", body: "darwin-arm64-binary"})
	assets := map[string][]byte{"stradl_darwin_arm64.tar.gz": archive}

	t.Run("verified download", func(t *testing.T) {
		srv := fakeRelease{tag: "v1.2.0", assets: assets, withChecksums: true}.serve(t)
		setReleaseRepo(t, "owner/stradl", srv.URL)
		rel, err := getLatestRelease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := fetchReleaseBinary(context.Background(), rel, "darwin", "arm64")
		if err != nil || string(got) != "darwin-arm64-binary" {
			t.Fatalf("fetchReleaseBinary() = %q, %v", got, err)
		}
	})

	t.Run("tampered archive is refused", func(t *testing.T) {
		srv := fakeRelease{tag: "v1.2.0", assets: assets, withChecksums: true,
			checksumOver: map[string]string{"stradl_darwin_arm64.tar.gz": strings.Repeat("ab", 32)}}.serve(t)
		setReleaseRepo(t, "owner/stradl", srv.URL)
		rel, _ := getLatestRelease(context.Background())
		if _, err := fetchReleaseBinary(context.Background(), rel, "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatalf("fetchReleaseBinary() error = %v, want mismatch", err)
		}
	})

	t.Run("release without checksums.txt is refused", func(t *testing.T) {
		srv := fakeRelease{tag: "v1.2.0", assets: assets}.serve(t)
		setReleaseRepo(t, "owner/stradl", srv.URL)
		rel, _ := getLatestRelease(context.Background())
		if _, err := fetchReleaseBinary(context.Background(), rel, "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "unverified") {
			t.Fatalf("fetchReleaseBinary() error = %v, want refusal", err)
		}
	})

	t.Run("no build for this platform", func(t *testing.T) {
		srv := fakeRelease{tag: "v1.2.0", assets: assets, withChecksums: true}.serve(t)
		setReleaseRepo(t, "owner/stradl", srv.URL)
		rel, _ := getLatestRelease(context.Background())
		if _, err := fetchReleaseBinary(context.Background(), rel, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "linux/amd64") {
			t.Fatalf("fetchReleaseBinary() error = %v, want platform error", err)
		}
	})
}

func TestGetLatestReleaseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	setReleaseRepo(t, "owner/missing", srv.URL)
	if _, err := getLatestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "no published release") {
		t.Errorf("404: got %v", err)
	}
	setReleaseRepo(t, "owner/limited", srv.URL)
	if _, err := getLatestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("403: got %v", err)
	}
}

func TestRunUpdate(t *testing.T) {
	assetName, err := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no release build for this platform: %v", err)
	}
	t.Setenv("HOME", t.TempDir()) // no launch agent plist

	archive := makeTarGz(t, tarEntry{name: "stradl", body: "v1.2.0 binary"})
	srv := fakeRelease{tag: "v1.2.0", assets: map[string][]byte{assetName: archive}, withChecksums: true}.serve(t)
	setReleaseRepo(t, "owner/stradl", srv.URL)

	oldVersion := version
	t.Cleanup(func() { version = oldVersion })

	newExe := func(t *testing.T) string {
		exe := filepath.Join(t.TempDir(), "stradl")
		if err := os.WriteFile(exe, []byte("old binary"), 0755); err != nil {
			t.Fatal(err)
		}
		return exe
	}
	content := func(t *testing.T, p string) string {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("installs newer release", func(t *testing.T) {
		version = "1.1.0"
		exe := newExe(t)
		if err := runUpdate(context.Background(), false, exe); err != nil {
			t.Fatalf("runUpdate() = %v", err)
		}
		if got := content(t, exe); got != "v1.2.0 binary" {
			t.Errorf("binary = %q, want the new release", got)
		}
	})

	t.Run("check only leaves binary alone", func(t *testing.T) {
		version = "1.1.0"
		exe := newExe(t)
		if err := runUpdate(context.Background(), true, exe); err != nil {
			t.Fatalf("runUpdate(check) = %v", err)
		}
		if got := content(t, exe); got != "old binary" {
			t.Errorf("binary = %q, want unchanged", got)
		}
	})

	t.Run("already up to date", func(t *testing.T) {
		for _, v := range []string{"1.2.0", "v1.2.0", "1.3.0"} {
			version = v
			exe := newExe(t)
			if err := runUpdate(context.Background(), false, exe); err != nil {
				t.Fatalf("runUpdate() at %s = %v", v, err)
			}
			if got := content(t, exe); got != "old binary" {
				t.Errorf("at %s binary = %q, want unchanged", v, got)
			}
		}
	})
}
