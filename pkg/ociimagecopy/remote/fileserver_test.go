package remote

// fileserver_test.go unit-tests the fileServerRemote implementation.
// Scenarios covered:
//   - PutIndex/PutOciLayout accumulation: meta Put happens exactly once, after
//     both files, with correct descriptors (D15).
//   - InspectImage: reads manifest blob; sha256(returned bytes)==manifest digest.
//   - ReadOnly: PutOciLayout/PrepUpload return ociimagecopy.ErrReadOnly.
//   - Close: no error.
//   - ListBlobsByImage: derived from consulted meta (replaces global ListBlobs,
//     which was deleted per D4/D5).
//   - Blobs()/Tags() return self (replaces Dir()==OciDirs(r) identity, per D12).
//   - PrepDownload uses meta chunkSize (replaces BlobSource test).
//   - Stat: complete/partial/absent/transport-error taxonomy (replaces ProbeBlob).
//   - ListBlobsByImage error taxonomy: transport error propagated; absent meta
//     yields nothing (replaces PrimeRefs error tests, per D3/D5).
//   - LoadImage / DumpImage: no-op assertions.
//
// DELETED (with reasons):
//   - TestFileServerRemote_ListImages_Error: Remote.ListImages removed (D4).
//   - TestFileServerRemote_MkdirBlobParent_NoOp: MkdirBlobParent removed (D9).
//   - TestFileServerRemote_PrimeRefs, _PropagatesTransportError, _AbsentIsSilent:
//     PrimeRefs/RefPrimer dropped (D3). Intent re-expressed as
//     TestFileServerRemote_ListBlobsByImage_PropagatesTransportError and
//     TestFileServerRemote_ListBlobsByImage_AbsentIsSilent below.
//   - TestFileServerRemote_ProbeBlob, _PropagatesTransportError: ProbeBlob folded
//     into Stat (D10). Intent re-expressed as Stat tests below.
//   - TestFileServerRemote_Blob_MultiChunkUnsupported,
//     TestFileServerRemote_Blob_SingleChunkOK: meta-less Blob accessor removed.
//     No clean mapping to PrepDownload+Open (the new surface already has coverage
//     via TestFileServerRemote_PrepDownload_UsesMetaChunkSize). Deleted.
//   - TestFileServerRemote_Dir_ReturnsSelf: Dir() removed. Intent re-expressed
//     as TestFileServerRemote_BlobsTagsSelf (Blobs()/Tags() return self, D12).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"testing"

	fsfileserver "github.com/ngicks/go-fsys-helper/stream/fileserver"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy/fileserver"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ---- in-memory file-server Client for testing ----

type fsTestClient struct {
	objects  map[string][]byte
	putNames []string // log of Put calls in order
}

func newFSTestClient() *fsTestClient {
	return &fsTestClient{objects: make(map[string][]byte)}
}

