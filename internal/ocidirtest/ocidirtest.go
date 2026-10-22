// Package ocidirtest builds a small synthetic OCI image layout for tests.
package ocidirtest

import (
	"encoding/json"
	"path"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	SingleName     = "example.com/single:1"
	MultiName      = "example.com/multi:1"
	DockerName     = "example.com/docker:1"
	DockerListName = "example.com/docker-list:1"
)

// Docker media types, as kept by `skopeo copy --preserve-digests docker://... oci:...`.
const (
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerConfig       = "application/vnd.docker.container.image.v1+json"
	MediaTypeDockerLayerGzip    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

type mediaTypes struct {
	index, manifest, config, layer string
}

var (
	ociTypes = mediaTypes{
		ocispec.MediaTypeImageIndex,
		ocispec.MediaTypeImageManifest,
		ocispec.MediaTypeImageConfig,
		ocispec.MediaTypeImageLayer,
	}
	dockerTypes = mediaTypes{
		MediaTypeDockerManifestList,
		MediaTypeDockerManifest,
		MediaTypeDockerConfig,
		MediaTypeDockerLayerGzip,
	}
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
//   - Docker: a Docker v2 manifest named [DockerName]
//   - DockerList: a Docker manifest list named [DockerListName] with one manifest
type Fixture struct {
	// Files maps oci-dir relative paths, e.g. "blobs/sha256/<hex>", to their contents.
	Files       map[string][]byte
	SharedLayer ocispec.Descriptor
	Single      Image
	Multi       ocispec.Descriptor
	MultiAmd64  Image
	MultiArm64  Image
	Unnamed     Image
	Docker      Image
	DockerList  ocispec.Descriptor
	DockerAmd64 Image
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
	image := func(mt mediaTypes, arch string, layers ...ocispec.Descriptor) Image {
		img := ocispec.Image{
			OS: "linux", Architecture: arch,
			RootFS: ocispec.RootFS{Type: "layers"},
		}
		for _, l := range layers {
			img.RootFS.DiffIDs = append(img.RootFS.DiffIDs, l.Digest)
		}
		config := putJSON(mt.config, img)
		// A Docker v2 manifest has the same JSON shape as an OCI manifest.
		manifest := putJSON(mt.manifest, ocispec.Manifest{
			SchemaVersion: 2,
			MediaType:     mt.manifest,
			Config:        config,
			Layers:        layers,
		})
		manifest.Platform = &img.Platform
		return Image{Manifest: manifest, Config: config, Layers: layers, Image: img}
	}
	layer := func(mt mediaTypes, content string) ocispec.Descriptor {
		return put(mt.layer, []byte(content))
	}
	index := func(mt mediaTypes, manifests ...ocispec.Descriptor) ocispec.Descriptor {
		return putJSON(mt.index, ocispec.Index{
			SchemaVersion: 2,
			MediaType:     mt.index,
			Manifests:     manifests,
		})
	}

	f.SharedLayer = layer(ociTypes, "shared")
	f.Single = image(ociTypes, "amd64", f.SharedLayer, layer(ociTypes, "single"))
	f.MultiAmd64 = image(ociTypes, "amd64", f.SharedLayer, layer(ociTypes, "multi-amd64"))
	f.MultiArm64 = image(ociTypes, "arm64", f.SharedLayer, layer(ociTypes, "multi-arm64"))
	f.Multi = index(ociTypes, f.MultiAmd64.Manifest, f.MultiArm64.Manifest)
	f.Unnamed = image(ociTypes, "riscv64", layer(ociTypes, "unnamed"))
	f.Docker = image(dockerTypes, "amd64", layer(dockerTypes, "docker"))
	f.DockerAmd64 = image(dockerTypes, "amd64", layer(dockerTypes, "docker-list-amd64"))
	f.DockerList = index(dockerTypes, f.DockerAmd64.Manifest)

	named := func(d ocispec.Descriptor, name string) ocispec.Descriptor {
		d.Platform = nil
		d.Annotations = map[string]string{ocispec.AnnotationRefName: name}
		return d
	}
	unnamed := f.Unnamed.Manifest
	unnamed.Platform = nil
	indexJSON, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			named(f.Single.Manifest, SingleName),
			named(f.Multi, MultiName),
			unnamed,
			named(f.Docker.Manifest, DockerName),
			named(f.DockerList, DockerListName),
		},
	})
	if err != nil {
		panic(err)
	}
	f.Files[ocispec.ImageIndexFile] = indexJSON
	layout, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		panic(err)
	}
	f.Files[ocispec.ImageLayoutFile] = layout

	return f
}
