package imagecopy

// fileserver_remote.go implements the file-server Remote variant
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
//   The implementation lives in the imagecopy package (alongside localdir_remote.go
//   and the ssh remote) to avoid import cycles: the Remote/OciDirs interfaces are
//   defined here, and pkg/imagecopy/fileserver (naming, meta, adapters) is a
//   dependency-only sub-package with no back-reference.
//
// Limitations (documented per PLAN3):
//   - ListImages: not supported (no global index on the server).
//   - ListBlobs: returns the union of descriptors from metas consulted in this
//     run (the refs being pushed/pulled). Blobs from other images not accessed
//     in this session are not reported.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"maps"
	"net/http"
	"strings"
	"sync"

	"github.com/ngicks/go-fsys-helper/fsutil"
	fsfileserver "github.com/ngicks/go-fsys-helper/stream/fileserver"
	"github.com/ngicks/oci-image-copy/pkg/imagecopy/fileserver"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Compile-time check: [*fileServerRemote] satisfies [Remote] and [OciDirs].
var _ Remote = (*fileServerRemote)(nil)
var _ OciDirs = (*fileServerRemote)(nil)

// blobMetaInfo records the size and chunk size for a blob derived from a
// consulted image meta.
type blobMetaInfo struct {
	size      int64
	chunkSize int64
}

// fileServerRemote implements [Remote] and [OciDirs] over a generic HTTP
// file server. SSH-remote uses SFTP; this uses HTTP GET/HEAD/PUT.
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

// FileServerConfig configures [NewFileServerRemote].
type FileServerConfig struct {
	// Client is the underlying file-server client (e.g. *fsfileserver.HTTPClient).
	// Required.
	Client fsfileserver.Client
	// Naming maps OCI-level requests to object names. Defaults to
	// fileserver.DefaultNaming{} when nil.
	Naming fileserver.NamingConvention
	// ChunkSize is the upload chunk size in bytes. Defaults to DefaultChunkSize.
	ChunkSize int64
	// ReadOnly disables all mutating operations.
	ReadOnly bool
}

