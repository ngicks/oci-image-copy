package blobcache_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/blobcache"
)

var errInterrupted = errors.New("interrupted")

// source serves data, failing reads at or past failAt when failAt >= 0, and records read offsets.
type source struct {
	data   []byte
	failAt int64
	offs   []int64
}

func (s *source) ReadAt(p []byte, off int64) (int, error) {
	s.offs = append(s.offs, off)
	if s.failAt >= 0 && off >= s.failAt {
		return 0, errInterrupted
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if s.failAt >= 0 && off+int64(n) > s.failAt {
		n = int(s.failAt - off)
	}
	return n, nil
}

func (s *source) Close() error { return nil }
func (s *source) Size() int64  { return int64(len(s.data)) }

func blob(n int) ([]byte, ocispec.Descriptor) {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromBytes(data),
		Size:      int64(n),
	}
}

func newCache(t *testing.T) (*blobcache.Cache, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := blobcache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, dir
}

func readCached(t *testing.T, c *blobcache.Cache, dgst digest.Digest) []byte {
	t.Helper()
	r, err := c.Open(dgst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func partialPath(dir string, dgst digest.Digest) string {
	return filepath.Join(dir, "partial", dgst.Algorithm().String(), dgst.Encoded())
}

func TestFetch(t *testing.T) {
	c, _ := newCache(t)
	data, desc := blob(3<<20 + 17)

	var last int64
	if err := c.Fetch(
		t.Context(),
		desc,
		&source{data: data, failAt: -1},
		func(d int64) { last = d },
	); err != nil {
		t.Fatal(err)
	}
	if !c.Has(desc.Digest) {
		t.Fatal("Has = false after Fetch")
	}
	if !bytes.Equal(readCached(t, c, desc.Digest), data) {
		t.Error("cached content differs")
	}
	if last != desc.Size {
		t.Errorf("last progress = %d, want %d", last, desc.Size)
	}

	src := &source{data: data, failAt: -1}
	if err := c.Fetch(t.Context(), desc, src, nil); err != nil {
		t.Fatal(err)
	}
	if len(src.offs) != 0 {
		t.Errorf("Fetch of a cached blob read the source at %v", src.offs)
	}
}

func TestFetch_resume(t *testing.T) {
	c, dir := newCache(t)
	data, desc := blob(4 << 20)
	half := desc.Size / 2

	err := c.Fetch(t.Context(), desc, &source{data: data, failAt: half}, nil)
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("interrupted Fetch: want errInterrupted, got %v", err)
	}
	info, err := os.Stat(partialPath(dir, desc.Digest))
	if err != nil || info.Size() != half {
		t.Fatalf("partial file after interruption: %v, %v; want %d bytes", info, err, half)
	}

	src := &source{data: data, failAt: -1}
	if err := c.Fetch(t.Context(), desc, src, nil); err != nil {
		t.Fatal(err)
	}
	if first := slices.Min(src.offs); first != half {
		t.Errorf("resumed Fetch first read at %d, want %d", first, half)
	}
	if !bytes.Equal(readCached(t, c, desc.Digest), data) {
		t.Error("cached content differs")
	}
	if _, err := os.Stat(partialPath(dir, desc.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial file remains after commit: %v", err)
	}
}

func TestFetch_corruptPartial(t *testing.T) {
	c, dir := newCache(t)
	data, desc := blob(1 << 20)
	p := partialPath(dir, desc.Digest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{0xff}, 1000), 0o644); err != nil {
		t.Fatal(err)
	}

	src := &source{data: data, failAt: -1}
	if err := c.Fetch(t.Context(), desc, src, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(src.offs, 0) {
		t.Error("corrupt partial was not refetched from offset 0")
	}
	if !bytes.Equal(readCached(t, c, desc.Digest), data) {
		t.Error("cached content differs")
	}
}

func TestFetch_mismatch(t *testing.T) {
	c, dir := newCache(t)
	_, desc := blob(1 << 10)
	other, _ := blob(1 << 10)
	other[0] ^= 1

	err := c.Fetch(t.Context(), desc, &source{data: other, failAt: -1}, nil)
	if !errors.Is(err, blobcache.ErrMismatch) {
		t.Fatalf("want ErrMismatch, got %v", err)
	}
	if c.Has(desc.Digest) {
		t.Error("mismatching content was committed")
	}
	if _, err := os.Stat(partialPath(dir, desc.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial file remains after mismatch: %v", err)
	}

	short := desc
	short.Size++
	if err := c.Fetch(
		t.Context(),
		short,
		&source{data: other, failAt: -1},
		nil,
	); !errors.Is(
		err,
		blobcache.ErrMismatch,
	) {
		t.Errorf("source shorter than descriptor: want ErrMismatch, got %v", err)
	}
}

func TestPut(t *testing.T) {
	c, _ := newCache(t)
	data, want := blob(5000)
	for range 2 {
		got, err := c.Put(t.Context(), ocispec.MediaTypeImageLayer, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest != want.Digest || got.Size != want.Size || got.MediaType != want.MediaType {
			t.Errorf("Put = %#v, want %#v", got, want)
		}
	}
	if !bytes.Equal(readCached(t, c, want.Digest), data) {
		t.Error("cached content differs")
	}
}

// slowSource delays every read so a second fetcher arrives while the first holds the partial file.
type slowSource struct {
	source
	delay time.Duration
}

func (s *slowSource) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(s.delay)
	return s.source.ReadAt(p, off)
}

// TestFetch_sharedDir uses two Cache values on one directory as two processes would:
// they share no mutex, only the lock on the partial file.
func TestFetch_sharedDir(t *testing.T) {
	_, dir := newCache(t)
	first, err := blobcache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := blobcache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	data, desc := blob(4 << 20)

	firstErr := make(chan error, 1)
	go func() {
		slow := &slowSource{source{data: data, failAt: -1}, 30 * time.Millisecond}
		firstErr <- first.Fetch(t.Context(), desc, slow, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, err := os.Stat(partialPath(dir, desc.Digest)); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first fetch never started writing")
		}
		time.Sleep(time.Millisecond)
	}

	src := &source{data: data, failAt: -1}
	if err := second.Fetch(t.Context(), desc, src, nil); err != nil {
		t.Errorf("second fetch: %v", err)
	}
	if err := <-firstErr; err != nil {
		t.Errorf("first fetch: %v", err)
	}
	if len(src.offs) != 0 {
		t.Errorf("second fetch read its source at %v instead of waiting for the first", src.offs)
	}
	if !bytes.Equal(readCached(t, second, desc.Digest), data) {
		t.Error("cached content differs")
	}
}
