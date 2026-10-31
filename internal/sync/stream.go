package sync

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// Large files move through storage as streams on backends that support it
// (storage.Streaming): hashed while read, compressed and encrypted on their
// way into the upload, and downloaded to a temporary file beside the target,
// decrypted and checked against their key there before an atomic rename.
// Nothing holds them in memory, so there is no size limit. The blob format is
// the one small files use, age(gzip(header + content)), so either path reads
// what the other wrote.

// streamThreshold is the size above which files stream (a variable so tests
// can exercise the streaming path with small files).
var streamThreshold int64 = 32 << 20

// mergeMemoryLimit bounds transcripts and history merged line by line in
// memory. Above it, a file changed on two Macs keeps the newest copy and saves
// the other as a conflict copy.
var mergeMemoryLimit int64 = 512 << 20

// errTooLargeForMemory stops an in-memory read of a remote file above
// mergeMemoryLimit.
var errTooLargeForMemory = errors.New("too large to merge in memory")

// loadAgeIdentity reads the age identity from the key file, for streaming
// encryption. It returns nil when the key can't be read; files then never
// stream.
func loadAgeIdentity(path string) *age.X25519Identity {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	return id
}

// streamer returns the storage's streaming interface when files can stream.
func (s *Syncer) streamer() (storage.Streaming, bool) {
	st, ok := s.storage.(storage.Streaming)
	if !ok || s.ageID == nil {
		return nil, false
	}
	return st, true
}

// streams reports whether a file of this size at npath moves as a stream.
// Desktop records never do: they are small and rewritten on the way.
func (s *Syncer) streams(npath string, size int64) bool {
	_, ok := s.streamer()
	// The prompt history is filtered whole while some account doesn't sync.
	return ok && size > streamThreshold && !strings.HasPrefix(npath, DesktopPrefix) &&
		!(npath == "history.jsonl" && s.registry().anyOff())
}

// contentFilter returns the per-line path rewrite for npath on upload
// (normalize) or download (resolve), or nil for files stored as they are.
// Rewriting line by line matches rewriting the whole file: no path pattern
// spans a newline.
func (s *Syncer) contentFilter(npath string, upload bool) func([]byte) []byte {
	if s.paths == nil || strings.HasPrefix(npath, DesktopPrefix) || !IsPortableContentPath(npath) {
		return nil
	}
	if upload {
		return func(b []byte) []byte { return s.paths.NormalizeContent(npath, b) }
	}
	return func(b []byte) []byte { return s.paths.ResolveContent(npath, b) }
}

// copyLines copies src to dst a line at a time through fn.
func copyLines(dst io.Writer, src io.Reader, fn func([]byte) []byte) error {
	br := bufio.NewReaderSize(src, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(fn(line)); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// encodeStream writes the blob for key, with src's content, to w.
func (s *Syncer) encodeStream(w io.Writer, key string, src io.Reader, filter func([]byte) []byte) error {
	aw, err := age.Encrypt(w, s.ageID.Recipient())
	if err != nil {
		return fmt.Errorf("failed to encrypt: %w", err)
	}
	gz, err := gzip.NewWriterLevel(aw, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if _, err := gz.Write(bindKey(nil, key)); err != nil {
		return err
	}
	if filter != nil {
		err = copyLines(gz, src, filter)
	} else {
		_, err = io.Copy(gz, src)
	}
	if err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("failed to compress: %w", err)
	}
	if err := aw.Close(); err != nil {
		return fmt.Errorf("failed to encrypt: %w", err)
	}
	return nil
}

// decodeStream decrypts and decompresses a blob read from r and checks it was
// written for key, returning the content.
func (s *Syncer) decodeStream(r io.Reader, key string) (io.Reader, error) {
	ar, err := age.Decrypt(r, s.ageID)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}
	plain := bufio.NewReaderSize(ar, 64<<10)
	if magic, _ := plain.Peek(2); isGzipped(magic) {
		gz, err := gzip.NewReader(plain)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress: %w", err)
		}
		plain = bufio.NewReaderSize(gz, 64<<10)
	}
	if head, _ := plain.Peek(len(blobHeader)); string(head) == blobHeader {
		line, err := plain.ReadSlice('\n')
		if err != nil || string(line[len(blobHeader):len(line)-1]) != key {
			return nil, fmt.Errorf("%s: %w", key, ErrBlobMoved)
		}
	}
	return plain, nil
}

