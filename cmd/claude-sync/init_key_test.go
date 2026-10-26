package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"testing"

	"github.com/AlecAivazis/survey/v2/terminal"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// fakeRemote is an in-memory container for the init and reset tests. It logs
// every write and delete in order and can be told to fail.
type fakeRemote struct {
	mu   gosync.Mutex
	objs map[string][]byte
	seq  int
	ops  []string // "upload <key>" and "delete <key>", in order

	listErr       error
	downloadErr   error
	uploadErr     error
	bucketMissing bool
	plainNotFound bool // report missing objects without storage.ErrNotFound, like S3 or WebDAV
}

func newFakeRemote() *fakeRemote { return &fakeRemote{objs: map[string][]byte{}} }

func (f *fakeRemote) put(key string, data []byte) {
	f.objs[key] = append([]byte(nil), data...)
	f.ops = append(f.ops, "upload "+key)
}

func (f *fakeRemote) Upload(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadErr != nil {
		return f.uploadErr
	}
	f.put(key, data)
	return nil
}

func (f *fakeRemote) UploadIf(_ context.Context, key string, data []byte, ifMatch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	if _, exists := f.objs[key]; exists != (ifMatch != "") {
		return "", storage.ErrPreconditionFailed
	}
	f.put(key, data)
	f.seq++
	return fmt.Sprint(f.seq), nil
}

func (f *fakeRemote) Download(ctx context.Context, key string) ([]byte, error) {
	data, _, err := f.DownloadWithETag(ctx, key)
	return data, err
}

func (f *fakeRemote) DownloadWithETag(_ context.Context, key string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.downloadErr != nil {
		return nil, "", f.downloadErr
	}
	data, ok := f.objs[key]
	if !ok {
		if f.plainNotFound {
			return nil, "", fmt.Errorf("NoSuchKey: %s", key)
		}
		return nil, "", fmt.Errorf("%s: %w", key, storage.ErrNotFound)
	}
	return append([]byte(nil), data...), "1", nil
}

func (f *fakeRemote) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, key)
	f.ops = append(f.ops, "delete "+key)
	return nil
}

func (f *fakeRemote) DeleteBatch(ctx context.Context, keys []string) error {
	for _, k := range keys {
		_ = f.Delete(ctx, k)
	}
	return nil
}

func (f *fakeRemote) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []storage.ObjectInfo
	for k, d := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.ObjectInfo{Key: k, Size: int64(len(d))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeRemote) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectInfo{Key: key, Size: int64(len(d))}, nil
}

func (f *fakeRemote) BucketExists(context.Context) (bool, error) { return !f.bucketMissing, nil }

