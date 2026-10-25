package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// Azurite's well-known development account (public, documented by Microsoft).
const (
	devAccount = "devstoreaccount1"
	devKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

// azuriteEndpoint returns the emulator endpoint, skipping the test when no
// emulator is listening. Override with AZURITE_BLOB_ENDPOINT.
func azuriteEndpoint(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("Azurite tests skipped in -short mode")
	}
	ep := os.Getenv("AZURITE_BLOB_ENDPOINT")
	if ep == "" {
		ep = "http://127.0.0.1:10000/" + devAccount
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:10000", 500*time.Millisecond)
	if err != nil && os.Getenv("AZURITE_BLOB_ENDPOINT") == "" {
		t.Skip("Azurite not running on 127.0.0.1:10000 (start it with: azurite-blob --loose)")
	}
	if conn != nil {
		_ = conn.Close()
	}
	return ep
}

// newContainer creates a fresh container on Azurite and returns a shared-key
// config for it.
func newContainer(t *testing.T) *storage.StorageConfig {
	t.Helper()
	ep := azuriteEndpoint(t)
	name := fmt.Sprintf("t%d", time.Now().UnixNano())
	cred, err := container.NewSharedKeyCredential(devAccount, devKey)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := container.NewClientWithSharedKeyCredential(ep+"/"+name, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Create(context.Background(), nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Delete(context.Background(), nil) })
	return &storage.StorageConfig{
		Provider:        storage.ProviderAzure,
		Bucket:          name,
		Endpoint:        ep,
		AzureAccount:    devAccount,
		AzureAccountKey: devKey,
	}
}

func mustNew(t *testing.T, cfg *storage.StorageConfig) *Client {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s.(*Client)
}

func TestNewRequiresCredential(t *testing.T) {
	_, err := New(&storage.StorageConfig{Provider: storage.ProviderAzure, Bucket: "claude-sync", AzureAccount: "acct"})
	if err == nil {
		t.Fatal("expected an error without SAS token or account key")
	}
}

func TestMapError(t *testing.T) {
	plain := errors.New("boom")
	if got := mapError(plain); got != plain {
		t.Fatalf("unrelated errors must pass through unchanged, got %v", got)
	}
	leaky := errors.New(`Get "https://acct.blob.core.windows.net/stradl?restype=container&sv=2024&sp=racwdl&sig=AbC%2Fsecret%3D&se=2027": dial tcp: lookup acct: no such host`)
	got := mapError(leaky)
	if strings.Contains(got.Error(), "secret") || !strings.Contains(got.Error(), "sig=REDACTED") {
		t.Fatalf("SAS signature not redacted: %v", got)
	}
	if !errors.Is(got, leaky) {
		t.Fatal("redacted error must still unwrap to the original")
	}
}

func TestRoundTripAndList(t *testing.T) {
	c := mustNew(t, newContainer(t))
	ctx := context.Background()

	files := map[string][]byte{
		"projects/-HOME-code-a/s1.jsonl.age": []byte("one"),
		"projects/-HOME-code-a/s2.jsonl.age": []byte("two"),
		"history.jsonl.age":                  []byte("hist"),
	}
	for k, v := range files {
		if err := c.Upload(ctx, k, v); err != nil {
			t.Fatalf("upload %s: %v", k, err)
		}
	}

	got, err := c.Download(ctx, "projects/-HOME-code-a/s2.jsonl.age")
	if err != nil || !bytes.Equal(got, []byte("two")) {
		t.Fatalf("download = %q, %v", got, err)
	}

	all, err := c.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("list returned %d objects, want 3", len(all))
	}
	for _, o := range all {
		if o.ETag == "" || o.LastModified.IsZero() || o.Size == 0 {
			t.Fatalf("incomplete object info: %+v", o)
		}
	}

	some, err := c.List(ctx, "projects/")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{some[0].Key, some[1].Key}
	sort.Strings(keys)
	if len(some) != 2 || keys[0] != "projects/-HOME-code-a/s1.jsonl.age" {
		t.Fatalf("prefix list = %v", keys)
	}

	head, err := c.Head(ctx, "history.jsonl.age")
	if err != nil || head.Size != 4 {
		t.Fatalf("head = %+v, %v", head, err)
	}

	if _, err := c.Download(ctx, "missing.age"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing blob error = %v, want ErrNotFound", err)
	}
	if _, err := c.Head(ctx, "missing.age"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing head error = %v, want ErrNotFound", err)
	}

	if err := c.DeleteBatch(ctx, []string{"history.jsonl.age", "projects/-HOME-code-a/s1.jsonl.age", "never-existed.age"}); err != nil {
		t.Fatalf("delete batch: %v", err)
	}
	left, _ := c.List(ctx, "")
	if len(left) != 1 {
		t.Fatalf("%d objects left after delete, want 1", len(left))
	}
}

