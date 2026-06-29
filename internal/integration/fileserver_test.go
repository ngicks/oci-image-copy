package integration

// fileserver_test.go contains integration tests for the file-server Remote.
//
// Tests use net/http/httptest to spin up an in-process HTTP file server with
// a request-method log. No external services or sshd are required; skopeo is
// still needed for the OCI fixture validation.
//
// Scenarios:
//   (a) TestFileServer_PushMeta — push to empty server; meta is LAST PUT;
//       second push → Sent=0 (all blobs Reused).
//   (b) TestFileServer_MultiChunk — multi-chunk via tiny chunkSize (1 byte).
//   (c) TestFileServer_DryRunPush — --dry-run → ZERO PUT requests, with
//       EXACT Sent/Reused plans (meta GET + HEAD chunk probes) for empty,
//       complete, and partially pushed remotes.
//   (d) TestFileServer_InterruptResume — interrupted push (missing chunk
//       suffix + meta) re-uploads only the missing suffix.
//   (e) TestFileServer_InterruptResumePull — pull into a fresh store
//       (byte-true index.json mirror + skopeo inspect), then mid-chunk
//       resume via a ranged GET with a non-zero offset.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngicks/go-common/contextkey"
	fsfileserver "github.com/ngicks/go-fsys-helper/stream/fileserver"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy/fileserver"
	remotepkg "github.com/ngicks/oci-image-copy/pkg/ociimagecopy/remote"
	godigest "github.com/opencontainers/go-digest"
)

// ────────────────────────────────────────────────────────────────────────────
// HTTP file server for testing
// ────────────────────────────────────────────────────────────────────────────

// httpFileServerHandler simulates a dumb object store supporting GET
// (with Range), HEAD, and PUT. All "METHOD /path" strings are recorded in
// requestLog so tests can assert on them. Range headers on GET requests are
// recorded separately in rangeLog as "/path Range:bytes=N-" strings.
type httpFileServerHandler struct {
	mu         sync.Mutex
	objects    map[string][]byte
	requestLog []string
	rangeLog   []string // "GET /path Range:bytes=N-" style
}

func newHTTPFileServerHandler() *httpFileServerHandler {
	return &httpFileServerHandler{objects: make(map[string][]byte)}
}

