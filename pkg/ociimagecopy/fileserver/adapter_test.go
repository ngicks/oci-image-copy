package fileserver_test

// adapter_test.go verifies the ChunkedSourceAdapter and ChunkedSinkAdapter
// compile-time interface assertions and behavioural contracts.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/ngicks/go-fsys-helper/fsutil"
	fsfileserver "github.com/ngicks/go-fsys-helper/stream/fileserver"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy/fileserver"
	"github.com/opencontainers/go-digest"
)

// ---- in-memory Client ----

type inMemFSClient struct {
	objects map[string][]byte
}

func newInMemFSClient() *inMemFSClient {
	return &inMemFSClient{objects: make(map[string][]byte)}
}

func (m *inMemFSClient) Get(
	_ context.Context, name string, offset int64,
) (io.ReadCloser, int64, error) {
	data, ok := m.objects[name]
	if !ok {
		return nil, 0, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[offset:])), int64(len(data)), nil
}

func (m *inMemFSClient) Stat(
	_ context.Context, name string,
) (int64, error) {
	data, ok := m.objects[name]
	if !ok {
		return 0, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return int64(len(data)), nil
}

func (m *inMemFSClient) Put(
	_ context.Context, name string, _ int64, r io.Reader,
) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.objects[name] = data
	return nil
}

// Compile-time assertion: inMemFSClient satisfies fsfileserver.Client.
var _ fsfileserver.Client = (*inMemFSClient)(nil)

// ---- helpers ----

// newTestDigest returns a sha256 digest of the given data.
func newTestDigest(data []byte) digest.Digest {
	return digest.SHA256.FromBytes(data)
}

func makeNaming() fileserver.NamingConvention {
	return fileserver.DefaultNaming{}
}

// ---- ChunkedSourceAdapter tests ----

// TestChunkedSourceAdapter_InterfaceAndETag verifies that:
//   - ChunkedSourceAdapter implements fsutil.ResumableSource at compile time.
//   - Open returns ContentInfo with the correct ETag and size.
func TestChunkedSourceAdapter_InterfaceAndETag(t *testing.T) {
	t.Parallel()

	data := []byte("hello world")
	dgst := newTestDigest(data)
	m := newInMemFSClient()
	n := makeNaming()
	// Populate the single chunk.
	chunkName := n.BlobChunk(dgst, 0)
	m.objects[chunkName] = data

	adapter := fileserver.NewChunkedSourceAdapter(m, n, dgst, int64(len(data)), 1024)

	// Open at offset 0.
	rc, info, err := adapter.Open(context.Background(), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if info.ETag != dgst.String() {
		t.Errorf("ETag = %q, want %q", info.ETag, dgst.String())
	}
	if info.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", info.Size, len(data))
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: got %q, want %q", got, data)
	}
}

// TestChunkedSinkAdapter_StateMapping verifies that SinkState.ETag == digest
// and Complete/Offset fields are correctly mapped from ChunkedSink.State.
func TestChunkedSinkAdapter_StateMapping(t *testing.T) {
	t.Parallel()

	data := []byte("abcdefghijklmnopqrstuvwxy") // 25 bytes
	dgst := newTestDigest(data)
	chunkSize := int64(10)
	m := newInMemFSClient()
	n := makeNaming()

	adapter := fileserver.NewChunkedSinkAdapter(m, n, dgst, int64(len(data)), chunkSize)

	// Fresh state: no chunks uploaded.
	st, err := adapter.State(context.Background())
	if err != nil {
		t.Fatalf("State (empty): %v", err)
	}
	if st.ETag != dgst.String() {
		t.Errorf("ETag = %q, want %q", st.ETag, dgst.String())
	}
	if st.Offset != 0 {
		t.Errorf("Offset = %d, want 0", st.Offset)
	}
	if st.Complete {
		t.Error("Complete = true on empty sink")
	}

	// Upload all chunks via Append.
	err = adapter.Append(
		context.Background(),
		fsutil.ContentInfo{ETag: dgst.String(), Size: int64(len(data))},
		0,
		bytes.NewReader(data),
	)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Complete state after all chunks uploaded.
	st, err = adapter.State(context.Background())
	if err != nil {
		t.Fatalf("State (complete): %v", err)
	}
	if st.ETag != dgst.String() {
		t.Errorf("ETag (complete) = %q, want %q", st.ETag, dgst.String())
	}
	if st.Offset != int64(len(data)) {
		t.Errorf("Offset (complete) = %d, want %d", st.Offset, len(data))
	}
	if !st.Complete {
		t.Error("Complete = false after full upload")
	}
}

// TestChunkedSinkAdapter_Commit verifies Commit is a no-op.
func TestChunkedSinkAdapter_Commit(t *testing.T) {
	t.Parallel()
	data := []byte("test")
	dgst := newTestDigest(data)
	m := newInMemFSClient()
	adapter := fileserver.NewChunkedSinkAdapter(m, makeNaming(), dgst, int64(len(data)), 1024)
	if err := adapter.Commit(context.Background()); err != nil {
		t.Errorf("Commit: %v", err)
	}
}