func (m *fsTestClient) Get(
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

func (m *fsTestClient) Stat(_ context.Context, name string) (int64, error) {
	data, ok := m.objects[name]
	if !ok {
		return 0, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return int64(len(data)), nil
}

func (m *fsTestClient) Put(_ context.Context, name string, _ int64, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.objects[name] = data
	m.putNames = append(m.putNames, name)
	return nil
}

// Compile-time assertion: fsTestClient satisfies fsfileserver.Client.
var _ fsfileserver.Client = (*fsTestClient)(nil)

// ---- helpers ----

func newFSRemoteFromCfg(c *fsTestClient, chunkSize int64) *fileServerRemote {
	return NewFileServer(FileServerConfig{
		Client:    c,
		Naming:    fileserver.DefaultNaming{},
		ChunkSize: chunkSize,
	}).(*fileServerRemote)
}

func newFSRemoteReadOnly(c *fsTestClient) *fileServerRemote {
	return NewFileServer(FileServerConfig{
		Client:   c,
		ReadOnly: true,
	}).(*fileServerRemote)
}

// seedBlobFS puts the blob data as a single chunk in the client.
func seedBlobFS(
	t *testing.T,
	m *fsTestClient,
	naming fileserver.NamingConvention,
	data []byte,
) {
	t.Helper()
	dgst := digest.SHA256.FromBytes(data)
	name := naming.BlobChunk(dgst, 0)
	m.objects[name] = data
}

// buildManifest builds a valid OCI image manifest JSON referencing configBytes
// and layerBytes, and returns its serialized form.
func buildManifest(t *testing.T, configBytes, layerBytes []byte) []byte {
	t.Helper()
	cfgDgst := digest.SHA256.FromBytes(configBytes)
	lyrDgst := digest.SHA256.FromBytes(layerBytes)
	// Build compact JSON directly so the byte content is deterministic.
	man := struct {
		SchemaVersion int             `json:"schemaVersion"`
		MediaType     string          `json:"mediaType"`
		Config        v1.Descriptor   `json:"config"`
		Layers        []v1.Descriptor `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     v1.MediaTypeImageManifest,
		Config: v1.Descriptor{
			MediaType: v1.MediaTypeImageConfig,
			Digest:    cfgDgst,
			Size:      int64(len(configBytes)),
		},
		Layers: []v1.Descriptor{
			{
				MediaType: v1.MediaTypeImageLayerGzip,
				Digest:    lyrDgst,
				Size:      int64(len(layerBytes)),
			},
		},
	}
	data, err := json.Marshal(man)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return data
}

// buildIndexJSON returns a minimal index.json pointing at a single manifest.
func buildIndexJSON(t *testing.T, mDgst digest.Digest, mSize int64) []byte {
	t.Helper()
	idx := struct {
		SchemaVersion int             `json:"schemaVersion"`
		MediaType     string          `json:"mediaType"`
		Manifests     []v1.Descriptor `json:"manifests"`
	}{
		SchemaVersion: 2,
		MediaType:     v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{
			{
				MediaType: v1.MediaTypeImageManifest,
				Digest:    mDgst,
				Size:      mSize,
			},
		},
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return data
}

// ---- Tests ----

func TestFileServerRemote_ReadOnly_False(t *testing.T) {
	t.Parallel()
	r := newFSRemoteFromCfg(newFSTestClient(), DefaultChunkSize)
	if r.ReadOnly() {
		t.Error("ReadOnly() = true, want false")
	}
}

func TestFileServerRemote_Close_NoOp(t *testing.T) {
	t.Parallel()
	r := newFSRemoteFromCfg(newFSTestClient(), DefaultChunkSize)
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestFileServerRemote_LoadImage_NoOp(t *testing.T) {
	t.Parallel()
	r := newFSRemoteFromCfg(newFSTestClient(), DefaultChunkSize)
	ref, _ := imageref.Parse("example.com/repo:v1")
	if err := r.LoadImage(context.Background(), ref); err != nil {
		t.Errorf("LoadImage: %v", err)
	}
}

func TestFileServerRemote_DumpImage_NoOp(t *testing.T) {
	t.Parallel()
	r := newFSRemoteFromCfg(newFSTestClient(), DefaultChunkSize)
	ref, _ := imageref.Parse("example.com/repo:v1")
	if err := r.DumpImage(context.Background(), ref); err != nil {
		t.Errorf("DumpImage: %v", err)
	}
}

// TestFileServerRemote_BlobsTagsSelf verifies that Blobs() and Tags() both
// return the remote itself (D12: self-contained store — no indirection).
func TestFileServerRemote_BlobsTagsSelf(t *testing.T) {
	t.Parallel()
	r := newFSRemoteFromCfg(newFSTestClient(), DefaultChunkSize)
	if r.Blobs() != ociimagecopy.BlobStore(r) {
		t.Error("Blobs() did not return self")
	}
	if r.Tags() != ociimagecopy.TagStoreV1(r) {
		t.Error("Tags() did not return self")
	}
}

// TestFileServerRemote_PutIndex_PutOciLayout_AccumulationAndCommit verifies:
//   - Putting oci-layout alone does NOT write the meta.
//   - Putting index.json (the second file) triggers exactly one meta Put.
//   - The meta Put name matches DefaultNaming.ImageMeta(ref).
//   - The meta contains correct descriptors (manifest+config+layer).
func TestFileServerRemote_PutIndex_PutOciLayout_AccumulationAndCommit(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}
	const chunkSize = 1024

	configBytes := []byte(`{"architecture":"amd64"}`)
	layerBytes := []byte("layer data")
	manifestBytes := buildManifest(t, configBytes, layerBytes)

	// Pre-seed blobs (as if pushBlobs already ran).
	seedBlobFS(t, m, naming, configBytes)
	seedBlobFS(t, m, naming, layerBytes)
	seedBlobFS(t, m, naming, manifestBytes)

	manifestDgst := digest.SHA256.FromBytes(manifestBytes)
	idxBytes := buildIndexJSON(t, manifestDgst, int64(len(manifestBytes)))
	layoutBytes := []byte(`{"imageLayoutVersion":"1.0.0"}`)

	r := newFSRemoteFromCfg(m, chunkSize)
	ref, _ := imageref.Parse("example.com/test:v1")

	initialPuts := len(m.putNames)

	// Put oci-layout first — no meta yet.
	if err := r.PutOciLayout(context.Background(), ref, layoutBytes); err != nil {
		t.Fatalf("PutOciLayout: %v", err)
	}
	if len(m.putNames) != initialPuts {
		t.Errorf("meta was written after oci-layout only (got %d puts)", len(m.putNames))
	}

	// Put index.json — should trigger meta commit.
	if err := r.PutIndex(context.Background(), ref, idxBytes); err != nil {
		t.Fatalf("PutIndex: %v", err)
	}

	// Exactly one Put should have happened (the meta).
	if len(m.putNames) != initialPuts+1 {
		t.Fatalf("expected 1 meta Put, got %d total new puts", len(m.putNames)-initialPuts)
	}

	// The meta name should match the naming convention.
	wantMetaName := naming.ImageMeta(ref)
	if m.putNames[initialPuts] != wantMetaName {
		t.Errorf("meta Put name = %q, want %q", m.putNames[initialPuts], wantMetaName)
	}

	// Parse the meta and verify contents.
	rawMeta := m.objects[wantMetaName]
	if rawMeta == nil {
		t.Fatal("meta object not found in client")
	}
	meta, err := fileserver.UnmarshalImageMeta(rawMeta)
	if err != nil {
		t.Fatalf("UnmarshalImageMeta: %v", err)
	}

	if meta.ChunkSize != chunkSize {
		t.Errorf("meta.ChunkSize = %d, want %d", meta.ChunkSize, chunkSize)
	}
	if string(meta.OciLayout) != string(layoutBytes) {
		t.Errorf("meta.OciLayout = %q, want %q", meta.OciLayout, layoutBytes)
	}
	if string(meta.IndexJSON) != string(idxBytes) {
		t.Errorf("meta.IndexJSON = %q, want %q", meta.IndexJSON, idxBytes)
	}

	// Descriptors: manifest + config + layer.
	if len(meta.Descriptors) != 3 {
		t.Fatalf("meta.Descriptors len = %d, want 3", len(meta.Descriptors))
	}
	if meta.Descriptors[0].Digest != manifestDgst.String() {
		t.Errorf("Descriptors[0].Digest = %q, want %q", meta.Descriptors[0].Digest, manifestDgst)
	}
	configDgst := digest.SHA256.FromBytes(configBytes)
	if meta.Descriptors[1].Digest != configDgst.String() {
		t.Errorf("Descriptors[1].Digest = %q, want %q", meta.Descriptors[1].Digest, configDgst)
	}
	layerDgst := digest.SHA256.FromBytes(layerBytes)
	if meta.Descriptors[2].Digest != layerDgst.String() {
		t.Errorf("Descriptors[2].Digest = %q, want %q", meta.Descriptors[2].Digest, layerDgst)
	}
}

// TestFileServerRemote_PutIndex_PutOciLayout_OrderIndependent verifies that
// putting index.json FIRST then oci-layout still triggers the meta commit.
func TestFileServerRemote_PutIndex_PutOciLayout_OrderIndependent(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}

	configBytes := []byte(`{"os":"linux"}`)
	layerBytes := []byte("layer2")
	manifestBytes := buildManifest(t, configBytes, layerBytes)
	seedBlobFS(t, m, naming, configBytes)
	seedBlobFS(t, m, naming, layerBytes)
	seedBlobFS(t, m, naming, manifestBytes)

	manifestDgst := digest.SHA256.FromBytes(manifestBytes)
	idxBytes := buildIndexJSON(t, manifestDgst, int64(len(manifestBytes)))
	layoutBytes := []byte(`{"imageLayoutVersion":"1.0.0"}`)

	r := newFSRemoteFromCfg(m, 1024)
	ref, _ := imageref.Parse("example.com/test:latest")

	// index.json first, then oci-layout.
	if err := r.PutIndex(context.Background(), ref, idxBytes); err != nil {
		t.Fatalf("PutIndex: %v", err)
	}
	if len(m.putNames) != 0 {
		t.Errorf("meta written after index.json only (expected no puts yet)")
	}
	if err := r.PutOciLayout(context.Background(), ref, layoutBytes); err != nil {
		t.Fatalf("PutOciLayout: %v", err)
	}
	if len(m.putNames) != 1 {
		t.Fatalf("expected 1 meta Put, got %d", len(m.putNames))
	}
}

// TestFileServerRemote_PutIndex_PutOciLayout_NoCrossImageLeak verifies that
// two refs accumulate independently and each gets its own meta object.
func TestFileServerRemote_PutIndex_PutOciLayout_NoCrossImageLeak(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}

	buildAndSeedImage := func(tag string) (ref imageref.ImageRef, idxBytes, layoutBytes []byte) {
		cfg := fmt.Appendf(nil, `{"tag":%q}`, tag)
		lyr := fmt.Appendf(nil, "layer-%s", tag)
		man := buildManifest(t, cfg, lyr)
		seedBlobFS(t, m, naming, cfg)
		seedBlobFS(t, m, naming, lyr)
		seedBlobFS(t, m, naming, man)
		mDgst := digest.SHA256.FromBytes(man)
		idx := buildIndexJSON(t, mDgst, int64(len(man)))
		layout := []byte(`{"imageLayoutVersion":"1.0.0"}`)
		r, _ := imageref.Parse(fmt.Sprintf("example.com/repo:%s", tag))
		return r, idx, layout
	}

	ref1, idx1, layout1 := buildAndSeedImage("v1")
	ref2, idx2, layout2 := buildAndSeedImage("v2")

	r := newFSRemoteFromCfg(m, 1024)

	// Interleave: put one file from each ref before completing either.
	_ = r.PutOciLayout(context.Background(), ref1, layout1)
	_ = r.PutOciLayout(context.Background(), ref2, layout2)
	if len(m.putNames) != 0 {
		t.Fatal("meta written too early (after first file for each ref)")
	}

	_ = r.PutIndex(context.Background(), ref1, idx1)
	if len(m.putNames) != 1 {
		t.Fatalf("expected 1 meta after ref1 complete, got %d", len(m.putNames))
	}

	_ = r.PutIndex(context.Background(), ref2, idx2)
	if len(m.putNames) != 2 {
		t.Fatalf("expected 2 metas after both refs complete, got %d", len(m.putNames))
	}

	// Both meta names must be distinct.
	if m.putNames[0] == m.putNames[1] {
		t.Errorf("both metas have same name %q", m.putNames[0])
	}
}

// TestFileServerRemote_ReadOnly_PutOciLayout verifies PutOciLayout returns
// ociimagecopy.ErrReadOnly on a read-only remote.
func TestFileServerRemote_ReadOnly_PutOciLayout(t *testing.T) {
	t.Parallel()
	r := newFSRemoteReadOnly(newFSTestClient())
	ref, _ := imageref.Parse("example.com/repo:v1")
	err := r.PutOciLayout(context.Background(), ref, []byte("{}"))
	if err != ociimagecopy.ErrReadOnly {
		t.Errorf("PutOciLayout on read-only: got %v, want ociimagecopy.ErrReadOnly", err)
	}
}

// TestFileServerRemote_ReadOnly_PrepUpload verifies PrepUpload returns
// ociimagecopy.ErrReadOnly on a read-only remote.
func TestFileServerRemote_ReadOnly_PrepUpload(t *testing.T) {
	t.Parallel()
	r := newFSRemoteReadOnly(newFSTestClient())
	dgst := digest.SHA256.FromBytes([]byte("x"))
	_, err := r.PrepUpload(context.Background(), dgst, 1)
	if err != ociimagecopy.ErrReadOnly {
		t.Errorf("PrepUpload on read-only: got %v, want ociimagecopy.ErrReadOnly", err)
	}
}

// TestFileServerRemote_InspectImage_DigestMath verifies that the raw bytes
// returned by InspectImage have sha256 == the manifest descriptor digest.
func TestFileServerRemote_InspectImage_DigestMath(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}
	const chunkSize = 1024

	configBytes := []byte(`{"architecture":"arm64"}`)
	layerBytes := []byte("inspect-layer")
	manifestBytes := buildManifest(t, configBytes, layerBytes)

	seedBlobFS(t, m, naming, configBytes)
	seedBlobFS(t, m, naming, layerBytes)
	seedBlobFS(t, m, naming, manifestBytes)

	manifestDgst := digest.SHA256.FromBytes(manifestBytes)
	idxBytes := buildIndexJSON(t, manifestDgst, int64(len(manifestBytes)))
	layoutBytes := []byte(`{"imageLayoutVersion":"1.0.0"}`)

	// Write the meta object directly.
	meta := fileserver.ImageMeta{
		Version:   1,
		ChunkSize: chunkSize,
		OciLayout: json.RawMessage(layoutBytes),
		IndexJSON: json.RawMessage(idxBytes),
		Descriptors: []fileserver.MetaDescriptor{
			{Digest: manifestDgst.String(), Size: int64(len(manifestBytes))},
			{Digest: digest.SHA256.FromBytes(configBytes).String(), Size: int64(len(configBytes))},
			{Digest: digest.SHA256.FromBytes(layerBytes).String(), Size: int64(len(layerBytes))},
		},
	}
	metaBytes, _ := fileserver.MarshalImageMeta(meta)
	ref, _ := imageref.Parse("example.com/inspect:v1")
	m.objects[naming.ImageMeta(ref)] = metaBytes

	r := newFSRemoteFromCfg(m, chunkSize)
	got, err := r.InspectImage(context.Background(), ref)
	if err != nil {
		t.Fatalf("InspectImage: %v", err)
	}

	// sha256(returned bytes) == manifest digest.
	gotDgst := digest.SHA256.FromBytes(got)
	if gotDgst != manifestDgst {
		t.Errorf(
			"digest(InspectImage bytes) = %s, want %s",
			gotDgst, manifestDgst,
		)
	}
	if !bytes.Equal(got, manifestBytes) {
		t.Errorf("InspectImage content mismatch:\ngot  %q\nwant %q", got, manifestBytes)
	}
}

// TestFileServerRemote_InspectImage_DigestMismatch verifies that InspectImage
// rejects a manifest blob whose stored bytes do not hash to the descriptor
// digest recorded in the meta (corrupt/tampered blob pool).
func TestFileServerRemote_InspectImage_DigestMismatch(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}
	const chunkSize = 1024

	configBytes := []byte(`{"architecture":"arm64"}`)
	layerBytes := []byte("inspect-layer")
	manifestBytes := buildManifest(t, configBytes, layerBytes)
	manifestDgst := digest.SHA256.FromBytes(manifestBytes)

	// Seed the config + layer normally, but store a CORRUPTED manifest blob
	// under the (correct) manifest chunk name.
	seedBlobFS(t, m, naming, configBytes)
	seedBlobFS(t, m, naming, layerBytes)
	corrupt := append([]byte(nil), manifestBytes...)
	corrupt[len(corrupt)/2] ^= 0xFF
	m.objects[naming.BlobChunk(manifestDgst, 0)] = corrupt

	idxBytes := buildIndexJSON(t, manifestDgst, int64(len(manifestBytes)))
	layoutBytes := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	meta := fileserver.ImageMeta{
		Version:   1,
		ChunkSize: chunkSize,
		OciLayout: json.RawMessage(layoutBytes),
		IndexJSON: json.RawMessage(idxBytes),
		Descriptors: []fileserver.MetaDescriptor{
			{Digest: manifestDgst.String(), Size: int64(len(manifestBytes))},
			{Digest: digest.SHA256.FromBytes(configBytes).String(), Size: int64(len(configBytes))},
			{Digest: digest.SHA256.FromBytes(layerBytes).String(), Size: int64(len(layerBytes))},
		},
	}
	metaBytes, _ := fileserver.MarshalImageMeta(meta)
	ref, _ := imageref.Parse("example.com/inspect:corrupt")
	m.objects[naming.ImageMeta(ref)] = metaBytes

	r := newFSRemoteFromCfg(m, chunkSize)
	_, err := r.InspectImage(context.Background(), ref)
	if !errors.Is(err, ocidir.ErrManifestDigestMismatch) {
		t.Fatalf("expected ErrManifestDigestMismatch, got %v", err)
	}
}

// TestFileServerRemote_ListBlobsByImage_FromMeta verifies that after the meta
// is consulted via a GetIndex/ListBlobsByImage call, the descriptors are
// enumerable via ListBlobsByImage (replaces the deleted global ListBlobs test).
// The count assertion (3 descriptors: manifest+config+layer) is preserved.
func TestFileServerRemote_ListBlobsByImage_FromMeta(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}

	configBytes := []byte(`{}`)
	layerBytes := []byte("lyr")
	manifestBytes := buildManifest(t, configBytes, layerBytes)

	seedBlobFS(t, m, naming, configBytes)
	seedBlobFS(t, m, naming, layerBytes)
	seedBlobFS(t, m, naming, manifestBytes)

	manifestDgst := digest.SHA256.FromBytes(manifestBytes)
	idxBytes := buildIndexJSON(t, manifestDgst, int64(len(manifestBytes)))
	layoutBytes := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	meta := fileserver.ImageMeta{
		Version:   1,
		ChunkSize: 1024,
		OciLayout: json.RawMessage(layoutBytes),
		IndexJSON: json.RawMessage(idxBytes),
		Descriptors: []fileserver.MetaDescriptor{
			{Digest: manifestDgst.String(), Size: int64(len(manifestBytes))},
			{Digest: digest.SHA256.FromBytes(configBytes).String(), Size: int64(len(configBytes))},
			{Digest: digest.SHA256.FromBytes(layerBytes).String(), Size: int64(len(layerBytes))},
		},
	}
	metaBytes, _ := fileserver.MarshalImageMeta(meta)
	ref, _ := imageref.Parse("example.com/lb:v1")
	m.objects[naming.ImageMeta(ref)] = metaBytes

	r := newFSRemoteFromCfg(m, 1024)

	// ListBlobsByImage reads the meta and yields the 3 descriptors.
	var blobDigests []string
	for d, err := range r.ListBlobsByImage(context.Background(), ref) {
		if err != nil {
			t.Fatalf("ListBlobsByImage: %v", err)
		}
		blobDigests = append(blobDigests, string(d))
	}
	if len(blobDigests) != 3 {
		t.Errorf("ListBlobsByImage: got %d blobs, want 3", len(blobDigests))
	}
}

// TestFileServerRemote_ListBlobsByImage_AbsentIsSilent verifies that an absent
// meta (fs.ErrNotExist, e.g. a first push) yields nothing and is NOT an error.
// (Replaces the deleted TestFileServerRemote_PrimeRefs_AbsentIsSilent.)
func TestFileServerRemote_ListBlobsByImage_AbsentIsSilent(t *testing.T) {
	t.Parallel()
	r := newFSRemoteFromCfg(newFSTestClient(), 1024)
	ref, _ := imageref.Parse("example.com/absent:v1")

	count := 0
	for _, err := range r.ListBlobsByImage(context.Background(), ref) {
		if err != nil {
			t.Fatalf("absent meta must be silent, got error: %v", err)
		}
		count++
	}
	if count != 0 {
		t.Errorf("absent meta: expected 0 blobs, got %d", count)
	}
}

// TestFileServerRemote_ListBlobsByImage_PropagatesTransportError verifies that
// a non-ErrNotExist error fetching the meta (e.g. 401/5xx) is yielded by
// ListBlobsByImage rather than silently read as "absent" (D14 intent: swallowing
// it would downgrade a failed probe into "remote has nothing, send everything").
// (Replaces the deleted TestFileServerRemote_PrimeRefs_PropagatesTransportError.)
func TestFileServerRemote_ListBlobsByImage_PropagatesTransportError(t *testing.T) {
	t.Parallel()

	boom := errors.New("HTTP 503 service unavailable")
	r := NewFileServer(FileServerConfig{
		Client: &errClient{err: boom},
		Naming: fileserver.DefaultNaming{},
	}).(*fileServerRemote)

	ref, _ := imageref.Parse("example.com/probe:v1")
	var yielded error
	for _, err := range r.ListBlobsByImage(context.Background(), ref) {
		yielded = err
	}
	if !errors.Is(yielded, boom) {
		t.Fatalf("expected transport error propagated, got %v", yielded)
	}
}

// TestFileServerRemote_ReadOnly_ValidatePush verifies that validatePush
// rejects a read-only remote.
func TestFileServerRemote_ReadOnly_ValidatePush(t *testing.T) {
	t.Parallel()
	r := newFSRemoteReadOnly(newFSTestClient())
	if !r.ReadOnly() {
		t.Error("ReadOnly() = false on read-only remote")
	}
	// validatePush is an unexported function in push.go.
	// We test the observable effect: push.go calls peer.ReadOnly() and
	// returns an error.
}

// seedBlobChunked puts the blob data as multiple chunks of chunkSize in the
// client, using DefaultNaming.
func seedBlobChunked(
	t *testing.T,
	m *fsTestClient,
	naming fileserver.NamingConvention,
	data []byte,
	chunkSize int,
) digest.Digest {
	t.Helper()
	dgst := digest.SHA256.FromBytes(data)
	for i := 0; i*chunkSize < len(data); i++ {
		start := i * chunkSize
		end := min(start+chunkSize, len(data))
		name := naming.BlobChunk(dgst, i)
		m.objects[name] = data[start:end]
	}
	// Also seed chunk 0 if data is empty (zero-size blob).
	if len(data) == 0 {
		m.objects[naming.BlobChunk(dgst, 0)] = []byte{}
	}
	return dgst
}

// buildMetaWithChunkSize builds and stores an ImageMeta for ref in the client
// with the given descriptors and chunkSize.
func buildMetaWithChunkSize(
	t *testing.T,
	m *fsTestClient,
	naming fileserver.NamingConvention,
	ref imageref.ImageRef,
	descs []fileserver.MetaDescriptor,
	chunkSize int64,
) {
	t.Helper()
	layoutBytes := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	// Build a minimal index.json from the first descriptor (manifest).
	var idxBytes []byte
	if len(descs) > 0 {
		mDgst := digest.Digest(descs[0].Digest)
		idxBytes = buildIndexJSON(t, mDgst, descs[0].Size)
	} else {
		idxBytes = []byte(
			`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`,
		)
	}
	meta := fileserver.ImageMeta{
		Version:     1,
		ChunkSize:   chunkSize,
		OciLayout:   json.RawMessage(layoutBytes),
		IndexJSON:   json.RawMessage(idxBytes),
		Descriptors: descs,
	}
	metaBytes, err := fileserver.MarshalImageMeta(meta)
	if err != nil {
		t.Fatalf("MarshalImageMeta: %v", err)
	}
	m.objects[naming.ImageMeta(ref)] = metaBytes
}

// TestFileServerRemote_PrepDownload_UsesMetaChunkSize verifies that PrepDownload
// uses the chunkSize recorded in the meta, not the remote's configured
// chunkSize, when the blob was registered via getImageMeta (replaces the
// deleted TestFileServerRemote_BlobSource_UsesMetaChunkSize).
//
// The test seeds a blob split across multiple chunks of size X (meta chunkSize),
// while the remote is configured with a different chunkSize Y. PrepDownload must
// correctly read all bytes using chunkSize X.
func TestFileServerRemote_PrepDownload_UsesMetaChunkSize(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}

	const metaChunkSize = 3      // chunks of 3 bytes each
	const remoteChunkSize = 1024 // remote configured with a large chunk size

	// Blob data that spans multiple chunks of size metaChunkSize.
	blobData := []byte("hello world!") // 12 bytes → 4 chunks of 3
	dgst := seedBlobChunked(t, m, naming, blobData, metaChunkSize)

	ref, _ := imageref.Parse("example.com/chunked:v1")
	buildMetaWithChunkSize(t, m, naming, ref, []fileserver.MetaDescriptor{
		{Digest: dgst.String(), Size: int64(len(blobData))},
	}, metaChunkSize)

	// Remote is configured with remoteChunkSize (1024), NOT metaChunkSize (3).
	r := newFSRemoteFromCfg(m, remoteChunkSize)

	// Prime the meta: this loads the metaChunkSize into blobsFromMeta.
	_, err := r.getImageMeta(context.Background(), ref)
	if err != nil {
		t.Fatalf("getImageMeta: %v", err)
	}

	// Now PrepDownload should use metaChunkSize (3), not remoteChunkSize (1024).
	src, err := r.PrepDownload(context.Background(), dgst, int64(len(blobData)))
	if err != nil {
		t.Fatalf("PrepDownload: %v", err)
	}

	rc, _, err := src.Open(context.Background(), 0)
	if err != nil {
		t.Fatalf("PrepDownload.Open: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if !bytes.Equal(got, blobData) {
		t.Errorf("PrepDownload content mismatch:\ngot  %q\nwant %q", got, blobData)
	}
}

// TestNewFileServerFromSpec_MalformedHeaderRedacted verifies that a
// malformed --remote-header (no colon, so it may be a bare secret) is reported
// with the value redacted rather than echoed verbatim.
func TestNewFileServerFromSpec_MalformedHeaderRedacted(t *testing.T) {
	t.Parallel()

	secret := "sk-super-secret-token-no-colon"
	u, _ := url.Parse("http://example.com")
	spec := &FileServerRemoteSpec{
		URL:     u,
		Headers: []string{secret},
	}
	_, err := NewFileServerFromSpec(spec)
	if err == nil {
		t.Fatal("expected error for malformed header, got nil")
	}
	if bytes.Contains([]byte(err.Error()), []byte(secret)) {
		t.Errorf("error leaks the raw header value: %q", err.Error())
	}
	if !bytes.Contains([]byte(err.Error()), []byte("redacted")) {
		t.Errorf("error does not mention redaction: %q", err.Error())
	}
}

// errClient is a fsfileserver.Client whose Get/Stat always fail with a fixed
// non-ErrNotExist error, modelling a transport / auth (401/403) / server (5xx)
// failure rather than an absent object.
type errClient struct{ err error }

func (c *errClient) Get(
	_ context.Context, _ string, _ int64,
) (io.ReadCloser, int64, error) {
	return nil, 0, c.err
}

func (c *errClient) Stat(_ context.Context, _ string) (int64, error) {
	return 0, c.err
}

func (c *errClient) Put(_ context.Context, _ string, _ int64, _ io.Reader) error {
	return c.err
}

var _ fsfileserver.Client = (*errClient)(nil)

// TestFileServerRemote_Stat_Complete verifies Stat returns CurrentSize==Size
// when all blob chunks are present on the server. (Replaces the ProbeBlob
// "complete=true" case, per D10.)
func TestFileServerRemote_Stat_Complete(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}

	const chunkSize = int64(4)
	blobData := []byte("abcdefgh") // 8 bytes → 2 chunks of 4

	dgst := seedBlobChunked(t, m, naming, blobData, int(chunkSize))

	ref, _ := imageref.Parse("example.com/probe:v1")
	buildMetaWithChunkSize(t, m, naming, ref, []fileserver.MetaDescriptor{
		{Digest: dgst.String(), Size: int64(len(blobData))},
	}, chunkSize)

	r := newFSRemoteFromCfg(m, 1024) // remote configured with a different chunkSize

	// Prime the meta so blobsFromMeta has the correct chunkSize.
	_, err := r.getImageMeta(context.Background(), ref)
	if err != nil {
		t.Fatalf("getImageMeta: %v", err)
	}

	// All chunks present → complete (CurrentSize == Size).
	info, err := r.Stat(context.Background(), dgst, int64(len(blobData)))
	if err != nil {
		t.Fatalf("Stat (complete): %v", err)
	}
	if info.CurrentSize != info.Size {
		t.Errorf("Stat: complete blob expected CurrentSize==Size, got %d / %d",
			info.CurrentSize, info.Size)
	}
	if info.Size != int64(len(blobData)) {
		t.Errorf("Stat: Size = %d, want %d", info.Size, len(blobData))
	}
}

// TestFileServerRemote_Stat_MissingChunk verifies Stat returns fs.ErrNotExist
// when the blob is entirely absent (offset == 0, !complete). (Replaces the
// ProbeBlob "complete=false" case, per D10.)
func TestFileServerRemote_Stat_MissingChunk(t *testing.T) {
	t.Parallel()

	m := newFSTestClient()
	naming := fileserver.DefaultNaming{}

	const chunkSize = int64(4)
	blobData := []byte("abcdefgh") // 8 bytes → 2 chunks of 4

	dgst := seedBlobChunked(t, m, naming, blobData, int(chunkSize))

	ref, _ := imageref.Parse("example.com/probe:missing")
	buildMetaWithChunkSize(t, m, naming, ref, []fileserver.MetaDescriptor{
		{Digest: dgst.String(), Size: int64(len(blobData))},
	}, chunkSize)

	r := newFSRemoteFromCfg(m, 1024)

	// Prime the meta.
	_, err := r.getImageMeta(context.Background(), ref)
	if err != nil {
		t.Fatalf("getImageMeta: %v", err)
	}

	// Remove chunk 0 → entirely absent → fs.ErrNotExist.
	delete(m.objects, naming.BlobChunk(dgst, 0))
	_, err = r.Stat(context.Background(), dgst, int64(len(blobData)))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat on absent blob: expected fs.ErrNotExist, got %v", err)
	}
}

// TestFileServerRemote_Stat_PropagatesTransportError verifies that a
// transport error from the State probe is surfaced by Stat, not treated as
// incomplete or mapped to ErrNotExist. (Replaces the deleted
// TestFileServerRemote_ProbeBlob_PropagatesTransportError, per D10.)
func TestFileServerRemote_Stat_PropagatesTransportError(t *testing.T) {
	t.Parallel()

	boom := errors.New("HTTP 401 unauthorized")
	r := NewFileServer(FileServerConfig{
		Client: &errClient{err: boom},
		Naming: fileserver.DefaultNaming{},
	}).(*fileServerRemote)

	dgst := digest.SHA256.FromBytes([]byte("x"))
	_, err := r.Stat(context.Background(), dgst, 10)
	if !errors.Is(err, boom) {
		t.Fatalf("expected transport error propagated, got %v", err)
	}
}
