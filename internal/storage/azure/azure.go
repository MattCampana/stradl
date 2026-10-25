package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/tawanorg/claude-sync/internal/storage"
)

func init() {
	storage.NewAzure = New
}

// deleteWorkers bounds concurrent deletes in DeleteBatch. The Blob Batch API
// would save round trips, but it rejects container-scoped SAS tokens, which
// are the recommended way to authenticate.
const deleteWorkers = 8

// Client implements storage.Storage and storage.Conditional for Azure Blob Storage
type Client struct {
	container *container.Client
	name      string
}

// New creates a new Azure Blob Storage client
func New(cfg *storage.StorageConfig) (storage.Storage, error) {
	endpoint := strings.TrimSuffix(storage.NormalizeEndpoint(cfg.GetEndpoint()), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("azure: no endpoint (set azure_account)")
	}
	containerURL := endpoint + "/" + cfg.Bucket

	var (
		client *container.Client
		err    error
	)
	switch {
	case cfg.AzureSASToken != "":
		client, err = container.NewClientWithNoCredential(containerURL+"?"+strings.TrimPrefix(cfg.AzureSASToken, "?"), nil)
	case cfg.AzureAccountKey != "":
		account := cfg.AzureAccount
		if account == "" {
			return nil, fmt.Errorf("azure: azure_account is required with an account key")
		}
		var cred *container.SharedKeyCredential
		cred, err = container.NewSharedKeyCredential(account, cfg.AzureAccountKey)
		if err != nil {
			return nil, fmt.Errorf("azure: invalid account key: %w", err)
		}
		client, err = container.NewClientWithSharedKeyCredential(containerURL, cred, nil)
	default:
		return nil, fmt.Errorf("azure: azure_sas_token or azure_account_key is required")
	}
	if err != nil {
		return nil, fmt.Errorf("azure: failed to create client: %w", err)
	}

	return &Client{container: client, name: cfg.Bucket}, nil
}

// Upload stores data with the given key
func (c *Client) Upload(ctx context.Context, key string, data []byte) error {
	_, err := c.put(ctx, key, data, nil)
	return err
}

// UploadIf stores data only if the blob's current ETag equals ifMatch, or, when
// ifMatch is empty, only if the blob does not exist yet.
func (c *Client) UploadIf(ctx context.Context, key string, data []byte, ifMatch string) (string, error) {
	cond := &blob.ModifiedAccessConditions{}
	if ifMatch == "" {
		cond.IfNoneMatch = to.Ptr(azcore.ETagAny)
	} else {
		cond.IfMatch = to.Ptr(azcore.ETag(ifMatch))
	}
	return c.put(ctx, key, data, &blob.AccessConditions{ModifiedAccessConditions: cond})
}

func (c *Client) put(ctx context.Context, key string, data []byte, cond *blob.AccessConditions) (string, error) {
	resp, err := c.container.NewBlockBlobClient(key).Upload(ctx, streaming.NopCloser(bytes.NewReader(data)), &blockblob.UploadOptions{
		HTTPHeaders:      &blob.HTTPHeaders{BlobContentType: to.Ptr("application/octet-stream")},
		AccessConditions: cond,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload %s: %w", key, mapError(err))
	}
	return etagString(resp.ETag), nil
}

// Download retrieves data for the given key
func (c *Client) Download(ctx context.Context, key string) ([]byte, error) {
	data, _, err := c.DownloadWithETag(ctx, key)
	return data, err
}

// DownloadWithETag retrieves data and the ETag of the version that was read
func (c *Client) DownloadWithETag(ctx context.Context, key string) ([]byte, string, error) {
	resp, err := c.container.NewBlobClient(key).DownloadStream(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to download %s: %w", key, mapError(err))
	}
	if resp.ContentLength != nil && *resp.ContentLength > storage.MaxDownloadSize {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("file %s exceeds maximum download size of %d bytes", key, storage.MaxDownloadSize)
	}
	// Pin retries to the version we started reading: a concurrent overwrite
	// fails the read instead of splicing two versions together.
	body := resp.NewRetryReader(ctx, &blob.RetryReaderOptions{MaxRetries: 3})
	defer func() { _ = body.Close() }()

	// Limit download size to prevent memory exhaustion
	limited := io.LimitReader(body, storage.MaxDownloadSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read %s: %w", key, mapError(err))
	}
	if int64(len(data)) > storage.MaxDownloadSize {
		return nil, "", fmt.Errorf("file %s exceeds maximum download size of %d bytes", key, storage.MaxDownloadSize)
	}
	return data, etagString(resp.ETag), nil
}

// Delete removes the object with the given key. Deleting a blob that does not
// exist is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.container.NewBlobClient(key).Delete(ctx, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return fmt.Errorf("failed to delete %s: %w", key, mapError(err))
	}
	return nil
}

