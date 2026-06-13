package fileserver

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ngicks/oci-image-copy/pkg/ocidir"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// metaVersion is the only supported version of the per-image metadata format.
const metaVersion = 1

// MetaDescriptor is the minimal blob descriptor embedded in the per-image
// metadata. It carries digest and size only; mediaType is intentionally
// omitted to keep the metadata compact — the consumer reads the blob to
// learn its mediaType.
type MetaDescriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// ImageMeta is the per-image metadata object written as the LAST step of a
// push (the commit marker). It records all information needed for a pull
// without any additional round trips:
//
//   - chunkSize: the chunk size used when uploading blobs for this image, so
//     readers know how to map chunk indices back to byte offsets.
//   - ociLayout: the verbatim oci-layout file bytes (json.RawMessage so the
//     bytes round-trip untouched and digest math over them holds).
//   - indexJSON: the verbatim index.json bytes (same rationale).
//   - descriptors: the full digest closure — manifest + config + layers —
//     so a single meta GET reveals the blob inventory without reading any blob.
//
// Writing the meta last is the crash-consistency invariant: a crash mid-push
// leaves at worst orphan chunk objects, never a meta referencing missing chunks.
type ImageMeta struct {
	Version     int              `json:"version"`
	ChunkSize   int64            `json:"chunkSize"`
	OciLayout   json.RawMessage  `json:"ociLayout"`
	IndexJSON   json.RawMessage  `json:"indexJSON"`
	Descriptors []MetaDescriptor `json:"descriptors"`
}

// MarshalImageMeta serialises m to JSON. Returns an error if the meta
// fails validation.
func MarshalImageMeta(m ImageMeta) ([]byte, error) {
	if err := validateImageMeta(m); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// UnmarshalImageMeta deserialises data into an ImageMeta and validates the
// result.
func UnmarshalImageMeta(data []byte) (ImageMeta, error) {
	var m ImageMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return ImageMeta{}, fmt.Errorf("fileserver meta: unmarshal: %w", err)
	}
	if err := validateImageMeta(m); err != nil {
		return ImageMeta{}, err
	}
	return m, nil
}

func validateImageMeta(m ImageMeta) error {
	if m.Version != metaVersion {
		return fmt.Errorf(
			"fileserver meta: unsupported version %d (want %d)",
			m.Version, metaVersion,
		)
	}
	if m.ChunkSize <= 0 {
		return errors.New("fileserver meta: chunkSize must be > 0")
	}
	if len(m.OciLayout) == 0 {
		return errors.New("fileserver meta: ociLayout is empty")
	}
	if len(m.IndexJSON) == 0 {
		return errors.New("fileserver meta: indexJSON is empty")
	}
	return nil
}

// ParsedImageLayout parses the verbatim OciLayout bytes into a
// v1.ImageLayout, routed through [ocidir.ParseImageLayout] so the
// structural contract is validated at the same choke point as on-disk
// oci-layout reads.
func (m ImageMeta) ParsedImageLayout() (v1.ImageLayout, error) {
	l, err := ocidir.ParseImageLayout(m.OciLayout)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("fileserver meta: parse ociLayout: %w", err)
	}
	return l, nil
}

// ParsedIndex parses the verbatim IndexJSON bytes into a v1.Index, routed
// through [ocidir.ParseIndex] so the non-empty-manifests contract is
// validated at the same choke point as on-disk index reads (a peer
// serving `{"manifests":[]}` is rejected here, not at a later
// `Manifests[0]` panic).
func (m ImageMeta) ParsedIndex() (v1.Index, error) {
	idx, err := ocidir.ParseIndex(m.IndexJSON)
	if err != nil {
		return v1.Index{}, fmt.Errorf("fileserver meta: parse indexJSON: %w", err)
	}
	return idx, nil
}

// DescriptorsFromManifest converts the per-image manifest closure
// (manifest descriptor + parsed manifest) into the []MetaDescriptor slice
// stored in the meta. Order: manifest, config, layers.
func DescriptorsFromManifest(mDesc v1.Descriptor, man v1.Manifest) []MetaDescriptor {
	out := make([]MetaDescriptor, 0, 2+len(man.Layers))
	out = append(out,
		MetaDescriptor{
			Digest: string(mDesc.Digest),
			Size:   mDesc.Size,
		},
		MetaDescriptor{
			Digest: string(man.Config.Digest),
			Size:   man.Config.Size,
		},
	)
	for _, l := range man.Layers {
		out = append(out, MetaDescriptor{
			Digest: string(l.Digest),
			Size:   l.Size,
		})
	}
	return out
}
