package fileserver_test

import (
	"encoding/json"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/imagecopy/fileserver"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// verbatimOciLayout and verbatimIndexJSON are raw byte slices that must
// round-trip untouched through ImageMeta marshal/unmarshal.
var (
	verbatimOciLayout = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	verbatimIndexJSON = []byte(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",` +
			`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
			`"digest":"sha256:abc123","size":42}]}`,
	)
)

func TestMarshalUnmarshalImageMeta_RoundTrip(t *testing.T) {
	t.Parallel()

	meta := fileserver.ImageMeta{
		Version:   1,
		ChunkSize: 1024 * 1024,
		OciLayout: json.RawMessage(verbatimOciLayout),
		IndexJSON: json.RawMessage(verbatimIndexJSON),
		Descriptors: []fileserver.MetaDescriptor{
			{Digest: "sha256:abc", Size: 100},
			{Digest: "sha256:def", Size: 200},
		},
	}

	data, err := fileserver.MarshalImageMeta(meta)
	if err != nil {
		t.Fatalf("MarshalImageMeta: %v", err)
	}

	got, err := fileserver.UnmarshalImageMeta(data)
	if err != nil {
		t.Fatalf("UnmarshalImageMeta: %v", err)
	}

	if got.Version != meta.Version {
		t.Errorf("Version = %d, want %d", got.Version, meta.Version)
	}
	if got.ChunkSize != meta.ChunkSize {
		t.Errorf("ChunkSize = %d, want %d", got.ChunkSize, meta.ChunkSize)
	}

	// Verbatim round-trip: raw bytes must be byte-identical.
	if string(got.OciLayout) != string(verbatimOciLayout) {
		t.Errorf("OciLayout = %q, want %q", got.OciLayout, verbatimOciLayout)
	}
	if string(got.IndexJSON) != string(verbatimIndexJSON) {
		t.Errorf("IndexJSON = %q, want %q", got.IndexJSON, verbatimIndexJSON)
	}

	if len(got.Descriptors) != 2 {
		t.Fatalf("Descriptors len = %d, want 2", len(got.Descriptors))
	}
	if got.Descriptors[0].Digest != "sha256:abc" || got.Descriptors[0].Size != 100 {
		t.Errorf("Descriptors[0] = %+v", got.Descriptors[0])
	}
}

func TestUnmarshalImageMeta_InvalidVersion(t *testing.T) {
	t.Parallel()

	meta := fileserver.ImageMeta{
		Version:   999,
		ChunkSize: 1024,
		OciLayout: json.RawMessage(verbatimOciLayout),
		IndexJSON: json.RawMessage(verbatimIndexJSON),
	}
	data, _ := json.Marshal(meta)
	_, err := fileserver.UnmarshalImageMeta(data)
	if err == nil {
		t.Error("expected error for invalid version, got nil")
	}
}

func TestUnmarshalImageMeta_ZeroChunkSize(t *testing.T) {
	t.Parallel()

	meta := fileserver.ImageMeta{
		Version:   1,
		ChunkSize: 0,
		OciLayout: json.RawMessage(verbatimOciLayout),
		IndexJSON: json.RawMessage(verbatimIndexJSON),
	}
	data, _ := json.Marshal(meta)
	_, err := fileserver.UnmarshalImageMeta(data)
	if err == nil {
		t.Error("expected error for zero chunkSize, got nil")
	}
}

func TestDescriptorsFromManifest(t *testing.T) {
	t.Parallel()

	mDesc := v1.Descriptor{
		Digest: "sha256:manifest",
		Size:   500,
	}
	man := v1.Manifest{
		Config: v1.Descriptor{Digest: "sha256:config", Size: 100},
		Layers: []v1.Descriptor{
			{Digest: "sha256:layer1", Size: 200},
			{Digest: "sha256:layer2", Size: 300},
		},
	}

	descs := fileserver.DescriptorsFromManifest(mDesc, man)

	if len(descs) != 4 {
		t.Fatalf("len = %d, want 4 (manifest+config+2 layers)", len(descs))
	}
	if descs[0].Digest != "sha256:manifest" {
		t.Errorf("descs[0].Digest = %q, want sha256:manifest", descs[0].Digest)
	}
	if descs[1].Digest != "sha256:config" {
		t.Errorf("descs[1].Digest = %q, want sha256:config", descs[1].Digest)
	}
	if descs[2].Digest != "sha256:layer1" {
		t.Errorf("descs[2].Digest = %q, want sha256:layer1", descs[2].Digest)
	}
	if descs[3].Digest != "sha256:layer2" {
		t.Errorf("descs[3].Digest = %q, want sha256:layer2", descs[3].Digest)
	}
}