// DeleteBatch removes multiple objects concurrently
func (c *Client) DeleteBatch(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	sem := make(chan struct{}, deleteWorkers)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, key := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := c.Delete(ctx, key); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(key)
	}
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("failed to delete %d of %d blobs: %w", len(errs), len(keys), errors.Join(errs...))
	}
	return nil
}

// List returns all objects with the given prefix
func (c *Client) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	var opts *container.ListBlobsFlatOptions
	if prefix != "" {
		opts = &container.ListBlobsFlatOptions{Prefix: to.Ptr(prefix)}
	}

	var objects []storage.ObjectInfo
	pager := c.container.NewListBlobsFlatPager(opts)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", mapError(err))
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			info := storage.ObjectInfo{Key: *item.Name}
			if p := item.Properties; p != nil {
				if p.ContentLength != nil {
					info.Size = *p.ContentLength
				}
				if p.LastModified != nil {
					info.LastModified = *p.LastModified
				}
				info.ETag = etagString(p.ETag)
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

// Head returns metadata for the given key without downloading content
func (c *Client) Head(ctx context.Context, key string) (*storage.ObjectInfo, error) {
	props, err := c.container.NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", key, mapError(err))
	}
	info := &storage.ObjectInfo{Key: key, ETag: etagString(props.ETag)}
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	}
	if props.LastModified != nil {
		info.LastModified = *props.LastModified
	}
	return info, nil
}

// BucketExists checks if the configured container exists. It lists at most
// one blob rather than reading container properties, because a container SAS
// grants List but not Get Container Properties.
func (c *Client) BucketExists(ctx context.Context) (bool, error) {
	pager := c.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{MaxResults: to.Ptr[int32](1)})
	if _, err := pager.NextPage(ctx); err != nil {
		if bloberror.HasCode(err, bloberror.ContainerNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check container %s: %w", c.name, mapError(err))
	}
	return true, nil
}

func etagString(e *azcore.ETag) string {
	if e == nil {
		return ""
	}
	return string(*e)
}

// mapError translates Azure errors into the storage sentinels and adds a hint
// for the failures a user can fix (an expired or under-privileged SAS token).
func mapError(err error) error {
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound):
		return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
	case bloberror.HasCode(err, bloberror.ConditionNotMet):
		return fmt.Errorf("%w: %w", storage.ErrPreconditionFailed, err)
	case bloberror.HasCode(err, bloberror.AuthenticationFailed):
		return fmt.Errorf("Azure rejected the credentials; the SAS token may have expired (extend its stored access policy in the portal, or rerun init with a new token): %w", err)
	case bloberror.HasCode(err, bloberror.AuthorizationPermissionMismatch, bloberror.AuthorizationFailure):
		return fmt.Errorf("the SAS token lacks a required permission (it needs read, add, create, write, delete and list): %w", err)
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
		case http.StatusPreconditionFailed, http.StatusConflict:
			return fmt.Errorf("%w: %w", storage.ErrPreconditionFailed, err)
		}
	}
	return err
}
