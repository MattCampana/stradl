package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/util"
)

// releaseRepo is the GitHub "owner/name" repository that publishes Stradl
// releases. Release builds inject it with
// -ldflags "-X main.releaseRepo=<owner>/stradl"; `update` and `changelog`
// refuse to run while it still holds the placeholder.
var releaseRepo = "MattCampana/stradl"

// githubAPIBase is the GitHub REST API root. It is a variable so tests can
// point it at an httptest server.
var githubAPIBase = "https://api.github.com"

const (
	// releaseRepoPlaceholder marks a build made without -X main.releaseRepo.
	releaseRepoPlaceholder = "REPLACE_OWNER"

	// binaryName is the executable inside every release archive.
	binaryName = "stradl"

	// checksumsAsset lists "<sha256>  <asset filename>" for every archive in
	// a release.
	checksumsAsset = "checksums.txt"

	// maxBinarySize caps release downloads and the extracted executable
	// (200MB), so a bad or hostile response can't exhaust memory.
	maxBinarySize = 200 * 1024 * 1024

	// launchAgentLabel is the background agent's launchd label; update
	// restarts that job onto the new binary.
	launchAgentLabel = agent.Label
)

// GitHubRelease is the subset of the GitHub release API response Stradl uses.
type GitHubRelease struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	PublishedAt string         `json:"published_at"`
	Assets      []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is one downloadable file attached to a release.
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// assetURL returns the download URL of the named asset.
func (r *GitHubRelease) assetURL(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL, true
		}
	}
	return "", false
}

// checkReleaseRepo reports whether this build knows where its releases live.
func checkReleaseRepo() error {
	owner, name, ok := strings.Cut(releaseRepo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(releaseRepo, releaseRepoPlaceholder) {
		return fmt.Errorf("this build of stradl doesn't know which GitHub repository publishes its releases.\n" +
			"  Install a release build, or rebuild with -ldflags \"-X main.releaseRepo=<owner>/stradl\"")
	}
	return nil
}

// projectURL is the GitHub page for this build's release repository, or ""
// when the build has no release repository configured.
func projectURL() string {
	if checkReleaseRepo() != nil {
		return ""
	}
	return "https://github.com/" + releaseRepo
}

// releaseAssetName returns the release archive built for goos/goarch, per the
// naming contract stradl_<goos>_<goarch>.tar.gz.
func releaseAssetName(goos, goarch string) (string, error) {
	switch goos {
	case "darwin", "linux":
	default:
		return "", fmt.Errorf("stradl releases are not built for %s", goos)
	}
	switch goarch {
	case "arm64", "amd64":
	default:
		return "", fmt.Errorf("stradl releases are not built for %s/%s", goos, goarch)
	}
	return fmt.Sprintf("%s_%s_%s.tar.gz", binaryName, goos, goarch), nil
}

// githubGet fetches a GitHub API path (e.g. "/repos/o/r/releases/latest") and
// decodes the JSON response into v.
func githubGet(ctx context.Context, apiPath string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+apiPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "stradl/"+version)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("no published release found for github.com/%s", releaseRepo)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("GitHub API rate limit reached (status %d); try again later", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	return json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(v)
}

// getLatestRelease returns the newest non-prerelease release of releaseRepo.
func getLatestRelease(ctx context.Context) (*GitHubRelease, error) {
	var release GitHubRelease
	if err := githubGet(ctx, "/repos/"+releaseRepo+"/releases/latest", &release); err != nil {
		return nil, err
	}
	if release.TagName == "" {
		return nil, fmt.Errorf("GitHub returned a release without a tag")
	}
	return &release, nil
}

// getAllReleases returns up to limit releases of releaseRepo, newest first.
func getAllReleases(ctx context.Context, limit int) ([]GitHubRelease, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100 // GitHub's per_page maximum
	}
	var releases []GitHubRelease
	if err := githubGet(ctx, fmt.Sprintf("/repos/%s/releases?per_page=%d", releaseRepo, limit), &releases); err != nil {
		return nil, err
	}
	return releases, nil
}

// downloadAsset fetches url, refusing responses larger than maxBinarySize.
func downloadAsset(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stradl/"+version)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinarySize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBinarySize {
		return nil, fmt.Errorf("download exceeds maximum size of %d bytes", maxBinarySize)
	}
	return data, nil
}

// checksumFor returns the sha256 that checksums.txt lists for asset. Lines use
// the sha256sum format "<hash>  <filename>" ("*" prefix = binary mode).
func checksumFor(checksums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s has no entry for %s", checksumsAsset, asset)
}

// verifySHA256 checks data against a hex-encoded sha256 (case-insensitive).
func verifySHA256(data []byte, want string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: release lists %s, downloaded %s", want, got)
	}
	return nil
}

