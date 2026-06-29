package remote

// fileserver.go implements the file-server Remote variant
// (--remote file-server:<url>).
//
// Design (per PLAN3-fileserver-remote.md):
//
//   Each blob is stored as N chunk objects on a generic HTTP file server.
//   Object names are derived by a NamingConvention (DefaultNaming by default).
//   A per-image metadata object is written as the LAST step of a push — the
//   commit marker. A crash mid-push leaves at most orphan chunks (harmless for
//   content-addressed names), never a meta referencing missing chunks.
//
//   The Remote/StoreV1 interfaces are defined in package ociimagecopy; the
//   chunked naming/meta/adapters live in pkg/ociimagecopy/fileserver. This file
//   ties them together, depending on both with no back-reference from either.
//
// Limitations (documented per PLAN3):
//   - There is no global index on the server, so enumeration is per-image:
//     ListBlobsByImage reads a single image's meta. Blobs belonging to images
//     not accessed in this session are not reported.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"net/http"
	"strings"
	"sync"

	"github.com/ngicks/go-fsys-helper/fsutil"
	fsfileserver "github.com/ngicks/go-fsys-helper/stream/fileserver"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy/fileserver"
	"github.com/opencontainers/go-digest"
)

// Compile-time check: [*fileServerRemote] satisfies [ociimagecopy.Remote] and
// [ociimagecopy.StoreV1].
var _ ociimagecopy.Remote = (*fileServerRemote)(nil)
var _ ociimagecopy.StoreV1 = (*fileServerRemote)(nil)

// blobMetaInfo records the size and chunk size for a blob derived from a
// consulted image meta.
type blobMetaInfo struct {
	size      int64
	chunkSize int64
}

// fileServerRemote implements [ociimagecopy.Remote] and [ociimagecopy.StoreV1]
// over a generic HTTP file server. SSH-remote uses SFTP; this uses HTTP
// GET/HEAD/PUT.
//
// Thread safety: the struct is safe for concurrent use between different image
// refs. Per-image state (tagFileAccum) is protected by mu.
type fileServerRemote struct {
	client    fsfileserver.Client
	naming    fileserver.NamingConvention
	chunkSize int64
	readOnly  bool

	// tagFileAccum accumulates per-ref tag files (oci-layout and index.json)
	// until both are present, at which point the commit meta is assembled
	// and written via a single Put.
	mu    sync.Mutex
	accum map[string]*fsTagFileState // key = ref.String()

	// blobsFromMeta is the union of descriptors from metas consulted in this
	// run. Protected by mu.
	blobsFromMeta map[string]blobMetaInfo // digest string → {size, chunkSize}
}

// fsTagFileState holds the partially-accumulated tag files for one image ref.
type fsTagFileState struct {
	ociLayout []byte
	indexJSON []byte
}

// FileServerConfig configures [NewFileServer].
type FileServerConfig struct {
	// Client is the underlying file-server client (e.g. *fsfileserver.HTTPClient).
	// Required.
	Client fsfileserver.Client
	// Naming maps OCI-level requests to object names. Defaults to
	// fileserver.DefaultNaming{} when nil.
	Naming fileserver.NamingConvention
	// ChunkSize is the upload chunk size in bytes. Defaults to
	// [DefaultChunkSize].
	ChunkSize int64
	// ReadOnly disables all mutating operations.
	ReadOnly bool
}