func (h *httpFileServerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requestLog = append(h.requestLog, r.Method+" "+r.URL.Path)
	h.mu.Unlock()

	// Unescape path to get the canonical object name.
	name, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.serveGet(w, r, name)
	case http.MethodHead:
		h.serveHead(w, name)
	case http.MethodPut:
		h.servePut(w, r, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *httpFileServerHandler) serveGet(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	h.mu.Lock()
	raw, ok := h.objects[name]
	var data []byte
	if ok {
		data = append([]byte(nil), raw...)
	}
	// Record the Range header if present.
	if rangeHdrVal := r.Header.Get("Range"); rangeHdrVal != "" {
		h.rangeLog = append(h.rangeLog, r.URL.Path+" Range:"+rangeHdrVal)
	}
	h.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	// Parse "bytes=<start>-" or "bytes=<start>-<end>".
	spec := strings.TrimPrefix(rangeHdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	var start int64
	if _, err := fmt.Sscanf(parts[0], "%d", &start); err != nil {
		http.Error(w, "bad range start", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	total := int64(len(data))
	end := total - 1
	if parts[1] != "" {
		if _, err := fmt.Sscanf(parts[1], "%d", &end); err != nil {
			http.Error(w, "bad range end", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	if start > total {
		start = total
	}
	if end >= total {
		end = total - 1
	}
	slice := data[start : end+1]
	w.Header().Set(
		"Content-Range",
		fmt.Sprintf("bytes %d-%d/%d", start, end, total),
	)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(slice)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(slice)
}

func (h *httpFileServerHandler) serveHead(w http.ResponseWriter, name string) {
	h.mu.Lock()
	data, ok := h.objects[name]
	h.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
}

func (h *httpFileServerHandler) servePut(
	w http.ResponseWriter,
	r *http.Request,
	name string,
) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	h.mu.Lock()
	h.objects[name] = data
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// putNames returns the path-unescaped names of all PUT requests, in order.
func (h *httpFileServerHandler) putNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, entry := range h.requestLog {
		after, ok := strings.CutPrefix(entry, "PUT /")
		if !ok {
			continue
		}
		name, _ := url.PathUnescape(after)
		out = append(out, name)
	}
	return out
}

// resetLog clears the request log so a second operation can be measured.
func (h *httpFileServerHandler) resetLog() {
	h.mu.Lock()
	h.requestLog = nil
	h.mu.Unlock()
}

// rangeRequests returns a copy of the rangeLog (recorded Range headers on GET
// requests).
func (h *httpFileServerHandler) rangeRequests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.rangeLog))
	copy(out, h.rangeLog)
	return out
}

// resetRangeLog clears the range log.
func (h *httpFileServerHandler) resetRangeLog() {
	h.mu.Lock()
	h.rangeLog = nil
	h.mu.Unlock()
}

// deleteObject removes the named object from the in-memory store.
func (h *httpFileServerHandler) deleteObject(name string) {
	h.mu.Lock()
	delete(h.objects, name)
	h.mu.Unlock()
}

// getObject returns a copy of the object bytes (nil if absent).
func (h *httpFileServerHandler) getObject(name string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if data, ok := h.objects[name]; ok {
		return append([]byte(nil), data...)
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

// newFSRemote constructs a file-server Remote pointing at srv.
func newFSRemote(
	t *testing.T,
	srv *httptest.Server,
	chunkSize int64,
) ociimagecopy.Remote {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	c := &fsfileserver.HTTPClient{
		BaseURL: u,
		Client:  srv.Client(),
	}
	r := remotepkg.NewFileServer(remotepkg.FileServerConfig{
		Client:    c,
		Naming:    fileserver.DefaultNaming{},
		ChunkSize: chunkSize,
	})
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// discardCtx returns a context with a discard-logger so imagecopy's
// slog calls do not panic on a nil logger.
func discardCtx(t *testing.T) context.Context {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return contextkey.WithSlogLogger(context.Background(), logger)
}

// ────────────────────────────────────────────────────────────────────────────
// Tests
// ────────────────────────────────────────────────────────────────────────────

// TestFileServer_PushMeta verifies:
//   - A fresh push uploads all blobs and writes the meta as the LAST PUT.
//   - The meta parses correctly and contains descriptors.
//   - A second (idempotent) push issues zero non-meta chunk PUTs.
func TestFileServer_PushMeta(t *testing.T) {
	skipUnlessReady(t)

	const tag = "fs-v1.0"
	env := newTestEnv(t, tag)

	handler := newHTTPFileServerHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(discardCtx(t), 60*time.Second)
	defer cancel()

	remote := newFSRemote(t, srv, remotepkg.DefaultChunkSize)
	local := env.makeLocal(ctx)

	res, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if res.FailedCount != 0 {
		for _, rr := range res.Reports {
			if rr.Err != nil {
				t.Logf("push report error: %v", rr.Err)
			}
		}
		t.Fatal("Push reported failures")
	}
	t.Logf("push result: %s", res.Reports[0].SummaryLine())

	naming := fileserver.DefaultNaming{}
	ref, err := imageref.Parse(env.fixture.imageRef)
	if err != nil {
		t.Fatalf("imageref.Parse: %v", err)
	}
	metaName := naming.ImageMeta(ref)

	// Meta object must be present.
	rawMeta := handler.getObject(metaName)
	if rawMeta == nil {
		t.Fatalf("meta object %q not found after push", metaName)
	}

	// Meta must be the LAST PUT.
	puts := handler.putNames()
	if len(puts) == 0 {
		t.Fatal("no PUT requests observed during push")
	}
	if puts[len(puts)-1] != metaName {
		t.Errorf(
			"last PUT = %q, want meta %q\nall PUTs: %v",
			puts[len(puts)-1], metaName, puts,
		)
	}

	// Parse and validate the meta.
	meta, err := fileserver.UnmarshalImageMeta(rawMeta)
	if err != nil {
		t.Fatalf("UnmarshalImageMeta: %v", err)
	}
	if meta.ChunkSize != remotepkg.DefaultChunkSize {
		t.Errorf("meta.ChunkSize = %d, want %d", meta.ChunkSize, remotepkg.DefaultChunkSize)
	}
	// Descriptors: manifest + config + layer = 3 minimum.
	if len(meta.Descriptors) < 3 {
		t.Errorf("meta.Descriptors len = %d, want >= 3", len(meta.Descriptors))
	}

	// Second push: all blobs Reused → zero non-meta chunk PUTs.
	handler.resetLog()
	res2, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if res2.FailedCount != 0 {
		t.Fatal("second Push reported failures")
	}
	t.Logf("second push: %s", res2.Reports[0].SummaryLine())

	puts2 := handler.putNames()
	chunkPuts := 0
	for _, name := range puts2 {
		if name != metaName {
			chunkPuts++
		}
	}
	if chunkPuts != 0 {
		t.Errorf("second push: %d non-meta PUTs, want 0; PUTs: %v", chunkPuts, puts2)
	}
	if res2.Reports[0].Sent != 0 {
		t.Errorf("second push: Sent = %d, want 0", res2.Reports[0].Sent)
	}
}

// TestFileServer_MultiChunk verifies correct behaviour with tiny chunkSize=1.
func TestFileServer_MultiChunk(t *testing.T) {
	skipUnlessReady(t)

	const tag = "fs-multi"
	env := newTestEnv(t, tag)

	handler := newHTTPFileServerHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(discardCtx(t), 120*time.Second)
	defer cancel()

	const chunkSize = int64(1)
	remote := newFSRemote(t, srv, chunkSize)
	local := env.makeLocal(ctx)

	res, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("Push (multi-chunk): %v", err)
	}
	if res.FailedCount != 0 {
		for _, rr := range res.Reports {
			if rr.Err != nil {
				t.Logf("error: %v", rr.Err)
			}
		}
		t.Fatal("Push (multi-chunk) reported failures")
	}
	t.Logf("multi-chunk push: %s", res.Reports[0].SummaryLine())
	stored := readStoredFixture(t, env.localBase, env.fixture)

	naming := fileserver.DefaultNaming{}
	ref, err := imageref.Parse(env.fixture.imageRef)
	if err != nil {
		t.Fatalf("imageref.Parse: %v", err)
	}
	metaName := naming.ImageMeta(ref)
	rawMeta := handler.getObject(metaName)
	if rawMeta == nil {
		t.Fatalf("meta object %q not found", metaName)
	}
	meta, err := fileserver.UnmarshalImageMeta(rawMeta)
	if err != nil {
		t.Fatalf("UnmarshalImageMeta: %v", err)
	}
	if meta.ChunkSize != chunkSize {
		t.Errorf("meta.ChunkSize = %d, want %d", meta.ChunkSize, chunkSize)
	}

	// Layer blob with chunkSize=1 → layerSize chunk PUTs.
	puts := handler.putNames()
	layerDgst := godigest.Digest(stored.layerDigest)
	chunk0 := naming.BlobChunk(layerDgst, 0)
	chunkPrefix := strings.TrimSuffix(chunk0, "00000000")
	layerChunkPUTs := 0
	for _, name := range puts {
		if strings.HasPrefix(name, chunkPrefix) {
			layerChunkPUTs++
		}
	}
	if int64(layerChunkPUTs) != stored.layerSize {
		t.Errorf(
			"layer chunk PUTs = %d, want %d (layer=%d bytes, chunkSize=1)",
			layerChunkPUTs, stored.layerSize, stored.layerSize,
		)
	}
}

// TestFileServer_DryRunPush verifies that --dry-run issues ZERO PUT requests.
func TestFileServer_DryRunPush(t *testing.T) {
	skipUnlessReady(t)

	const tag = "fs-dry"
	env := newTestEnv(t, tag)

	handler := newHTTPFileServerHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(discardCtx(t), 60*time.Second)
	defer cancel()

	remote := newFSRemote(t, srv, remotepkg.DefaultChunkSize)
	local := env.makeLocal(ctx)

	res, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
		DryRun: true,
	}, remote)
	if err != nil {
		t.Fatalf("dry-run Push: %v", err)
	}
	if res.FailedCount != 0 {
		for _, rr := range res.Reports {
			if rr.Err != nil {
				t.Logf("error: %v", rr.Err)
			}
		}
		t.Fatal("dry-run Push reported failures")
	}
	t.Logf("dry-run plan: Sent=%d Reused=%d Bytes=%d",
		res.Reports[0].Sent, res.Reports[0].Reused, res.Reports[0].BytesSent)

	puts := handler.putNames()
	if len(puts) != 0 {
		t.Errorf("dry-run Push: got %d PUTs, want 0. PUTs: %v", len(puts), puts)
	}
	// Empty server: the exact plan is "send everything".
	if res.Reports[0].Sent != 3 || res.Reports[0].Reused != 0 {
		t.Errorf("dry-run plan on empty server: Sent=%d Reused=%d, want Sent=3 Reused=0",
			res.Reports[0].Sent, res.Reports[0].Reused)
	}

	// Real push so the server holds the full image (3 blobs + meta).
	if _, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote); err != nil {
		t.Fatalf("real Push: %v", err)
	}
	stored := readStoredFixture(t, env.localBase, env.fixture)

	// Dry-run against the fully pushed image: exact plan must be all-Reused.
	// Fresh remote so no session state (meta cache) helps; the plan must come
	// from the meta GET (getImageMeta/ListBlobsByImage) + HEAD chunk probes alone.
	remote2 := newFSRemote(t, srv, remotepkg.DefaultChunkSize)
	handler.resetLog()
	res, err = local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
		DryRun: true,
	}, remote2)
	if err != nil {
		t.Fatalf("dry-run Push (complete remote): %v", err)
	}
	if res.Reports[0].Sent != 0 || res.Reports[0].Reused != 3 {
		t.Errorf("dry-run plan on complete remote: Sent=%d Reused=%d, want Sent=0 Reused=3",
			res.Reports[0].Sent, res.Reports[0].Reused)
	}
	if puts := handler.putNames(); len(puts) != 0 {
		t.Errorf("dry-run Push (complete remote): got %d PUTs, want 0. PUTs: %v", len(puts), puts)
	}

	// Simulate an interrupted push: layer chunk and meta missing, config and
	// manifest chunks intact. The exact plan must be Sent=1 (the layer),
	// Reused=2, discovered purely via HEAD probes (meta is gone).
	naming := fileserver.DefaultNaming{}
	layerDgst := godigest.Digest(stored.layerDigest)
	handler.deleteObject(naming.BlobChunk(layerDgst, 0))
	ref, err := imageref.Parse(env.fixture.imageRef)
	if err != nil {
		t.Fatalf("imageref.Parse: %v", err)
	}
	handler.deleteObject(naming.ImageMeta(ref))

	remote3 := newFSRemote(t, srv, remotepkg.DefaultChunkSize)
	handler.resetLog()
	res, err = local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
		DryRun: true,
	}, remote3)
	if err != nil {
		t.Fatalf("dry-run Push (partial remote): %v", err)
	}
	if res.Reports[0].Sent != 1 || res.Reports[0].Reused != 2 {
		t.Errorf("dry-run plan on partial remote: Sent=%d Reused=%d, want Sent=1 Reused=2",
			res.Reports[0].Sent, res.Reports[0].Reused)
	}
	if res.Reports[0].BytesSent != stored.layerSize {
		t.Errorf("dry-run plan on partial remote: BytesSent=%d, want layer size %d",
			res.Reports[0].BytesSent, stored.layerSize)
	}
	if puts := handler.putNames(); len(puts) != 0 {
		t.Errorf("dry-run Push (partial remote): got %d PUTs, want 0. PUTs: %v", len(puts), puts)
	}
}