// extractBinary returns the contents of the regular file called name inside a
// .tar.gz archive. Entries are matched by base name, so "stradl" and
// "stradl_darwin_arm64/stradl" both work; nothing is written to disk by entry
// name, so hostile paths in the archive have no effect.
func extractBinary(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("release archive is not a gzip file: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("corrupt release archive: %w", err)
		}
		if !hdr.FileInfo().Mode().IsRegular() || path.Base(hdr.Name) != name {
			continue
		}
		if hdr.Size > maxBinarySize {
			return nil, fmt.Errorf("%s in release archive exceeds %d bytes", name, maxBinarySize)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxBinarySize+1))
		if err != nil {
			return nil, fmt.Errorf("corrupt release archive: %w", err)
		}
		if int64(len(data)) > maxBinarySize {
			return nil, fmt.Errorf("%s in release archive exceeds %d bytes", name, maxBinarySize)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%s in release archive is empty", name)
		}
		return data, nil
	}
	return nil, fmt.Errorf("release archive does not contain %q", name)
}

// fetchReleaseBinary downloads the archive for goos/goarch from release,
// verifies it against the release's checksums.txt, and returns the extracted
// stradl executable. A release without checksums.txt is refused.
func fetchReleaseBinary(ctx context.Context, release *GitHubRelease, goos, goarch string) ([]byte, error) {
	assetName, err := releaseAssetName(goos, goarch)
	if err != nil {
		return nil, err
	}
	archiveURL, ok := release.assetURL(assetName)
	if !ok {
		return nil, fmt.Errorf("release %s has no build for %s/%s (%s)", release.TagName, goos, goarch, assetName)
	}
	checksumsURL, ok := release.assetURL(checksumsAsset)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s; refusing to install an unverified binary", release.TagName, checksumsAsset)
	}

	if !quiet {
		fmt.Printf("%s⋯%s Downloading %s...\n", colorDim, colorReset, assetName)
	}
	archive, err := downloadAsset(ctx, archiveURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download %s: %w", assetName, err)
	}
	checksums, err := downloadAsset(ctx, checksumsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download %s: %w", checksumsAsset, err)
	}
	want, err := checksumFor(checksums, assetName)
	if err != nil {
		return nil, err
	}
	if err := verifySHA256(archive, want); err != nil {
		return nil, fmt.Errorf("%s: %w", assetName, err)
	}
	if !quiet {
		fmt.Printf("%s✓%s Checksum verified\n", colorGreen, colorReset)
	}

	return extractBinary(archive, binaryName)
}

// replaceExecutable atomically swaps the file at execPath for newBinary: the
// new binary is written to a temp file in the same directory, flushed, and
// renamed over the old one. A crash at any point leaves either the old or the
// new binary in place, never a partial file, and the running process keeps
// its (now unlinked) original.
func replaceExecutable(execPath string, newBinary []byte) error {
	dir := filepath.Dir(execPath)
	mode := os.FileMode(0755)
	if info, err := os.Stat(execPath); err == nil {
		mode = info.Mode().Perm() | 0100
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(execPath)+".update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("no permission to write to %s; rerun with sudo, or reinstall stradl into a directory you own", dir)
		}
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to %s: %w", step, err)
	}

	if _, err := tmp.Write(newBinary); err != nil {
		return fail("write new binary", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail("make new binary executable", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("flush new binary", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to close new binary: %w", err)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to replace %s: %w", execPath, err)
	}
	return nil
}

// runUpdate checks releaseRepo for a newer release and, unless checkOnly,
// installs it over execPath.
func runUpdate(ctx context.Context, checkOnly bool, execPath string) error {
	if err := checkReleaseRepo(); err != nil {
		return err
	}

	if !quiet {
		fmt.Printf("%s⋯%s Checking for updates...\n", colorDim, colorReset)
	}
	release, err := getLatestRelease(ctx)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentVersion := strings.TrimPrefix(version, "v")

	if latestVersion == currentVersion || util.CompareVersions(currentVersion, latestVersion) >= 0 {
		fmt.Printf("%s✓%s Already up to date (v%s)\n", colorGreen, colorReset, currentVersion)
		return nil
	}

	fmt.Printf("%s↑%s New version available: %sv%s%s → %sv%s%s\n",
		colorCyan, colorReset,
		colorDim, currentVersion, colorReset,
		colorGreen, latestVersion, colorReset)

	if checkOnly {
		fmt.Printf("\n%sRun 'stradl update' to install%s\n", colorDim, colorReset)
		return nil
	}

	newBinary, err := fetchReleaseBinary(ctx, release, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("refusing to install update: %w", err)
	}

	if !quiet {
		fmt.Printf("%s⋯%s Installing to %s...\n", colorDim, colorReset, execPath)
	}
	if err := replaceExecutable(execPath, newBinary); err != nil {
		return fmt.Errorf("failed to install update: %w", err)
	}

	fmt.Printf("%s✓%s Updated stradl v%s → v%s\n", colorGreen, colorReset, currentVersion, latestVersion)
	restartAgentAfterUpdate()
	return nil
}

// restartAgentAfterUpdate restarts the background agent, when one is
// installed, so it runs the new binary instead of the old one it still has
// open. If launchd refuses, it says how to restart the agent by hand.
func restartAgentAfterUpdate() {
	if !launchAgentInstalled() {
		return
	}
	p, err := agentPaths()
	if err == nil {
		err = newLaunchd(p, "").Restart()
	}
	if err != nil {
		printWarning("Could not restart the background agent, so it keeps running the old version: " + err.Error())
		fmt.Printf("  Restart it with: %slaunchctl kickstart -k gui/%d/%s%s\n", colorCyan, os.Getuid(), launchAgentLabel, colorReset)
		return
	}
	fmt.Printf("%s✓%s Restarted the background agent on the new version\n", colorGreen, colorReset)
}

// launchAgentInstalled reports whether the background agent's launchd plist
// exists for this user.
func launchAgentInstalled() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"))
	return err == nil
}

