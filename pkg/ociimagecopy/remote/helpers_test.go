package remote

// helpers_test.go holds the OCI fixtures and fakes shared by the ssh and
// localdir remote tests in this package. The file-server tests are
// self-contained (they build their own in-memory client and metas).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

// must fails the test if err is non-nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Real blob contents used by the remote tests. The SHA-256 digests and sizes
// are computed from the actual bytes so that fsutil size/digest checks pass.
//
//	sha256([]byte("L1"))  = dffe8596427fc50e8f64654a609af134d45552f18bbecef90b31135a9e7acaa0
//	sha256([]byte("L2"))  = d76354d8457898445bb69e0dc0dc95fb74cc3cf334f8c1859162a16ad0041f8d
//	sha256([]byte("CFG")) = 12cb64fe927b420341b14fc03da3daf69619762c7a92d8505612cdd0309c3347
const (
	realLayer1Content = "L1"
	realLayer2Content = "L2"
	realConfigContent = "CFG"

	realLayer1Hex   = "dffe8596427fc50e8f64654a609af134d45552f18bbecef90b31135a9e7acaa0"
	realLayer2Hex   = "d76354d8457898445bb69e0dc0dc95fb74cc3cf334f8c1859162a16ad0041f8d"
	realConfigHex   = "12cb64fe927b420341b14fc03da3daf69619762c7a92d8505612cdd0309c3347"
	realManifestHex = "ce6f5a3c4d308c1acbe4ad0df9cbb76130685a4bebbc6c0f20c43c6baa091b37"

	// realManifestContent is the JSON-compact manifest whose SHA-256 is
	// realManifestHex. It references the three blobs above with correct sizes.
	realManifestContent = `{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json",` +
		`"digest":"sha256:12cb64fe927b420341b14fc03da3daf69619762c7a92d8505612cdd0309c3347",` +
		`"size":3},` +
		`"layers":[` +
		`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",` +
		`"digest":"sha256:dffe8596427fc50e8f64654a609af134d45552f18bbecef90b31135a9e7acaa0",` +
		`"size":2},` +
		`{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",` +
		`"digest":"sha256:d76354d8457898445bb69e0dc0dc95fb74cc3cf334f8c1859162a16ad0041f8d",` +
		`"size":2}]}`

	// realIndexJSON is a minimal index.json pointing at realManifestContent.
	realIndexJSON = `{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.index.v1+json",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"sha256:ce6f5a3c4d308c1acbe4ad0df9cbb76130685a4bebbc6c0f20c43c6baa091b37",` +
		`"size":549}]}`
)

// seedDump writes a complete dump (tag files + shared blob pool) for the real
// fixture into tagDir / shareDir, mirroring the on-disk store layout. It
// returns the manifest digest.
func seedDump(t *testing.T, tagDir, shareDir string) (manifestDigest string) {
	t.Helper()
	must(t, os.MkdirAll(tagDir, 0o755))
	must(t, os.MkdirAll(filepath.Join(shareDir, "sha256"), 0o755))
	must(
		t,
		os.WriteFile(
			filepath.Join(tagDir, "oci-layout"),
			[]byte(`{"imageLayoutVersion":"1.0.0"}`),
			0o644,
		),
	)
	must(t, os.WriteFile(filepath.Join(tagDir, "index.json"), []byte(realIndexJSON), 0o644))

	manifestDigest = "sha256:" + realManifestHex
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realManifestHex),
			[]byte(realManifestContent),
			0o644,
		),
	)
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realConfigHex),
			[]byte(realConfigContent),
			0o644,
		),
	)
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realLayer1Hex),
			[]byte(realLayer1Content),
			0o644,
		),
	)
	must(
		t,
		os.WriteFile(
			filepath.Join(shareDir, "sha256", realLayer2Hex),
			[]byte(realLayer2Content),
			0o644,
		),
	)
	return manifestDigest
}

// inspectCall records the arguments of a recordingSkopeo.Inspect call.
type inspectCall struct {
	Src skopeo.TransportRef
	Raw bool
}

// recordingSkopeo is a fake [ociimagecopy.SkopeoLike] that records Copy /
// Inspect calls so the argv-construction tests can assert on them without a
// real skopeo binary.
type recordingSkopeo struct {
	versionRet string
	inspectRaw map[string][]byte
	copyTo     func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error
	copyFrom   func(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error

	inspectCount  atomic.Int32
	copyToCount   atomic.Int32
	copyFromCount atomic.Int32

	// lastInspect holds the most recent Inspect call arguments.
	lastInspect inspectCall
}

var _ ociimagecopy.SkopeoLike = (*recordingSkopeo)(nil)

func (s *recordingSkopeo) Version(_ context.Context) (string, error) {
	if s.versionRet == "" {
		return "fake-skopeo", nil
	}
	return s.versionRet, nil
}

func (s *recordingSkopeo) Inspect(
	_ context.Context,
	src skopeo.TransportRef,
	raw bool,
	_ string,
	_ ...string,
) ([]byte, error) {
	s.inspectCount.Add(1)
	s.lastInspect = inspectCall{Src: src, Raw: raw}
	if data, ok := s.inspectRaw[string(src.Transport)+":"+src.Arg1]; ok {
		return data, nil
	}
	return nil, errors.New("no inspect fixture")
}

func (s *recordingSkopeo) Copy(
	ctx context.Context,
	src, dst skopeo.TransportRef,
	sharedBlobDir string,
) error {
	switch {
	case dst.Transport == skopeo.TransportOci:
		s.copyToCount.Add(1)
		if s.copyTo != nil {
			return s.copyTo(ctx, src, dst, sharedBlobDir)
		}
	case src.Transport == skopeo.TransportOci:
		s.copyFromCount.Add(1)
		if s.copyFrom != nil {
			return s.copyFrom(ctx, src, dst, sharedBlobDir)
		}
	}
	return nil
}