// TestFileServer_InterruptResume verifies chunk-granular push resume: an
// interrupted push leaves a complete prefix of chunks (chunks upload in
// order; the meta is written last, so it is absent too). Re-pushing must
// upload only the missing suffix, reusing the intact prefix.
func TestFileServer_InterruptResume(t *testing.T) {
	skipUnlessReady(t)

	const tag = "fs-resume"
	env := newTestEnv(t, tag)
	handler := newHTTPFileServerHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(discardCtx(t), 120*time.Second)
	defer cancel()

	// 1-byte chunks so we have multiple chunk objects per blob.
	const chunkSize = int64(1)
	remote := newFSRemote(t, srv, chunkSize)
	local := env.makeLocal(ctx)

	// Initial push: upload everything.
	_, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("initial Push: %v", err)
	}
	stored := readStoredFixture(t, env.localBase, env.fixture)
	if stored.layerSize < 2 {
		t.Skip("layer too small for interrupt-resume test")
	}

	// Simulate an interruption mid-blob: chunks upload sequentially, so an
	// interrupt leaves a missing suffix. Delete the LAST chunk of the layer
	// blob, and the meta (written last, so never present after an interrupt).
	naming := fileserver.DefaultNaming{}
	layerDgst := godigest.Digest(stored.layerDigest)
	lastChunk := int(stored.layerSize/chunkSize) - 1
	lastChunkName := naming.BlobChunk(layerDgst, lastChunk)
	handler.deleteObject(lastChunkName)

	ref, err := imageref.Parse(env.fixture.imageRef)
	if err != nil {
		t.Fatalf("imageref.Parse: %v", err)
	}
	metaName := naming.ImageMeta(ref)
	handler.deleteObject(metaName)

	// Re-push and count what gets re-uploaded.
	handler.resetLog()
	_, err = local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("resume Push: %v", err)
	}

	puts := handler.putNames()

	// The deleted last chunk must have been re-uploaded.
	if !slices.Contains(puts, lastChunkName) {
		t.Errorf("last chunk %q not re-uploaded; PUTs: %v", lastChunkName, puts)
	}

	// The meta must have been re-written (commit marker).
	if !slices.Contains(puts, metaName) {
		t.Errorf("meta %q not re-written; PUTs: %v", metaName, puts)
	}

	// Chunk 0 (intact prefix) must NOT appear in the re-push.
	chunk0Name := naming.BlobChunk(layerDgst, 0)
	if slices.Contains(puts, chunk0Name) {
		t.Errorf("chunk0 %q re-uploaded but should be reused; PUTs: %v", chunk0Name, puts)
	}
}

