// Package blobcache is a persistent local blob pool filled by resumable downloads.
//
// Layout under the cache directory:
//
//	blobs/<alg>/<hex>     complete, verified blobs
//	partial/<alg>/<hex>   a byte prefix of a blob being fetched
//	tmp/                  scratch files for Put
//
// Blobs are content-addressed, so a partial file is a prefix of the blob regardless of
// which source it came from; resuming needs no validator other than the final digest check.
package blobcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/ocidir"
)

var ErrMismatch = errors.New("blobcache: blob does not match its descriptor")

const (
	blobsDir   = "blobs"
	partialDir = "partial"
	tmpDir     = "tmp"
	bufSize    = 1 << 20
)

// Cache may be shared by several processes: fetches of one digest are serialized by an in-process
// mutex and, across processes, by an exclusive lock on the partial file (on unix).
type Cache struct {
	root  *os.Root
	locks sync.Map // digest.Digest -> *sync.Mutex
}

func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{blobsDir, partialDir, tmpDir} {
		if err := root.MkdirAll(d, 0o755); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return &Cache{root: root}, nil
}

func (c *Cache) Close() error {
	return c.root.Close()
}

func entry(dir string, dgst digest.Digest) string {
	return path.Join(dir, dgst.Algorithm().String(), dgst.Encoded())
}

func (c *Cache) Has(dgst digest.Digest) bool {
	if dgst.Validate() != nil {
		return false
	}
	_, err := c.root.Stat(entry(blobsDir, dgst))
	return err == nil
}

type file struct {
	*os.File
	size int64
}

func (f file) Size() int64 {
	return f.size
}

func (c *Cache) Open(dgst digest.Digest) (ocidir.ReadAtCloser, error) {
	if err := dgst.Validate(); err != nil {
		return nil, err
	}
	f, err := c.root.Open(entry(blobsDir, dgst))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return file{File: f, size: info.Size()}, nil
}

func (c *Cache) lock(dgst digest.Digest) func() {
	v, _ := c.locks.LoadOrStore(dgst, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Fetch copies desc from src into the cache, resuming a partial file from its size;
// it verifies size and digest and discards the partial file on mismatch.
// progress, when non-nil, receives the number of bytes of the blob present so far.
func (c *Cache) Fetch(
	ctx context.Context,
	desc ocispec.Descriptor,
	src ocidir.ReadAtCloser,
	progress func(done int64),
) error {
	if err := desc.Digest.Validate(); err != nil {
		return err
	}
	if desc.Size < 0 {
		return fmt.Errorf("blobcache: %s: negative size %d", desc.Digest, desc.Size)
	}
	defer c.lock(desc.Digest)()

	if c.Has(desc.Digest) {
		if progress != nil {
			progress(desc.Size)
		}
		return nil
	}

	resumed, err := c.fetch(ctx, desc, src, progress)
	if errors.Is(err, ErrMismatch) && resumed {
		// The reused prefix may be what is corrupt; one fresh attempt tells them apart.
		_, err = c.fetch(ctx, desc, src, progress)
	}
	return err
}

// fetch reports whether it reused a non-empty partial prefix.
func (c *Cache) fetch(
	ctx context.Context,
	desc ocispec.Descriptor,
	src ocidir.ReadAtCloser,
	progress func(done int64),
) (resumed bool, err error) {
	name := entry(partialDir, desc.Digest)
	if err := c.root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return false, err
	}
	f, err := c.openLocked(name)
	if err != nil {
		return false, err
	}
	// Closing releases the lock, so every path below finishes with the partial file
	// (committed or removed) before this runs.
	defer func() { _ = f.Close() }()

	if c.Has(desc.Digest) {
		// Another process completed the blob while this one waited for the lock.
		if progress != nil {
			progress(desc.Size)
		}
		return false, nil
	}

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	done := info.Size()
	if done > desc.Size {
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		done = 0
	}
	resumed = done > 0

	digester := desc.Digest.Algorithm().Digester()
	if _, err := io.Copy(digester.Hash(), io.NewSectionReader(f, 0, done)); err != nil {
		return resumed, err
	}
	if _, err := f.Seek(done, io.SeekStart); err != nil {
		return resumed, err
	}
	if progress != nil {
		progress(done)
	}

	w := io.MultiWriter(f, digester.Hash())
	buf := make([]byte, bufSize)
	for done < desc.Size {
		if err := ctx.Err(); err != nil {
			return resumed, err
		}
		n, rerr := src.ReadAt(buf[:min(int64(len(buf)), desc.Size-done)], done)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return resumed, err
			}
			done += int64(n)
			if progress != nil {
				progress(done)
			}
		}
		if rerr != nil && (!errors.Is(rerr, io.EOF) || done != desc.Size) {
			if errors.Is(rerr, io.EOF) {
				rerr = fmt.Errorf(
					"%w: source ended at %d of %d bytes",
					ErrMismatch,
					done,
					desc.Size,
				)
				c.discard(name)
			}
			return resumed, fmt.Errorf("blobcache: fetching %s: %w", desc.Digest, rerr)
		}
	}

	if got := digester.Digest(); got != desc.Digest {
		c.discard(name)
		return resumed, fmt.Errorf("blobcache: %s: %w: got %s", desc.Digest, ErrMismatch, got)
	}
	if err := f.Sync(); err != nil {
		return resumed, err
	}
	return resumed, c.commit(name, desc.Digest)
}

// openLocked opens the partial file name and locks it exclusively against other processes.
// A waiter may wake up holding a file that the previous holder has since renamed or removed;
// it then reopens, so the locked file is always the one at name.
func (c *Cache) openLocked(name string) (*os.File, error) {
	for {
		f, err := c.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		if err := lockFile(f); err != nil {
			_ = f.Close()
			return nil, err
		}
		held, herr := f.Stat()
		current, cerr := c.root.Stat(name)
		if herr == nil && cerr == nil && os.SameFile(held, current) {
			return f, nil
		}
		_ = f.Close()
		if herr != nil {
			return nil, herr
		}
		if cerr != nil && !errors.Is(cerr, fs.ErrNotExist) {
			return nil, cerr
		}
	}
}

func (c *Cache) discard(name string) {
	_ = c.root.Remove(name)
}

func (c *Cache) commit(name string, dgst digest.Digest) error {
	target := entry(blobsDir, dgst)
	if err := c.root.MkdirAll(path.Dir(target), 0o755); err != nil {
		return err
	}
	return c.root.Rename(name, target)
}

// Put stores r, computing its digest and size, and returns the descriptor with mediaType.
func (c *Cache) Put(
	ctx context.Context,
	mediaType string,
	r io.Reader,
) (ocispec.Descriptor, error) {
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	name := path.Join(tmpDir, hex.EncodeToString(suffix[:]))
	f, err := c.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			c.discard(name)
		}
	}()

	digester := digest.Canonical.Digester()
	size, err := io.Copy(io.MultiWriter(f, digester.Hash()), contextReader{ctx, r})
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := f.Sync(); err != nil {
		return ocispec.Descriptor{}, err
	}
	if err := f.Close(); err != nil {
		return ocispec.Descriptor{}, err
	}
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digester.Digest(), Size: size}

	defer c.lock(desc.Digest)()
	if c.Has(desc.Digest) {
		return desc, nil // the deferred cleanup removes the duplicate
	}
	if err := c.commit(name, desc.Digest); err != nil {
		return ocispec.Descriptor{}, err
	}
	committed = true
	return desc, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
