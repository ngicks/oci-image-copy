// Package ocidirtest builds a small synthetic OCI image layout for tests.
package ocidirtest

import (
	"encoding/json"
	"path"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	SingleName = "example.com/single:1"
	MultiName  = "example.com/multi:1"
)

// Image is one platform manifest with its blobs.
type Image struct {
	Manifest ocispec.Descriptor
	Config   ocispec.Descriptor
	Layers   []ocispec.Descriptor
	Image    ocispec.Image
}

// Fixture is an OCI image layout holding:
//   - Single: a manifest named [SingleName]
//   - Multi: an image index named [MultiName] with two platform manifests sharing SharedLayer
//   - Unnamed: a manifest without the ref name annotation
type Fixture struct {
	// Files maps oci-dir relative paths, e.g. "blobs/sha256/<hex>", to their contents.
	Files       map[string][]byte
	SharedLayer ocispec.Descriptor
	Single      Image
	Multi       ocispec.Descriptor
	MultiAmd64  Image
	MultiArm64  Image
	Unnamed     Image
}

func BlobPath(dgst digest.Digest) string {
	return path.Join(ocispec.ImageBlobsDir, dgst.Algorithm().String(), dgst.Encoded())
}

func New() Fixture {
	f := Fixture{Files: map[string][]byte{}}

	put := func(mediaType string, b []byte) ocispec.Descriptor {
		dgst := digest.FromBytes(b)
		f.Files[BlobPath(dgst)] = b
		return ocispec.Descriptor{MediaType: mediaType, Digest: dgst, Size: int64(len(b))}
	}
	putJSON := func(mediaType string, v any) ocispec.Descriptor {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return put(mediaType, b)
	}
	image := func(arch string, layers ...ocispec.Descriptor) Image {
		img := ocispec.Image{
			OS: "linux", Architecture: arch,
			RootFS: ocispec.RootFS{Type: "layers"},
		}
		for _, l := range layers {
			img.RootFS.DiffIDs = append(img.RootFS.DiffIDs, l.Digest)
		}
		config := putJSON(ocispec.MediaTypeImageConfig, img)
		manifest := putJSON(ocispec.MediaTypeImageManifest, ocispec.Manifest{
			SchemaVersion: 2,
			MediaType:     ocispec.MediaTypeImageManifest,
			Config:        config,
			Layers:        layers,
		})
		manifest.Platform = &img.Platform
		return Image{Manifest: manifest, Config: config, Layers: layers, Image: img}
	}
	layer := func(content string) ocispec.Descriptor {
		return put(ocispec.MediaTypeImageLayer, []byte(content))
	}

	f.SharedLayer = layer("shared")
	f.Single = image("amd64", f.SharedLayer, layer("single"))
	f.MultiAmd64 = image("amd64", f.SharedLayer, layer("multi-amd64"))
	f.MultiArm64 = image("arm64", f.SharedLayer, layer("multi-arm64"))
	f.Multi = putJSON(ocispec.MediaTypeImageIndex, ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests:     []ocispec.Descriptor{f.MultiAmd64.Manifest, f.MultiArm64.Manifest},
	})
	f.Unnamed = image("riscv64", layer("unnamed"))

	named := func(d ocispec.Descriptor, name string) ocispec.Descriptor {
		d.Platform = nil
		d.Annotations = map[string]string{ocispec.AnnotationRefName: name}
		return d
	}
	unnamed := f.Unnamed.Manifest
	unnamed.Platform = nil
	index, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			named(f.Single.Manifest, SingleName),
			named(f.Multi, MultiName),
			unnamed,
		},
	})
	if err != nil {
		panic(err)
	}
	f.Files[ocispec.ImageIndexFile] = index
	layout, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		panic(err)
	}
	f.Files[ocispec.ImageLayoutFile] = layout

	return f
}