// putStream uploads src's content as npath's blob, conditionally on ifMatch
// ("" means create-only), without holding it in memory.
func (s *Syncer) putStream(ctx context.Context, npath string, src io.Reader, ifMatch string) (string, error) {
	st, ok := s.streamer()
	if !ok {
		return "", fmt.Errorf("storage can't stream")
	}
	key := s.engineRemoteKey(npath)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.encodeStream(pw, key, src, s.contentFilter(npath, true))
		_ = pw.CloseWithError(err) // nil: the reader sees EOF
		done <- err
	}()
	etag, err := st.UploadStream(ctx, key, pr, ifMatch)
	// Unblock the encoder if the upload stopped reading early.
	_ = pr.CloseWithError(errors.New("upload ended"))
	encErr := <-done
	if err != nil {
		return "", err
	}
	if encErr != nil {
		return "", encErr
	}
	return etag, nil
}

// uploadFileStream streams a local file to npath's blob. It reads at most
// info.Size() bytes, the size the caller recorded, and returns the new ETag
// and the hash of what it read.
func (s *Syncer) uploadFileStream(ctx context.Context, npath, abs string, info os.FileInfo, ifMatch string) (etag, hash string, err error) {
	if err := checkInICloud(abs, info); err != nil {
		return "", "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	etag, err = s.putStream(ctx, npath, io.TeeReader(io.LimitReader(f, info.Size()), h), ifMatch)
	if err != nil {
		return "", "", err
	}
	return etag, hex.EncodeToString(h.Sum(nil)), nil
}

// fetchStream reads a remote blob's content into memory through the
// streaming interface, up to mergeMemoryLimit.
func (s *Syncer) fetchStream(ctx context.Context, key string) ([]byte, string, error) {
	st, _ := s.streamer()
	rc, etag, err := st.DownloadStream(ctx, key)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rc.Close() }()
	plain, err := s.decodeStream(rc, key)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(io.LimitReader(plain, mergeMemoryLimit+1))
	if err != nil {
		return nil, "", fmt.Errorf("failed to read %s: %w", key, err)
	}
	if int64(len(data)) > mergeMemoryLimit {
		return nil, "", fmt.Errorf("%s: %w", key, errTooLargeForMemory)
	}
	return data, etag, nil
}

// fetchToTemp downloads npath's blob into a temporary file in dir (the
// target's folder, so the final rename is atomic), resolving paths for this
// device. It returns the temporary file, the hash and size of its content and
// the ETag of the version read. The caller removes the file.
func (s *Syncer) fetchToTemp(ctx context.Context, npath, key, dir string, perm os.FileMode) (tmpPath, hash string, size int64, etag string, err error) {
	st, ok := s.streamer()
	if !ok {
		return "", "", 0, "", fmt.Errorf("storage can't stream")
	}
	rc, etag, err := st.DownloadStream(ctx, key)
	if err != nil {
		return "", "", 0, "", err
	}
	defer func() { _ = rc.Close() }()
	plain, err := s.decodeStream(rc, key)
	if err != nil {
		return "", "", 0, "", err
	}
	if err := os.MkdirAll(dir, dirPermFor(perm)); err != nil {
		return "", "", 0, "", fmt.Errorf("failed to create directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".stradl-*.tmp")
	if err != nil {
		return "", "", 0, "", fmt.Errorf("failed to create temp file: %w", err)
	}
	fail := func(e error) (string, string, int64, string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", "", 0, "", e
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(tmp, h)}
	if filter := s.contentFilter(npath, false); filter != nil {
		err = copyLines(cw, plain, filter)
	} else {
		_, err = io.Copy(cw, plain)
	}
	if err != nil {
		return fail(fmt.Errorf("failed to download %s: %w", key, err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", "", 0, "", err
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), cw.n, etag, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// commitTemp renames a downloaded temporary file over abs with mode perm,
// unless abs changed since it was scanned (expect) or lies outside roots.
func commitTemp(tmp, abs string, expect os.FileInfo, perm os.FileMode, roots ...string) error {
	if err := checkInsideRoots(abs, roots...); err != nil {
		return err
	}
	// A link is compared as itself when that is how it was scanned.
	cur, err := os.Lstat(abs)
	if err == nil && isSymlink(cur) && !isSymlink(expect) {
		cur, err = os.Stat(abs)
	}
	if err == nil {
		if expect == nil || cur.Size() != expect.Size() || !cur.ModTime().Equal(expect.ModTime()) {
			return errLocalChanged // changed, or created here while the pass ran
		}
	} else if expect != nil {
		return errLocalChanged // deleted meanwhile
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if filepath.Dir(tmp) != filepath.Dir(abs) {
		return moveFile(tmp, abs)
	}
	return os.Rename(tmp, abs)
}