// TestFileServer_InterruptResumePull verifies mid-chunk pull resume:
// an interrupted pull leaves a truncated local blob; a re-pull issues a
// ranged GET at a non-zero offset and produces the correct final blob.
func TestFileServer_InterruptResumePull(t *testing.T) {
	skipUnlessReady(t)

	const tag = "fs-pull-resume"
	env := newTestEnv(t, tag)

	handler := newHTTPFileServerHandler()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(discardCtx(t), 120*time.Second)
	defer cancel()

	// Use a small chunkSize so the layer has multiple chunks.
	const chunkSize = int64(4)
	remote := newFSRemote(t, srv, chunkSize)
	local := env.makeLocal(ctx)

	// Step 1: Push to the file server.
	_, err := local.Push(ctx, ociimagecopy.PushArgs{
		Images: []string{env.fixture.imageRef},
	}, remote)
	if err != nil {
		t.Fatalf("initial Push: %v", err)
	}
	stored := readStoredFixture(t, env.localBase, env.fixture)
	if stored.layerSize < 8 {
		t.Skip("layer too small for interrupt-resume pull test")
	}

	// Step 2: Pull to a fresh local store.
	pullLocalBase := filepath.Join(env.tmpDir, "pull-resume-local")
	must(t, os.MkdirAll(pullLocalBase, 0o755))

	pullLocal, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   pullLocalBase,
		Transport: skopeo.TransportOci,
		OCIPath:   env.fixture.srcDir,
	})
	if err != nil {
		t.Fatalf("NewLocal for pull: %v", err)
	}

	remote2 := newFSRemote(t, srv, chunkSize)
	pullRes, err := pullLocal.Pull(ctx, ociimagecopy.PullArgs{
		Images:            []string{env.fixture.imageRef},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote2)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	if pullRes.FailedCount != 0 {
		t.Fatalf("first Pull reported failures: %+v", pullRes.Reports)
	}
	t.Logf("first pull: %s", pullRes.Reports[0].SummaryLine())

	// Step 3: Verify pull succeeded and blobs are present.
	_, layerHex, _ := strings.Cut(stored.layerDigest, ":")
	layerBlobPath := filepath.Join(pullLocalBase, "share", "sha256", layerHex)
	if _, err := os.Stat(layerBlobPath); err != nil {
		t.Fatalf("layer blob not present after first pull: %v", err)
	}

	// Step 3b: The mirrored index.json must be byte-identical to the verbatim
	// bytes recorded in the server-side meta (no re-marshaling on pull).
	naming := fileserver.DefaultNaming{}
	ref, err := imageref.Parse(env.fixture.imageRef)
	if err != nil {
		t.Fatalf("imageref.Parse: %v", err)
	}
	rawMeta := handler.getObject(naming.ImageMeta(ref))
	if rawMeta == nil {
		t.Fatal("meta object not found on server")
	}
	meta, err := fileserver.UnmarshalImageMeta(rawMeta)
	if err != nil {
		t.Fatalf("UnmarshalImageMeta: %v", err)
	}
	pullTagDir := filepath.Join(pullLocalBase,
		"localregistry.test", "testimage", "_tags", tag)
	localIndex, err := os.ReadFile(filepath.Join(pullTagDir, "index.json"))
	if err != nil {
		t.Fatalf("read mirrored index.json: %v", err)
	}
	if string(localIndex) != string(meta.IndexJSON) {
		t.Errorf("mirrored index.json differs from meta's verbatim bytes:\nlocal: %s\nmeta:  %s",
			localIndex, meta.IndexJSON)
	}

	// Step 3c: The pulled store passes skopeo inspect (shared blob dir layout).
	localShareDir := filepath.Join(pullLocalBase, "share")
	inspectRef := fmt.Sprintf("oci:%s:%s", pullTagDir, env.fixture.imageRef)
	out, err := exec.Command("skopeo", "inspect",
		"--shared-blob-dir", localShareDir, inspectRef).CombinedOutput()
	if err != nil {
		t.Errorf("skopeo inspect pulled image failed: %v\n%s", err, out)
	}

	// Step 4: Simulate interrupted pull — leave a partial .part file at
	// chunkSize+1 bytes (mid-chunk). fsutil.Pull resumes from a .part file,
	// not from a truncated final file, so we must:
	//   a) remove the final blob file,
	//   b) create a .part file with the truncated bytes,
	//   c) create the .part.etag sidecar with the expected digest.
	truncateSize := chunkSize + 1
	layerContent, err := os.ReadFile(layerBlobPath)
	if err != nil {
		t.Fatalf("read layer blob: %v", err)
	}
	if int64(len(layerContent)) <= truncateSize {
		t.Skipf(
			"layer blob too small (%d bytes) to truncate to %d",
			len(layerContent), truncateSize,
		)
	}
	// Remove the complete final blob.
	if err := os.Remove(layerBlobPath); err != nil {
		t.Fatalf("remove final blob: %v", err)
	}
	// Create the .part file with truncated content.
	partPath := layerBlobPath + ".part"
	if err := os.WriteFile(partPath, layerContent[:truncateSize], 0o644); err != nil {
		t.Fatalf("write .part file: %v", err)
	}
	// Create the .part.etag sidecar with the layer digest (fsutil's resume key).
	if err := os.WriteFile(partPath+".etag", []byte(stored.layerDigest), 0o644); err != nil {
		t.Fatalf("write .part.etag: %v", err)
	}

	// Step 5: Reset range log, then re-pull to the same local store.
	handler.resetRangeLog()

	remote3 := newFSRemote(t, srv, chunkSize)
	pullRes2, err := pullLocal.Pull(ctx, ociimagecopy.PullArgs{
		Images:            []string{env.fixture.imageRef},
		AssumeLocalHasSet: map[string]struct{}{},
	}, remote3)
	if err != nil {
		t.Fatalf("resume Pull: %v", err)
	}
	if pullRes2.FailedCount != 0 {
		t.Fatalf("resume Pull reported failures: %+v", pullRes2.Reports)
	}
	t.Logf("resume pull: %s", pullRes2.Reports[0].SummaryLine())

	// Step 6a: Assert the server received at least one ranged GET with a
	// non-zero offset (mid-chunk resume).
	rangeReqs := handler.rangeRequests()
	t.Logf("range requests: %v", rangeReqs)
	foundNonZeroRange := false
	for _, req := range rangeReqs {
		// Range header format: "/path Range:bytes=N-"
		// Extract N from "bytes=N-" portion.
		if !strings.Contains(req, "Range:bytes=") {
			continue
		}
		spec := strings.TrimPrefix(req[strings.Index(req, "Range:bytes=")+len("Range:bytes="):], "")
		var offset int64
		if n, _ := fmt.Sscanf(spec, "%d", &offset); n == 1 && offset > 0 {
			foundNonZeroRange = true
			t.Logf("found non-zero range GET: %s (offset=%d)", req, offset)
			break
		}
	}
	if !foundNonZeroRange {
		t.Errorf(
			"expected at least one ranged GET with non-zero offset; range requests: %v", rangeReqs,
		)
	}

	// Step 6b: Assert the final local blob has correct sha256.
	finalContent, err := os.ReadFile(layerBlobPath)
	if err != nil {
		t.Fatalf("read final layer blob: %v", err)
	}
	h := sha256.Sum256(finalContent)
	gotDigest := fmt.Sprintf("sha256:%x", h)
	if gotDigest != stored.layerDigest {
		t.Errorf("final blob digest = %s, want %s", gotDigest, stored.layerDigest)
	}
	if len(finalContent) != len(layerContent) {
		t.Errorf("final blob size = %d, want %d", len(finalContent), len(layerContent))
	}
}