// NewFileServer constructs a file-server [ociimagecopy.Remote].
func NewFileServer(cfg FileServerConfig) ociimagecopy.Remote {
	if cfg.Naming == nil {
		cfg.Naming = fileserver.DefaultNaming{}
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	return &fileServerRemote{
		client:        cfg.Client,
		naming:        cfg.Naming,
		chunkSize:     cfg.ChunkSize,
		readOnly:      cfg.ReadOnly,
		accum:         make(map[string]*fsTagFileState),
		blobsFromMeta: make(map[string]blobMetaInfo),
	}
}

// NewFileServerFromSpec constructs a file-server [ociimagecopy.Remote] from a
// [FileServerRemoteSpec]. This is the factory called by the CLI wiring.
func NewFileServerFromSpec(spec *FileServerRemoteSpec) (ociimagecopy.Remote, error) {
	hdr := make(http.Header)
	for _, h := range spec.Headers {
		name, val, ok := splitHeaderPair(h)
		if !ok {
			// Redact the value: a malformed header may still carry a secret
			// (e.g. a bare token with no name), so never echo it verbatim.
			return nil, fmt.Errorf(
				"file-server: invalid header %q (expected 'Name: value')",
				RedactHeader(h),
			)
		}
		hdr.Add(name, val)
	}

	c := &fsfileserver.HTTPClient{
		BaseURL: spec.URL,
		Header:  hdr,
		Client:  &http.Client{},
	}

	return NewFileServer(FileServerConfig{
		Client:    c,
		Naming:    fileserver.DefaultNaming{Prefix: spec.NamingPrefix},
		ChunkSize: spec.ChunkSize,
	}), nil
}

// splitHeaderPair splits "Name: value" into (name, value, true).
// Returns ("", "", false) when the input contains no ":".
func splitHeaderPair(s string) (name, val string, ok bool) {
	before, after, ok := strings.Cut(s, ":")
	if !ok {
		return "", "", false
	}
	name = before
	val = after
	if len(val) > 0 && val[0] == ' ' {
		val = val[1:]
	}
	return name, val, true
}

// --- Remote interface ---

// Close implements [ociimagecopy.Remote].
// When the underlying client is an [*fsfileserver.HTTPClient] and its inner
// Doer implements CloseIdleConnections, idle HTTP connections are released.
func (r *fileServerRemote) Close() error {
	if hc, ok := r.client.(*fsfileserver.HTTPClient); ok {
		if closer, ok := hc.Client.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
	return nil
}

// ReadOnly implements [ociimagecopy.Remote].
func (r *fileServerRemote) ReadOnly() bool { return r.readOnly }

// Blobs implements [ociimagecopy.Remote]. Returns self — fileServerRemote also
// implements [ociimagecopy.BlobStore].
func (r *fileServerRemote) Blobs() ociimagecopy.BlobStore { return r }

// Tags implements [ociimagecopy.Remote]. Returns self — fileServerRemote also
// implements [ociimagecopy.TagStoreV1].
func (r *fileServerRemote) Tags() ociimagecopy.TagStoreV1 { return r }

// ListBlobsByImage implements [ociimagecopy.Remote].
//
// Reads ref's per-image meta (one GET) and yields each descriptor digest.
// An absent image (the meta GET returns [fs.ErrNotExist]) yields nothing;
// any other error — transport/auth/server — is yielded once. The getImageMeta
// read also primes blobsFromMeta (chunkSize) as a side effect.
func (r *fileServerRemote) ListBlobsByImage(
	ctx context.Context,
	ref imageref.ImageRef,
) iter.Seq2[digest.Digest, error] {
	return func(yield func(digest.Digest, error) bool) {
		meta, err := r.getImageMeta(ctx, ref)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			yield(digest.Digest(""), err)
			return
		}
		for i := range meta.Descriptors {
			if !yield(digest.Digest(meta.Descriptors[i].Digest), nil) {
				return
			}
		}
	}
}

// LoadImage implements [ociimagecopy.Remote]. No-op: the file server IS the
// store; there is no separate live container runtime to load into.
func (r *fileServerRemote) LoadImage(_ context.Context, _ imageref.ImageRef) error {
	return nil
}

// DumpImage implements [ociimagecopy.Remote]. No-op: the file server IS the
// store; there is no separate live storage to dump from.
func (r *fileServerRemote) DumpImage(_ context.Context, _ imageref.ImageRef) error {
	return nil
}

// InspectImage implements [ociimagecopy.Remote].
//
// Fetches the per-image metadata, identifies the manifest descriptor (first
// entry in Descriptors), reads the manifest blob bytes via ChunkedSource,
// and returns the raw bytes. sha256(returned bytes) == manifest descriptor digest.
func (r *fileServerRemote) InspectImage(
	ctx context.Context,
	ref imageref.ImageRef,
) ([]byte, error) {
	meta, err := r.getImageMeta(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("file-server inspect %s: %w", ref.String(), err)
	}
	if len(meta.Descriptors) == 0 {
		return nil, fmt.Errorf(
			"file-server inspect %s: meta has no descriptors", ref.String(),
		)
	}
	mDesc := meta.Descriptors[0]
	dgst := digest.Digest(mDesc.Digest)

	src := fileserver.NewChunkedSourceAdapter(r.client, r.naming, dgst, mDesc.Size, meta.ChunkSize)
	rc, _, err := src.Open(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf(
			"file-server inspect %s: open manifest blob %s: %w",
			ref.String(), mDesc.Digest, err,
		)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf(
			"file-server inspect %s: read manifest blob: %w", ref.String(), err,
		)
	}
	// Verify the raw manifest bytes hash back to the descriptor digest:
	// callers rely on sha256(returned bytes) == manifest digest, and the
	// chunked blob read is the trust-root content-addressed read here.
	if err := ocidir.VerifyBlobBytes(dgst, data); err != nil {
		return nil, fmt.Errorf("file-server inspect %s: %w", ref.String(), err)
	}
	return data, nil
}

// getImageMeta fetches and parses the per-image metadata for ref.
// Also registers the meta's descriptors in blobsFromMeta for ListBlobs.
func (r *fileServerRemote) getImageMeta(
	ctx context.Context,
	ref imageref.ImageRef,
) (fileserver.ImageMeta, error) {
	name := r.naming.ImageMeta(ref)
	rc, _, err := r.client.Get(ctx, name, 0)
	if err != nil {
		return fileserver.ImageMeta{}, fmt.Errorf(
			"file-server: get meta for %s: %w", ref.String(), err,
		)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fileserver.ImageMeta{}, fmt.Errorf(
			"file-server: read meta for %s: %w", ref.String(), err,
		)
	}

	meta, err := fileserver.UnmarshalImageMeta(data)
	if err != nil {
		return fileserver.ImageMeta{}, fmt.Errorf(
			"file-server: parse meta for %s: %w", ref.String(), err,
		)
	}

	// Register descriptors for ListBlobs, recording the meta's chunkSize.
	r.mu.Lock()
	for _, d := range meta.Descriptors {
		r.blobsFromMeta[d.Digest] = blobMetaInfo{
			size:      d.Size,
			chunkSize: meta.ChunkSize,
		}
	}
	r.mu.Unlock()

	return meta, nil
}

// --- BlobStore interface ---

// Stat implements [ociimagecopy.BlobStore.Stat]. It reports how much of the
// blob is present on the file server using the chunk-state machinery (the old
// ProbeBlob logic): the chunkSize is the meta-primed value when available,
// else the remote's configured chunkSize.
//
//   - complete → CurrentSize == Size;
//   - partial (offset > 0) → CurrentSize == offset;
//   - offset == 0 && !complete → fs.ErrNotExist;
//   - a real error is propagated (NOT mapped to not-exist).
func (r *fileServerRemote) Stat(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (ociimagecopy.BlobInfo, error) {
	chunkSize := r.chunkSize
	r.mu.Lock()
	if info, ok := r.blobsFromMeta[dgst.String()]; ok && info.chunkSize > 0 {
		chunkSize = info.chunkSize
	}
	r.mu.Unlock()

	sink := fileserver.NewChunkedSinkAdapter(r.client, r.naming, dgst, size, chunkSize)
	st, err := sink.State(ctx)
	if err != nil {
		return ociimagecopy.BlobInfo{}, fmt.Errorf("file-server stat %s: %w", dgst, err)
	}
	if st.Complete {
		return ociimagecopy.BlobInfo{CurrentSize: size, Size: size}, nil
	}
	// Only a strictly-partial chunk prefix is a usable resume point; anything
	// that is not Complete must report CurrentSize < Size so the skip test
	// ("complete ⟺ CurrentSize == Size") cannot fire on unverified content.
	// (ChunkedSink.State already guarantees offset < size when !complete; the
	// bound keeps the invariant uniform with the fs backend.)
	if st.Offset > 0 && st.Offset < size {
		return ociimagecopy.BlobInfo{CurrentSize: st.Offset, Size: size}, nil
	}
	return ociimagecopy.BlobInfo{}, fmt.Errorf("file-server stat %s: %w", dgst, fs.ErrNotExist)
}

// PrepDownload implements [ociimagecopy.BlobStore.PrepDownload] (pull direction).
// Returns a [fsutil.ResumableSource] backed by ChunkedSource.
// Uses the chunkSize recorded in the consulted meta when available, falling
// back to the remote's configured chunkSize otherwise.
func (r *fileServerRemote) PrepDownload(
	_ context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSource, error) {
	chunkSize := r.chunkSize
	r.mu.Lock()
	if info, ok := r.blobsFromMeta[dgst.String()]; ok && info.chunkSize > 0 {
		chunkSize = info.chunkSize
	}
	r.mu.Unlock()
	return fileserver.NewChunkedSourceAdapter(r.client, r.naming, dgst, size, chunkSize), nil
}

// PrepUpload implements [ociimagecopy.BlobStore.PrepUpload] (push direction).
// Returns a [fsutil.ResumableSink] backed by ChunkedSink.
// Returns [ociimagecopy.ErrReadOnly] when the remote is read-only.
// No parent-dir creation: the file server has a flat keyspace.
func (r *fileServerRemote) PrepUpload(
	_ context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSink, error) {
	if r.readOnly {
		return nil, ociimagecopy.ErrReadOnly
	}
	return fileserver.NewChunkedSinkAdapter(r.client, r.naming, dgst, size, r.chunkSize), nil
}

// --- TagStoreV1 interface ---

// GetIndex implements [ociimagecopy.TagStoreV1.GetIndex]. Returns the verbatim
// index.json bytes from the per-image meta. getImageMeta's not-exist error
// already wraps [fs.ErrNotExist].
func (r *fileServerRemote) GetIndex(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	meta, err := r.getImageMeta(ctx, ref)
	if err != nil {
		return nil, err
	}
	return []byte(meta.IndexJSON), nil
}

// GetOciLayout implements [ociimagecopy.TagStoreV1.GetOciLayout]. Returns the
// verbatim oci-layout bytes from the per-image meta. getImageMeta's not-exist
// error already wraps [fs.ErrNotExist].
func (r *fileServerRemote) GetOciLayout(ctx context.Context, ref imageref.ImageRef) ([]byte, error) {
	meta, err := r.getImageMeta(ctx, ref)
	if err != nil {
		return nil, err
	}
	return []byte(meta.OciLayout), nil
}

// PutIndex implements [ociimagecopy.TagStoreV1.PutIndex]. Stashes the verbatim
// index.json half in the per-ref accumulator; the commit fires once both
// halves are present.
func (r *fileServerRemote) PutIndex(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	return r.accumulateTagFile(ctx, ref, "index.json", raw)
}

// PutOciLayout implements [ociimagecopy.TagStoreV1.PutOciLayout]. Stashes the
// verbatim oci-layout half in the per-ref accumulator; the commit fires once
// both halves are present.
func (r *fileServerRemote) PutOciLayout(ctx context.Context, ref imageref.ImageRef, raw []byte) error {
	return r.accumulateTagFile(ctx, ref, "oci-layout", raw)
}

// accumulateTagFile stashes one verbatim tag-file half (oci-layout or
// index.json) per ref in memory. When both are present the meta is assembled
// (descriptor closure derived from the uploaded manifest blob) and written via
// a single Put — the commit marker.
//
// Push ordering is blobs-first/tag-files-last (enforced by push.go
// mirrorTagFiles, which is called after pushBlobs). By the time the second half
// arrives for a given ref, all blobs are already uploaded, so reading the
// manifest blob for descriptor derivation is always safe.
//
// Cross-image state leaks are prevented: each ref has its own accumulation
// entry keyed by ref.String().
func (r *fileServerRemote) accumulateTagFile(
	ctx context.Context,
	ref imageref.ImageRef,
	name string,
	data []byte,
) error {
	if r.readOnly {
		return ociimagecopy.ErrReadOnly
	}

	r.mu.Lock()
	key := ref.String()
	state := r.accum[key]
	if state == nil {
		state = &fsTagFileState{}
		r.accum[key] = state
	}
	switch name {
	case "oci-layout":
		state.ociLayout = append([]byte(nil), data...)
	case "index.json":
		state.indexJSON = append([]byte(nil), data...)
	}
	ready := state.ociLayout != nil && state.indexJSON != nil
	var snap *fsTagFileState
	if ready {
		snap = state
		delete(r.accum, key) // consume — no double-commit
	}
	r.mu.Unlock()

	if !ready {
		return nil
	}

	return r.commitImageMeta(ctx, ref, snap)
}

// commitImageMeta assembles the per-image metadata from the accumulated tag
// files and the uploaded manifest blob, then writes it via a single Put.
//
// This is the "commit" in the crash-consistent write sequence:
// blobs → index.json + oci-layout (tag files) → meta PUT (commit marker).
func (r *fileServerRemote) commitImageMeta(
	ctx context.Context,
	ref imageref.ImageRef,
	state *fsTagFileState,
) error {
	// Parse index.json (unmarshal + validate) via the shared choke point to
	// locate the manifest descriptor.
	idx, err := ocidir.ParseIndex(state.indexJSON)
	if err != nil {
		return fmt.Errorf(
			"file-server commit meta %s: parse index.json: %w", ref.String(), err,
		)
	}
	mDesc := idx.Manifests[0]
	if mDesc.Digest == "" {
		return fmt.Errorf(
			"file-server commit meta %s: manifest descriptor has no digest", ref.String(),
		)
	}

	// Read the manifest blob bytes to derive the full descriptor closure.
	// The blob MUST be present (blobs-first ordering guarantees this).
	manifestBytes, err := r.readBlobFull(ctx, mDesc.Digest, mDesc.Size)
	if err != nil {
		return fmt.Errorf(
			"file-server commit meta %s: read manifest blob %s: %w",
			ref.String(), mDesc.Digest, err,
		)
	}

	// Verify the manifest bytes hash back to the descriptor digest before
	// deriving the descriptor closure from them — the blob we just read
	// is the trust root for every config/layer digest recorded in the meta.
	if err := ocidir.VerifyBlobBytes(mDesc.Digest, manifestBytes); err != nil {
		return fmt.Errorf("file-server commit meta %s: %w", ref.String(), err)
	}

	man, err := ocidir.ParseManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf(
			"file-server commit meta %s: parse manifest: %w", ref.String(), err,
		)
	}

	descs := fileserver.DescriptorsFromManifest(mDesc, man)

	meta := fileserver.ImageMeta{
		Version:     1,
		ChunkSize:   r.chunkSize,
		OciLayout:   json.RawMessage(state.ociLayout),
		IndexJSON:   json.RawMessage(state.indexJSON),
		Descriptors: descs,
	}

	metaBytes, err := fileserver.MarshalImageMeta(meta)
	if err != nil {
		return fmt.Errorf(
			"file-server commit meta %s: marshal: %w", ref.String(), err,
		)
	}

	metaName := r.naming.ImageMeta(ref)
	if err := r.client.Put(
		ctx, metaName, int64(len(metaBytes)), bytes.NewReader(metaBytes),
	); err != nil {
		return fmt.Errorf(
			"file-server commit meta %s: put %s: %w", ref.String(), metaName, err,
		)
	}
	return nil
}

// readBlobFull reads all bytes of the blob identified by dgst / size via
// ChunkedSource. Used by commitImageMeta to read the manifest blob.
func (r *fileServerRemote) readBlobFull(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) ([]byte, error) {
	src := fileserver.NewChunkedSourceAdapter(r.client, r.naming, dgst, size, r.chunkSize)
	rc, _, err := src.Open(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