// NewFileServerRemote constructs a file-server [Remote].
func NewFileServerRemote(cfg FileServerConfig) Remote {
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

// NewFileServerRemoteFromSpec constructs a file-server [Remote] from a
// [FileServerRemoteSpec]. This is the factory called by the CLI wiring.
func NewFileServerRemoteFromSpec(spec *FileServerRemoteSpec) (Remote, error) {
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

	return NewFileServerRemote(FileServerConfig{
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

// Close implements [Remote].
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

// ReadOnly implements [Remote].
func (r *fileServerRemote) ReadOnly() bool { return r.readOnly }

// Dir implements [Remote]. Returns self — fileServerRemote also implements OciDirs.
func (r *fileServerRemote) Dir() OciDirs { return r }

// ListBlobs implements [Remote].
//
// Returns the union of descriptors from metas consulted in this run.
// Blobs belonging to images not accessed in this session are not reported.
// This is a documented limitation: the file server has no global index.
// Use --assume-remote-has to provide a complete inventory when needed.
func (r *fileServerRemote) ListBlobs(_ context.Context) iter.Seq2[digest.Digest, error] {
	r.mu.Lock()
	snap := make(map[string]blobMetaInfo, len(r.blobsFromMeta))
	maps.Copy(snap, r.blobsFromMeta)
	r.mu.Unlock()

	return func(yield func(digest.Digest, error) bool) {
		for d := range snap {
			if !yield(digest.Digest(d), nil) {
				return
			}
		}
	}
}

// ListImages implements [Remote].
// Always returns an error: file-server remotes have no global index.
func (r *fileServerRemote) ListImages(_ context.Context) iter.Seq2[imageref.ImageRef, error] {
	err := errors.New(
		"file-server remote: ListImages is not supported — there is no global " +
			"index on the server; use per-image InspectImage / ListBlobs, or " +
			"--assume-remote-has to skip server enumeration",
	)
	return func(yield func(imageref.ImageRef, error) bool) {
		yield(imageref.ImageRef{}, err)
	}
}

// LoadImage implements [Remote]. No-op: the file server IS the store;
// there is no separate live container runtime to load into.
func (r *fileServerRemote) LoadImage(_ context.Context, _ imageref.ImageRef) error {
	return nil
}

// DumpImage implements [Remote]. No-op: the file server IS the store;
// there is no separate live storage to dump from.
func (r *fileServerRemote) DumpImage(_ context.Context, _ imageref.ImageRef) error {
	return nil
}

// InspectImage implements [Remote].
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

// --- OciDirs interface ---

// ErrMultiChunkBlobUnsupported is returned by [fileServerRemote.Blob] when the
// blob spans more than one chunk. The meta-less top-level Blob accessor only
// reads chunk 0; rather than silently truncating a multi-chunk blob to its
// first chunk (a latent landmine — callers comparing against the descriptor
// size would see a short read), it errors explicitly. Callers that need a
// multi-chunk blob have the size and must use BlobSource / the meta-backed
// DirV1.Blob instead.
var ErrMultiChunkBlobUnsupported = errors.New(
	"file-server: meta-less Blob accessor cannot serve a multi-chunk blob; use BlobSource",
)

// Blob implements [OciDirs]. Reads chunk 0 directly for size detection.
// This method is primarily used by InspectImage's fallback and ocidir.ReadManifest.
//
// Without a meta the total size is unknown up front, so this accessor can only
// honestly serve single-chunk blobs. If a second chunk exists, the blob is
// multi-chunk and the call fails with [ErrMultiChunkBlobUnsupported] instead of
// returning only chunk 0 while claiming the full size.
func (r *fileServerRemote) Blob(
	ctx context.Context,
	dgst digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	// A second chunk means the blob is multi-chunk: refuse rather than
	// truncate. (chunk-0-only reads are otherwise indistinguishable from a
	// complete short blob.)
	if _, err := r.client.Stat(ctx, r.naming.BlobChunk(dgst, 1)); err == nil {
		return nil, 0, fmt.Errorf("%w: digest=%s", ErrMultiChunkBlobUnsupported, dgst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A transport/auth/server error probing chunk 1 must not be swallowed
		// as "single-chunk"; surface it.
		return nil, 0, fmt.Errorf("file-server Blob %s probe chunk1: %w", dgst, err)
	}

	name := r.naming.BlobChunk(dgst, 0)
	rc, total, err := r.client.Get(ctx, name, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("file-server Blob %s chunk0: %w", dgst, err)
	}
	return rc, total, nil
}

// Image implements [OciDirs].
// Returns a [fsMetaDirV1] view that serves index.json and oci-layout verbatim
// from the per-image meta, and reads blobs via ChunkedSource.
func (r *fileServerRemote) Image(ref imageref.ImageRef) ocidir.DirV1 {
	return &fsMetaDirV1{remote: r, ref: ref}
}

// BlobSource implements [OciDirs] (pull direction).
// Returns a [fsutil.ResumableSource] backed by ChunkedSource.
// Uses the chunkSize recorded in the consulted meta when available, falling
// back to the remote's configured chunkSize otherwise.
func (r *fileServerRemote) BlobSource(
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

// BlobSink implements [OciDirs] (push direction).
// Returns a [fsutil.ResumableSink] backed by ChunkedSink.
// Returns [ErrReadOnly] when the remote is read-only.
func (r *fileServerRemote) BlobSink(
	_ context.Context,
	dgst digest.Digest,
	size int64,
) (fsutil.ResumableSink, error) {
	if r.readOnly {
		return nil, ErrReadOnly
	}
	return fileserver.NewChunkedSinkAdapter(r.client, r.naming, dgst, size, r.chunkSize), nil
}

// MkdirBlobParent implements [OciDirs]. No-op: the file server has no
// directory hierarchy — objects are flat-named by key.
func (r *fileServerRemote) MkdirBlobParent(_ digest.Digest) error { return nil }

// PutTagFile implements [OciDirs].
//
// Accumulates oci-layout and index.json per ref in memory. When both are
// present the meta is assembled (descriptor closure derived from the uploaded
// manifest blob) and written via a single Put — the commit marker.
//
// Push ordering is blobs-first/tag-files-last (enforced by push.go
// mirrorTagFiles which is called after pushBlobs). By the time PutTagFile
// fires for a given ref, all blobs are already uploaded, so reading the
// manifest blob for descriptor derivation is always safe.
//
// Cross-image state leaks are prevented: each ref has its own accumulation
// entry keyed by ref.String().
func (r *fileServerRemote) PutTagFile(
	ctx context.Context,
	ref imageref.ImageRef,
	name string,
	data []byte,
) error {
	if r.readOnly {
		return ErrReadOnly
	}
	if name != "oci-layout" && name != "index.json" {
		return fmt.Errorf(
			"file-server PutTagFile: unexpected name %q (want oci-layout or index.json)", name,
		)
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

// --- fsMetaDirV1 ---

// fsMetaDirV1 implements [ocidir.DirV1] backed by the per-image metadata.
//
// Index and ImageLayout serve the verbatim raw bytes from the meta so that
// sha256 math over those bytes holds (no re-marshalling). Blob reads delegate
// to ChunkedSource using the chunkSize recorded in the meta.
type fsMetaDirV1 struct {
	remote *fileServerRemote
	ref    imageref.ImageRef

	// lazily fetched meta; protected by mu.
	mu      sync.Mutex
	meta    *fileserver.ImageMeta
	metaErr error
}

// fetchMeta fetches the meta once and caches the result.
func (d *fsMetaDirV1) fetchMeta(ctx context.Context) (*fileserver.ImageMeta, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.meta != nil {
		return d.meta, nil
	}
	if d.metaErr != nil {
		return nil, d.metaErr
	}
	meta, err := d.remote.getImageMeta(ctx, d.ref)
	if err != nil {
		d.metaErr = err
		return nil, err
	}
	d.meta = &meta
	return d.meta, nil
}

// Index implements [ocidir.DirV1].
// Returns the verbatim index.json parsed from the meta; no re-marshalling.
func (d *fsMetaDirV1) Index() (v1.Index, error) {
	meta, err := d.fetchMeta(context.Background())
	if err != nil {
		return v1.Index{}, err
	}
	return meta.ParsedIndex()
}

// ImageLayout implements [ocidir.DirV1].
// Returns the verbatim oci-layout parsed from the meta.
func (d *fsMetaDirV1) ImageLayout() (v1.ImageLayout, error) {
	meta, err := d.fetchMeta(context.Background())
	if err != nil {
		return v1.ImageLayout{}, err
	}
	return meta.ParsedImageLayout()
}

// RawIndex implements [ocidir.RawAccessor].
// Returns the verbatim raw bytes of index.json from the per-image meta.
func (d *fsMetaDirV1) RawIndex() ([]byte, error) {
	meta, err := d.fetchMeta(context.Background())
	if err != nil {
		return nil, err
	}
	return []byte(meta.IndexJSON), nil
}

// RawImageLayout implements [ocidir.RawAccessor].
// Returns the verbatim raw bytes of oci-layout from the per-image meta.
func (d *fsMetaDirV1) RawImageLayout() ([]byte, error) {
	meta, err := d.fetchMeta(context.Background())
	if err != nil {
		return nil, err
	}
	return []byte(meta.OciLayout), nil
}

// Blob implements [ocidir.DirV1].
// Reads blob bytes via ChunkedSource using the chunkSize from the meta.
// Returns [ErrNotExist] when the digest is not in the meta's descriptor list.
func (d *fsMetaDirV1) Blob(
	ctx context.Context,
	dgst digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	meta, err := d.fetchMeta(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("file-server DirV1 Blob %s: fetch meta: %w", dgst, err)
	}

	// Locate the blob size in the meta descriptors.
	var size int64 = -1
	for _, desc := range meta.Descriptors {
		if desc.Digest == dgst.String() {
			size = desc.Size
			break
		}
	}
	if size < 0 {
		return nil, 0, fmt.Errorf(
			"file-server DirV1 Blob %s: not found in meta for %s",
			dgst, d.ref.String(),
		)
	}

	src := fileserver.NewChunkedSourceAdapter(
		d.remote.client, d.remote.naming, dgst, size, meta.ChunkSize,
	)
	rc, err := src.Src.Open(ctx, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("file-server DirV1 Blob %s: open source: %w", dgst, err)
	}
	return rc, size, nil
}

// --- RefPrimer / BlobProber optional interfaces ---

// RefPrimer is an optional interface that a Remote's OciDirs may implement
// to pre-load remote metadata for the refs being pushed, enabling accurate
// dry-run plans and push remote-has resolution.
type RefPrimer interface {
	PrimeRefs(ctx context.Context, refs []imageref.ImageRef) error
}

// BlobProber is an optional interface that a Remote's OciDirs may implement
// to probe individual blobs for existence (via HEAD chunk probe).
// Returns true if the blob is complete on the remote.
type BlobProber interface {
	ProbeBlob(ctx context.Context, dgst digest.Digest, size int64) (bool, error)
}

// Compile-time checks: fileServerRemote satisfies both optional interfaces.
var _ RefPrimer = (*fileServerRemote)(nil)
var _ BlobProber = (*fileServerRemote)(nil)

// PrimeRefs implements [RefPrimer].
//
// For each ref it fetches the image meta to load chunkSize information into
// blobsFromMeta for use by BlobSource and ProbeBlob.
//
// Error taxonomy (decision D14): an absent meta ([fs.ErrNotExist], e.g. a 404
// for a first push) is normal and skipped silently. Any other error —
// transport failure, auth (401/403), server error (5xx) — is propagated: those
// must NOT be read as "absent", because doing so would silently downgrade a
// failed inventory probe into "the remote has nothing, send everything".
func (r *fileServerRemote) PrimeRefs(ctx context.Context, refs []imageref.ImageRef) error {
	for _, ref := range refs {
		if _, err := r.getImageMeta(ctx, ref); err != nil {
			// Absent images are normal (e.g. first push); skip silently.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			// Transport/auth/server errors are NOT "absent": surface them so
			// the caller does not mistake a failed probe for an empty remote.
			return fmt.Errorf("file-server prime %s: %w", ref.String(), err)
		}
	}
	return nil
}

// ProbeBlob implements [BlobProber].
// Creates a ChunkedSinkAdapter with the known (or fallback) chunkSize and
// calls State to determine whether the blob is already fully present on the
// remote. Returns true if the blob is complete.
func (r *fileServerRemote) ProbeBlob(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
) (bool, error) {
	chunkSize := r.chunkSize
	r.mu.Lock()
	if info, ok := r.blobsFromMeta[dgst.String()]; ok && info.chunkSize > 0 {
		chunkSize = info.chunkSize
	}
	r.mu.Unlock()

	sink := fileserver.NewChunkedSinkAdapter(r.client, r.naming, dgst, size, chunkSize)
	state, err := sink.State(ctx)
	if err != nil {
		return false, err
	}
	return state.Complete, nil
}
