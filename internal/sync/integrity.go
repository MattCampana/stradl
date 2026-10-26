package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// blobHeader starts every file payload, ahead of the content, inside the
// encryption. It names the remote key the blob was written for. age does not
// bind a ciphertext to its object name, and the storage token alone (no
// passphrase) lets anyone rename or copy blobs, so without this a blob could
// be moved onto another file and decrypt fine there.
const blobHeader = "stradl-blob v1 "

// ErrBlobMoved means a blob decrypted fine but was written for another key.
var ErrBlobMoved = errors.New("remote file was written for a different path (moved or copied in storage); refusing it")

func bindKey(data []byte, key string) []byte {
	out := make([]byte, 0, len(blobHeader)+len(key)+1+len(data))
	out = append(out, blobHeader...)
	out = append(out, key...)
	out = append(out, '\n')
	return append(out, data...)
}

// unbindKey strips and checks the header. Blobs without one (written before
// the header existed) are accepted as they are.
func unbindKey(data []byte, key string) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte(blobHeader)) {
		return data, nil
	}
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return nil, fmt.Errorf("%s: %w", key, ErrBlobMoved)
	}
	if string(data[len(blobHeader):nl]) != key {
		return nil, fmt.Errorf("%s: %w", key, ErrBlobMoved)
	}
	return data[nl+1:], nil
}

// KeyCheckKey is a small encrypted marker that pins a container to one
// encryption key. Every sync checks it before touching any file, so a device
// still holding an old key (after another device re-keyed the container) stops
// instead of filling the container with files nobody else can read.
const KeyCheckKey = "_metadata/keycheck.age"

const keyCheckPlaintext = "stradl key check v1\n"

// ErrKeyMismatch means this device's key can't read the container's key-check
// marker: the passphrase differs from the one the container was set up with.
var ErrKeyMismatch = errors.New("this Mac's encryption key doesn't match the one your other Macs use (run 'stradl init --passphrase' with the passphrase they use)")

// checkKey verifies the marker against this device's key, creating it on the
// first sync into a container that has none.
func (s *Syncer) checkKey(ctx context.Context, objects []storage.ObjectInfo, dryRun bool) error {
	present := false
	for _, o := range objects {
		if o.Key == KeyCheckKey {
			present = true
			break
		}
	}
	if !present {
		if dryRun {
			return nil
		}
		err := WriteKeyCheck(ctx, s.storage, s.encryptor, true)
		if !errors.Is(err, storage.ErrPreconditionFailed) {
			return err // created (nil), or storage failed
		}
		// Another device created it meanwhile: verify theirs below.
	}
	return VerifyKeyCheck(ctx, s.storage, s.encryptor)
}

// VerifyKeyCheck returns ErrKeyMismatch when enc can't read the marker, a
// wrapped storage.ErrNotFound when there is no marker, or another error when
// storage can't be reached.
func VerifyKeyCheck(ctx context.Context, store storage.Storage, enc *crypto.Encryptor) error {
	data, err := store.Download(ctx, KeyCheckKey)
	if err != nil {
		return err
	}
	plain, err := enc.Decrypt(data)
	if err != nil || !strings.HasPrefix(string(plain), "stradl key check") {
		return ErrKeyMismatch
	}
	return nil
}

// WriteKeyCheck writes the marker for enc. With createOnly it never replaces
// an existing marker (backends without compare-and-swap write it as is).
func WriteKeyCheck(ctx context.Context, store storage.Storage, enc *crypto.Encryptor, createOnly bool) error {
	data, err := enc.Encrypt([]byte(keyCheckPlaintext))
	if err != nil {
		return err
	}
	if c, ok := store.(storage.Conditional); ok && createOnly {
		_, err := c.UploadIf(ctx, KeyCheckKey, data, "")
		return err
	}
	return store.Upload(ctx, KeyCheckKey, data)
}