func TestConditionalUpload(t *testing.T) {
	c := mustNew(t, newContainer(t))
	ctx := context.Background()
	const key = "projects/-HOME-x/s.jsonl.age"

	// Create-only succeeds once, then refuses to clobber.
	e1, err := c.UploadIf(ctx, key, []byte("v1"), "")
	if err != nil || e1 == "" {
		t.Fatalf("create: etag=%q err=%v", e1, err)
	}
	if _, err := c.UploadIf(ctx, key, []byte("v1-again"), ""); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("second create error = %v, want ErrPreconditionFailed", err)
	}

	// Device B reads v1 and its ETag.
	data, etag, err := c.DownloadWithETag(ctx, key)
	if err != nil || string(data) != "v1" || etag != e1 {
		t.Fatalf("download: %q %q %v (want etag %q)", data, etag, err, e1)
	}

	// Device A writes v2 on top of v1.
	e2, err := c.UploadIf(ctx, key, []byte("v2"), e1)
	if err != nil || e2 == e1 {
		t.Fatalf("update: etag=%q err=%v", e2, err)
	}

	// Device B's stale write loses the race instead of erasing v2.
	if _, err := c.UploadIf(ctx, key, []byte("b-stale"), etag); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("stale write error = %v, want ErrPreconditionFailed", err)
	}
	data, _ = c.Download(ctx, key)
	if string(data) != "v2" {
		t.Fatalf("content = %q, want v2", data)
	}
}

// Azure quotes ETags in headers but not in List results; both must agree or
// every sync would think every blob changed.
func TestETagsConsistentAcrossListAndWrites(t *testing.T) {
	c := mustNew(t, newContainer(t))
	ctx := context.Background()
	const key = "projects/-HOME-x/s.jsonl.age"

	etag, err := c.UploadIf(ctx, key, []byte("v1"), "")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.List(ctx, "")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %v %v", listed, err)
	}
	if listed[0].ETag != etag {
		t.Fatalf("list ETag %q != upload ETag %q", listed[0].ETag, etag)
	}
	_, dlETag, err := c.DownloadWithETag(ctx, key)
	if err != nil || dlETag != etag {
		t.Fatalf("download ETag %q != upload ETag %q (%v)", dlETag, etag, err)
	}
	head, err := c.Head(ctx, key)
	if err != nil || head.ETag != etag {
		t.Fatalf("head ETag %v != upload ETag %q (%v)", head, etag, err)
	}
	// A precondition built from the listed ETag must be honored.
	if _, err := c.UploadIf(ctx, key, []byte("v2"), listed[0].ETag); err != nil {
		t.Fatalf("conditional write with listed ETag: %v", err)
	}
}

func TestSASTokenAuth(t *testing.T) {
	cfg := newContainer(t)
	cred, err := container.NewSharedKeyCredential(devAccount, devKey)
	if err != nil {
		t.Fatal(err)
	}
	perms := sas.ContainerPermissions{Read: true, Add: true, Create: true, Write: true, Delete: true, List: true}
	qp, err := sas.BlobSignatureValues{
		Protocol:      sas.ProtocolHTTPSandHTTP,
		ExpiryTime:    time.Now().Add(time.Hour).UTC(),
		ContainerName: cfg.Bucket,
		Permissions:   perms.String(),
	}.SignWithSharedKey(cred)
	if err != nil {
		t.Fatal(err)
	}

	sasCfg := *cfg
	sasCfg.AzureAccountKey = ""
	sasCfg.AzureSASToken = "?" + qp.Encode()
	c := mustNew(t, &sasCfg)
	ctx := context.Background()

	ok, err := c.BucketExists(ctx)
	if err != nil || !ok {
		t.Fatalf("bucket exists with SAS = %v, %v", ok, err)
	}
	if _, err := c.UploadIf(ctx, "a.age", []byte("x"), ""); err != nil {
		t.Fatalf("SAS upload: %v", err)
	}
	if err := c.Delete(ctx, "a.age"); err != nil {
		t.Fatalf("SAS delete: %v", err)
	}

	// A container that does not exist reports false, not an error.
	missing := *cfg
	missing.Bucket = "does-not-exist-" + cfg.Bucket[1:8]
	ok, err = mustNew(t, &missing).BucketExists(ctx)
	if err != nil || ok {
		t.Fatalf("missing container = %v, %v; want false, nil", ok, err)
	}
}
