package storage

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Provider represents a storage provider type
type Provider string

const (
	ProviderR2     Provider = "r2"
	ProviderS3     Provider = "s3"
	ProviderGCS    Provider = "gcs"
	ProviderWebDAV Provider = "webdav"
	ProviderAzure  Provider = "azure"
)

// ErrNotFound is returned (wrapped) when an object does not exist.
var ErrNotFound = errors.New("object not found")

// ErrPreconditionFailed is returned (wrapped) by Conditional.UploadIf when the
// remote object changed since the caller last saw it.
var ErrPreconditionFailed = errors.New("remote object changed (precondition failed)")

// MaxDownloadSize is the maximum allowed size for a single downloaded object (100MB).
// This prevents memory exhaustion from oversized or malicious remote files.
const MaxDownloadSize = 100 * 1024 * 1024

// ObjectInfo contains metadata about a stored object
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// Storage defines the interface for cloud storage operations
type Storage interface {
	// Upload stores data with the given key
	Upload(ctx context.Context, key string, data []byte) error

	// Download retrieves data for the given key
	Download(ctx context.Context, key string) ([]byte, error)

	// Delete removes the object with the given key
	Delete(ctx context.Context, key string) error

	// DeleteBatch removes multiple objects in a single operation
	DeleteBatch(ctx context.Context, keys []string) error

	// List returns all objects with the given prefix
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Head returns metadata for the given key without downloading content
	Head(ctx context.Context, key string) (*ObjectInfo, error)

	// BucketExists checks if the configured bucket exists
	BucketExists(ctx context.Context) (bool, error)
}

// Conditional is implemented by backends that support compare-and-swap
// writes. Several devices sync into one bucket, so an unconditional write can
// silently replace a version another device uploaded a moment earlier; with
// UploadIf the loser of that race gets ErrPreconditionFailed, merges, and
// retries instead.
type Conditional interface {
	// UploadIf stores data only if the object's current ETag equals ifMatch.
	// An empty ifMatch means the object must not exist yet. It returns the
	// new ETag, or an error wrapping ErrPreconditionFailed.
	UploadIf(ctx context.Context, key string, data []byte, ifMatch string) (string, error)

	// DownloadWithETag retrieves data together with the ETag of exactly the
	// version that was read, or an error wrapping ErrNotFound.
	DownloadWithETag(ctx context.Context, key string) ([]byte, string, error)
}

// New creates a new Storage instance based on the provided configuration
func New(cfg *StorageConfig) (Storage, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid storage config: %w", err)
	}

	switch cfg.Provider {
	case ProviderR2:
		return NewR2(cfg)
	case ProviderS3:
		return NewS3(cfg)
	case ProviderGCS:
		return NewGCS(cfg)
	case ProviderWebDAV:
		return NewWebDAV(cfg)
	case ProviderAzure:
		return NewAzure(cfg)
	default:
		return nil, fmt.Errorf("unsupported storage provider: %s", cfg.Provider)
	}
}

// NewR2 creates a new R2 storage adapter (implemented in r2/r2.go)
var NewR2 func(cfg *StorageConfig) (Storage, error)

// NewS3 creates a new S3 storage adapter (implemented in s3/s3.go)
var NewS3 func(cfg *StorageConfig) (Storage, error)

// NewGCS creates a new GCS storage adapter (implemented in gcs/gcs.go)
var NewGCS func(cfg *StorageConfig) (Storage, error)

// NewWebDAV creates a new WebDAV storage adapter (implemented in webdav/webdav.go)
var NewWebDAV func(cfg *StorageConfig) (Storage, error)

// NewAzure creates a new Azure Blob Storage adapter (implemented in azure/azure.go)
var NewAzure func(cfg *StorageConfig) (Storage, error)