func (f *fakeRemote) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ks []string
	for k := range f.objs {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// useFakeRemote makes init and reset open r instead of real storage.
func useFakeRemote(t *testing.T, r *fakeRemote) {
	t.Helper()
	old := newStorageClient
	newStorageClient = func(*storage.StorageConfig) (storage.Storage, error) { return r, nil }
	t.Cleanup(func() { newStorageClient = old })
}

// answerPassphrases makes the passphrase prompt return each of passphrases in
// turn, then Ctrl-C.
func answerPassphrases(t *testing.T, passphrases ...string) *int {
	t.Helper()
	asked := new(int)
	old := promptPassphrase
	promptPassphrase = func() (string, error) {
		*asked++
		if *asked > len(passphrases) {
			return "", terminal.InterruptErr
		}
		return passphrases[*asked-1], nil
	}
	t.Cleanup(func() { promptPassphrase = old })
	return asked
}

// answerKeyMismatch makes the key-mismatch menu return each of actions in
// turn; asking more often fails the test.
func answerKeyMismatch(t *testing.T, actions ...keyMismatchAction) *int {
	t.Helper()
	asked := new(int)
	old := promptKeyMismatch
	promptKeyMismatch = func() (keyMismatchAction, error) {
		*asked++
		if *asked > len(actions) {
			t.Errorf("key-mismatch menu shown %d times, want %d", *asked, len(actions))
			return actionAbort, nil
		}
		return actions[*asked-1], nil
	}
	t.Cleanup(func() { promptKeyMismatch = old })
	return asked
}

var (
	keyCacheMu gosync.Mutex
	keyCache   = map[string][]byte{}
)

// passphraseKey returns the key file content a passphrase derives (cached:
// Argon2id is deliberately slow).
func passphraseKey(t *testing.T, passphrase string) []byte {
	t.Helper()
	keyCacheMu.Lock()
	defer keyCacheMu.Unlock()
	if k, ok := keyCache[passphrase]; ok {
		return k
	}
	p := filepath.Join(t.TempDir(), "key")
	if err := crypto.GenerateKeyFromPassphrase(p, passphrase); err != nil {
		t.Fatal(err)
	}
	k, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	keyCache[passphrase] = k
	return k
}

// encryptorFor builds an encryptor from key file content.
func encryptorFor(t *testing.T, key []byte) *crypto.Encryptor {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, key, 0600); err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.NewEncryptor(p)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// seedRemote fills r like a container another Mac synced with key: the
// key-check marker (unless legacy) and two session files.
func seedRemote(t *testing.T, r *fakeRemote, key []byte, legacy bool) {
	t.Helper()
	enc := encryptorFor(t, key)
	if !legacy {
		if err := sync.WriteKeyCheck(context.Background(), r, enc, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"projects/${HOME}-app/one.jsonl.age", "projects/${HOME}-app/two.jsonl.age"} {
		data, err := enc.Encrypt([]byte(`{"type":"user"}` + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Upload(context.Background(), k, data); err != nil {
			t.Fatal(err)
		}
	}
	r.ops = nil
}

const (
	goodPassphrase  = "correct horse battery staple"
	typoPassphrase  = "correct horse battery stapel"
	otherPassphrase = "a completely different passphrase"
)

// passphraseHome sets up a temp HOME holding a working setup: a config for
// (fake) Azure and the live key derived from goodPassphrase. It returns the
// key path.
func passphraseHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &config.Config{
		Storage: &storage.StorageConfig{
			Provider:      storage.ProviderAzure,
			Bucket:        "stradl",
			AzureAccount:  "fakeaccount",
			AzureSASToken: testSAS,
		},
		EncryptionKey: "~/" + config.ConfigDir + "/" + config.AgeKeyFile,
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	keyPath := config.AgeKeyFilePath()
	if err := os.WriteFile(keyPath, passphraseKey(t, goodPassphrase), 0600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

func assertKey(t *testing.T, keyPath string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("live key: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("live key changed")
	}
	if _, err := os.Stat(keyPath + ".new"); !os.IsNotExist(err) {
		t.Errorf("staged key left behind: %v", err)
	}
}

func TestPassphraseTypoLeavesLiveKeyUntouched(t *testing.T) {
	ctx := context.Background()
	good := passphraseKey(t, goodPassphrase)

	for _, legacy := range []bool{false, true} {
		name := "key-check marker"
		if legacy {
			name = "legacy container without a marker"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("abort", func(t *testing.T) {
				keyPath := passphraseHome(t)
				remote := newFakeRemote()
				seedRemote(t, remote, good, legacy)
				useFakeRemote(t, remote)
				answerPassphrases(t, typoPassphrase)
				answerKeyMismatch(t, actionAbort)

				if err := initPassphraseOnly(ctx, keyPath); err == nil || !strings.Contains(err.Error(), "not changed") {
					t.Fatalf("initPassphraseOnly() = %v, want an abort that keeps the key", err)
				}
				assertKey(t, keyPath, good)
				if len(remote.ops) != 0 {
					t.Errorf("remote changed: %v", remote.ops)
				}
			})

			t.Run("ctrl-c at the retry", func(t *testing.T) {
				keyPath := passphraseHome(t)
				remote := newFakeRemote()
				seedRemote(t, remote, good, legacy)
				useFakeRemote(t, remote)
				answerPassphrases(t, typoPassphrase) // then Ctrl-C
				answerKeyMismatch(t, actionRetryPassphrase)

				if err := initPassphraseOnly(ctx, keyPath); !errors.Is(err, terminal.InterruptErr) {
					t.Fatalf("initPassphraseOnly() = %v, want the interrupt", err)
				}
				assertKey(t, keyPath, good)
			})

			t.Run("retry with the right passphrase", func(t *testing.T) {
				keyPath := passphraseHome(t)
				remote := newFakeRemote()
				seedRemote(t, remote, good, legacy)
				useFakeRemote(t, remote)
				asked := answerPassphrases(t, typoPassphrase, goodPassphrase)
				answerKeyMismatch(t, actionRetryPassphrase)

				if err := initPassphraseOnly(ctx, keyPath); err != nil {
					t.Fatalf("initPassphraseOnly() = %v", err)
				}
				if *asked != 2 {
					t.Errorf("asked for the passphrase %d times, want 2", *asked)
				}
				assertKey(t, keyPath, good)
				if len(remote.ops) != 0 {
					t.Errorf("remote changed: %v", remote.ops)
				}
			})
		})
	}
}

func TestPassphraseCheckFailsClosedOnStorageErrors(t *testing.T) {
	ctx := context.Background()
	keyPath := passphraseHome(t)
	goodKey := passphraseKey(t, goodPassphrase)

	for _, tc := range []struct {
		name string
		fail func(r *fakeRemote)
	}{
		{"list fails", func(r *fakeRemote) { r.listErr = errors.New("dial tcp: no such host (sig=SECRETSIG)") }},
		{"download fails", func(r *fakeRemote) { r.downloadErr = errors.New("connection reset") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := newFakeRemote()
			seedRemote(t, remote, goodKey, true)
			tc.fail(remote)
			useFakeRemote(t, remote)
			answerPassphrases(t, otherPassphrase)
			menu := answerKeyMismatch(t) // must not be shown

			err := initPassphraseOnly(ctx, keyPath)
			if err == nil || !strings.Contains(err.Error(), "could not read remote storage") {
				t.Fatalf("initPassphraseOnly() = %v, want a connectivity error", err)
			}
			if strings.Contains(err.Error(), "SECRETSIG") {
				t.Errorf("error leaks the SAS signature: %v", err)
			}
			if *menu != 0 {
				t.Errorf("a storage failure was treated as a key mismatch")
			}
			assertKey(t, keyPath, goodKey)
		})
	}
}

func TestVerifyKeyMatchesRemote(t *testing.T) {
	ctx := context.Background()
	goodKey := passphraseKey(t, goodPassphrase)
	otherKey := passphraseKey(t, otherPassphrase)
	keyPath := filepath.Join(t.TempDir(), "age-key.txt")
	if err := os.WriteFile(keyPath, goodKey, 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		setup    func(r *fakeRemote)
		match    bool // nil error
		mismatch bool // sync.ErrKeyMismatch
	}{
		{"empty container", func(r *fakeRemote) {}, true, false},
		{"marker for this key", func(r *fakeRemote) { seedRemote(t, r, goodKey, false) }, true, false},
		{"marker for another key", func(r *fakeRemote) { seedRemote(t, r, otherKey, false) }, false, true},
		{"legacy files for this key", func(r *fakeRemote) { seedRemote(t, r, goodKey, true) }, true, false},
		{"legacy files for another key", func(r *fakeRemote) { seedRemote(t, r, otherKey, true) }, false, true},
		{"backend without ErrNotFound, legacy files for this key", func(r *fakeRemote) {
			seedRemote(t, r, goodKey, true)
			r.plainNotFound = true
		}, true, false},
		{"backend without ErrNotFound, legacy files for another key", func(r *fakeRemote) {
			seedRemote(t, r, otherKey, true)
			r.plainNotFound = true
		}, false, true},
		{"list fails on an empty container", func(r *fakeRemote) { r.listErr = errors.New("403 AuthorizationFailure") }, false, false},
		{"list fails", func(r *fakeRemote) {
			seedRemote(t, r, otherKey, true)
			r.listErr = errors.New("403 AuthorizationFailure")
		}, false, false},
		{"download fails", func(r *fakeRemote) {
			seedRemote(t, r, otherKey, false)
			r.downloadErr = errors.New("connection reset")
		}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeRemote()
			tt.setup(r)
			err := verifyKeyMatchesRemote(ctx, r, keyPath)
			switch {
			case tt.match && err != nil:
				t.Errorf("verifyKeyMatchesRemote() = %v, want a match", err)
			case tt.mismatch && !errors.Is(err, sync.ErrKeyMismatch):
				t.Errorf("verifyKeyMatchesRemote() = %v, want a key mismatch", err)
			case !tt.match && !tt.mismatch && (err == nil || errors.Is(err, sync.ErrKeyMismatch)):
				t.Errorf("verifyKeyMatchesRemote() = %v, want a storage error", err)
			}
		})
	}
}

func TestClearRemoteWritesMarkerBeforeDeletes(t *testing.T) {
	ctx := context.Background()
	remote := newFakeRemote()
	seedRemote(t, remote, passphraseKey(t, otherPassphrase), false)
	newEnc := encryptorFor(t, passphraseKey(t, goodPassphrase))

	rekeyed, err := clearRemoteStorage(ctx, remote, newEnc)
	if err != nil || !rekeyed {
		t.Fatalf("clearRemoteStorage() = %v, %v", rekeyed, err)
	}
	if len(remote.ops) == 0 || remote.ops[0] != "upload "+sync.KeyCheckKey {
		t.Fatalf("first operation = %v, want the key-check marker upload", remote.ops)
	}
	for _, op := range remote.ops[1:] {
		if !strings.HasPrefix(op, "delete ") || op == "delete "+sync.KeyCheckKey {
			t.Errorf("unexpected operation after the marker: %q", op)
		}
	}
	if got := remote.keys(); len(got) != 1 || got[0] != sync.KeyCheckKey {
		t.Errorf("remote keys = %v, want only the marker", got)
	}
	if err := sync.VerifyKeyCheck(ctx, remote, newEnc); err != nil {
		t.Errorf("marker doesn't match the new key: %v", err)
	}

	t.Run("marker write fails: nothing is deleted", func(t *testing.T) {
		remote := newFakeRemote()
		seedRemote(t, remote, passphraseKey(t, otherPassphrase), false)
		remote.uploadErr = errors.New("403 AuthorizationPermissionMismatch")
		rekeyed, err := clearRemoteStorage(ctx, remote, newEnc)
		if err == nil || rekeyed {
			t.Fatalf("clearRemoteStorage() = %v, %v, want a failure", rekeyed, err)
		}
		if len(remote.ops) != 0 {
			t.Errorf("operations after a failed marker write: %v", remote.ops)
		}
	})
}

func TestPassphraseClearRemoteRekeysContainer(t *testing.T) {
	ctx := context.Background()
	keyPath := passphraseHome(t)
	remote := newFakeRemote()
	seedRemote(t, remote, passphraseKey(t, goodPassphrase), false)
	useFakeRemote(t, remote)
	answerPassphrases(t, otherPassphrase)
	answerKeyMismatch(t, actionClearRemote)

	if err := initPassphraseOnly(ctx, keyPath); err != nil {
		t.Fatalf("initPassphraseOnly() = %v", err)
	}
	newKey := passphraseKey(t, otherPassphrase)
	assertKey(t, keyPath, newKey)
	if remote.ops[0] != "upload "+sync.KeyCheckKey {
		t.Errorf("operations = %v, want the marker written first", remote.ops)
	}
	if err := sync.VerifyKeyCheck(ctx, remote, encryptorFor(t, newKey)); err != nil {
		t.Errorf("container not re-keyed: %v", err)
	}
	if got := remote.keys(); len(got) != 1 {
		t.Errorf("remote keys = %v, want only the marker", got)
	}

	t.Run("marker write fails: key unchanged", func(t *testing.T) {
		keyPath := passphraseHome(t)
		remote := newFakeRemote()
		seedRemote(t, remote, passphraseKey(t, goodPassphrase), false)
		remote.uploadErr = errors.New("403")
		useFakeRemote(t, remote)
		answerPassphrases(t, otherPassphrase)
		answerKeyMismatch(t, actionClearRemote)

		if err := initPassphraseOnly(ctx, keyPath); err == nil {
			t.Fatal("initPassphraseOnly() succeeded without re-keying the container")
		}
		assertKey(t, keyPath, passphraseKey(t, goodPassphrase))
		if got := remote.keys(); len(got) != 3 {
			t.Errorf("remote keys = %v, want all three kept", got)
		}
	})
}

// fullSetupOpts is a scripted Azure init that needs no prompts.
func fullSetupOpts() initOptions {
	return initOptions{force: true, azure: azureOptions{Account: "mystradl", SASToken: testSAS}}
}

func TestInitKeepsOtherSettings(t *testing.T) {
	ctx := context.Background()
	t.Setenv(envAzureSASToken, "")
	t.Setenv(envAzureAccountKey, "")
	on, off := true, false

	setup := func(t *testing.T, cfg *config.Config) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
		// An existing key, so init asks nothing.
		if err := crypto.GenerateKey(config.AgeKeyFilePath()); err != nil {
			t.Fatal(err)
		}
		useFakeRemote(t, newFakeRemote())
	}
	load := func(t *testing.T) *config.Config {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("modern config", func(t *testing.T) {
		setup(t, &config.Config{
			Storage:             &storage.StorageConfig{Provider: storage.ProviderS3, Bucket: "old", AccessKeyID: "AK", SecretAccessKey: "SK", Region: "us-east-1"},
			EncryptionKey:       "~/elsewhere/key.txt",
			Exclude:             []string{"projects/**/scratch/**"},
			Scope:               config.ScopeFull,
			SyncPaths:           []string{"projects", "plans", "skills"},
			MCPSync:             &on,
			PathMap:             map[string]string{"~/work": "WORK"},
			DesktopSync:         &off,
			SyncIntervalSeconds: 120,
		})
		if err := initFullSetup(ctx, config.AgeKeyFilePath(), fullSetupOpts()); err != nil {
			t.Fatalf("initFullSetup() = %v", err)
		}
		got := load(t)
		home, _ := os.UserHomeDir()
		if got.Storage.Provider != storage.ProviderAzure || got.Storage.AzureAccount != "mystradl" || got.Storage.AccessKeyID != "" {
			t.Errorf("storage not replaced: %+v", got.Storage)
		}
		if got.EncryptionKey != config.AgeKeyFilePath() {
			t.Errorf("encryption key path = %q", got.EncryptionKey)
		}
		if got.Scope != config.ScopeFull {
			t.Errorf("scope = %q, want full kept", got.Scope)
		}
		if len(got.Exclude) != 1 || len(got.SyncPaths) != 3 || !got.IsMCPSyncEnabled() ||
			got.PathMap[filepath.Join(home, "work")] != "WORK" || got.DesktopSync == nil || *got.DesktopSync ||
			got.SyncIntervalSeconds != 120 {
			t.Errorf("settings dropped: %+v", got)
		}
	})

	t.Run("--scope replaces the scope", func(t *testing.T) {
		setup(t, &config.Config{Scope: config.ScopeFull, EncryptionKey: "~/.stradl/age-key.txt", SyncIntervalSeconds: 90})
		opts := fullSetupOpts()
		opts.scope = config.ScopeSessions
		if err := initFullSetup(ctx, config.AgeKeyFilePath(), opts); err != nil {
			t.Fatalf("initFullSetup() = %v", err)
		}
		if got := load(t); got.Scope != config.ScopeSessions || got.SyncIntervalSeconds != 90 {
			t.Errorf("scope = %q, interval = %d", got.Scope, got.SyncIntervalSeconds)
		}
	})

	t.Run("legacy R2 fields are cleared", func(t *testing.T) {
		setup(t, &config.Config{
			AccountID: "acct", AccessKeyID: "AK", SecretAccessKey: "SK", Bucket: "legacy",
			EncryptionKey: "~/.stradl/age-key.txt",
			PathMap:       map[string]string{"~/src": "SRC"},
		})
		if err := initFullSetup(ctx, config.AgeKeyFilePath(), fullSetupOpts()); err != nil {
			t.Fatalf("initFullSetup() = %v", err)
		}
		got := load(t)
		if got.AccountID != "" || got.AccessKeyID != "" || got.SecretAccessKey != "" || got.Bucket != "" || got.Endpoint != "" {
			t.Errorf("legacy fields kept: %+v", got)
		}
		if got.IsLegacyConfig() || got.GetStorageConfig().Provider != storage.ProviderAzure {
			t.Errorf("config still reads as legacy R2: %+v", got.GetStorageConfig())
		}
		if got.Scope != config.ScopeSessions || len(got.PathMap) != 1 {
			t.Errorf("scope = %q, path_map = %v", got.Scope, got.PathMap)
		}
	})
}

func TestFailedInitLeavesNoKey(t *testing.T) {
	ctx := context.Background()
	t.Setenv(envAzureSASToken, "")
	t.Setenv(envAzureAccountKey, "")

	noKey := func(t *testing.T) {
		t.Helper()
		for _, p := range []string{config.AgeKeyFilePath(), config.AgeKeyFilePath() + ".new"} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s left behind (%v)", filepath.Base(p), err)
			}
		}
		if config.Exists() {
			t.Error("config saved by a failed init")
		}
	}

	t.Run("container missing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		useFakeRemote(t, &fakeRemote{objs: map[string][]byte{}, bucketMissing: true})
		asked := answerPassphrases(t, goodPassphrase)
		opts := fullSetupOpts()
		opts.usePassphrase = true
		if err := initFullSetup(ctx, config.AgeKeyFilePath(), opts); err == nil {
			t.Fatal("initFullSetup() succeeded against a missing container")
		}
		if *asked != 0 {
			t.Error("asked for a passphrase before the connection test")
		}
		noKey(t)
	})

	t.Run("passphrase rejected", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		remote := newFakeRemote()
		seedRemote(t, remote, passphraseKey(t, goodPassphrase), false)
		useFakeRemote(t, remote)
		answerPassphrases(t, typoPassphrase)
		answerKeyMismatch(t, actionAbort)
		opts := fullSetupOpts()
		opts.usePassphrase = true
		if err := initFullSetup(ctx, config.AgeKeyFilePath(), opts); err == nil {
			t.Fatal("initFullSetup() succeeded with the wrong passphrase")
		}
		noKey(t)
	})

	t.Run("random key against a container with files", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if err := os.MkdirAll(config.ConfigDirPath(), 0700); err != nil {
			t.Fatal(err)
		}
		remote := newFakeRemote()
		seedRemote(t, remote, passphraseKey(t, goodPassphrase), false)
		if err := generateRandomKey(ctx, remote, config.AgeKeyFilePath()); err == nil {
			t.Fatal("generateRandomKey() accepted a container another key owns")
		}
		noKey(t)
	})
}

func TestResetRemoteDeletesMarkerLast(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(&config.Config{
		Storage:       &storage.StorageConfig{Provider: storage.ProviderAzure, Bucket: "stradl", AzureAccount: "fakeaccount", AzureSASToken: testSAS},
		EncryptionKey: "~/.stradl/age-key.txt",
	}); err != nil {
		t.Fatal(err)
	}
	remote := newFakeRemote()
	seedRemote(t, remote, passphraseKey(t, goodPassphrase), false)
	// Sorted listing puts the marker (_metadata/) before projects/.
	useFakeRemote(t, remote)

	if err := execute(resetCmd(), "--remote", "--force"); err != nil {
		t.Fatalf("reset --remote: %v", err)
	}
	if len(remote.ops) != 3 || remote.ops[len(remote.ops)-1] != "delete "+sync.KeyCheckKey {
		t.Errorf("operations = %v, want the marker deleted last", remote.ops)
	}
	if len(remote.keys()) != 0 {
		t.Errorf("remote keys left: %v", remote.keys())
	}
	if _, err := os.Stat(filepath.Join(home, config.ConfigDir)); !os.IsNotExist(err) {
		t.Errorf("config dir not removed: %v", err)
	}
}