func updateCmd() *cobra.Command {
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update stradl to the latest release",
		Long: `Check GitHub Releases for a newer stradl and install it in place.

The release archive for this platform is verified against the release's
checksums.txt before the running binary is atomically replaced.

Examples:
  stradl update          # Update to the latest release
  stradl update --check  # Only check for updates, don't install`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkReleaseRepo(); err != nil {
				return err
			}
			execPath := ""
			if !checkOnly {
				var err error
				if execPath, err = currentExecutable(); err != nil {
					return err
				}
			}
			return runUpdate(context.Background(), checkOnly, execPath)
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "Only check for updates, don't install")

	return cmd
}

func changelogCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "changelog",
		Short: "Show release history and changelog",
		Long: `Display the release history of stradl with version notes.

Examples:
  stradl changelog           # Show recent releases
  stradl changelog --limit 5 # Show last 5 releases`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkReleaseRepo(); err != nil {
				return err
			}

			fmt.Printf("%s⋯%s Fetching releases...\n\n", colorDim, colorReset)

			releases, err := getAllReleases(context.Background(), limit)
			if err != nil {
				return fmt.Errorf("failed to fetch changelog: %w", err)
			}

			if len(releases) == 0 {
				fmt.Printf("%sNo releases found.%s\n", colorDim, colorReset)
				return nil
			}

			// Header
			fmt.Printf("%s╭─────────────────────────────────────────────────────────────╮%s\n", colorCyan, colorReset)
			fmt.Printf("%s│%s  %sSTRADL CHANGELOG%s                                           %s│%s\n", colorCyan, colorReset, colorBold, colorReset, colorCyan, colorReset)
			fmt.Printf("%s╰─────────────────────────────────────────────────────────────╯%s\n\n", colorCyan, colorReset)

			currentVersion := strings.TrimPrefix(version, "v")

			for i, release := range releases {
				tagVersion := strings.TrimPrefix(release.TagName, "v")

				// Version header with current marker
				if tagVersion == currentVersion {
					fmt.Printf("%s▸ %s%s %s(current)%s\n", colorGreen, release.TagName, colorReset, colorGreen, colorReset)
				} else {
					fmt.Printf("%s▸ %s%s\n", colorCyan, release.TagName, colorReset)
				}

				// Release date
				if release.PublishedAt != "" {
					if t, err := time.Parse(time.RFC3339, release.PublishedAt); err == nil {
						fmt.Printf("  %sReleased: %s%s\n", colorDim, t.Format("January 2, 2006"), colorReset)
					}
				}

				// Release notes (body)
				if release.Body != "" {
					fmt.Println()
					printReleaseBody(release.Body)
				}

				// Separator between releases
				if i < len(releases)-1 {
					fmt.Printf("\n%s───────────────────────────────────────────────────────────────%s\n\n", colorDim, colorReset)
				}
			}

			fmt.Println()
			fmt.Printf("%sView all releases: %s%s/releases%s\n", colorDim, colorCyan, projectURL(), colorReset)
			fmt.Println()

			return nil
		},
	}

	cmd.Flags().IntVarP(&limit, "limit", "n", 10, "Number of releases to show")

	return cmd
}

func printReleaseBody(body string) {
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Format markdown-style headers
		if strings.HasPrefix(line, "## ") {
			fmt.Printf("  %s%s%s\n", colorBold, strings.TrimPrefix(line, "## "), colorReset)
			continue
		}
		if strings.HasPrefix(line, "### ") {
			fmt.Printf("  %s%s%s\n", colorBold, strings.TrimPrefix(line, "### "), colorReset)
			continue
		}

		// Format bullet points
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			content := strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* ")
			fmt.Printf("  %s•%s %s\n", colorCyan, colorReset, content)
			continue
		}

		// Regular text
		fmt.Printf("  %s\n", line)
	}
}
