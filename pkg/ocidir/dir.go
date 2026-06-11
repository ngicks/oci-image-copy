package ocidir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// DirV1 is an OCI image-layout v1 directory. The methods expose the
// canonical files (`index.json`, `oci-layout`) plus blob lookup.
//
// More methods will be added as the OCI image-spec gains them
// (predeclared blobs, referrers, etc.). Implementations should accept
// missing optional files but return the standard `os.ErrNotExist` for
// any blob that is not present.
type DirV1 interface {
	// Index parses `index.json` and returns the typed [v1.Index].
	Index() (v1.Index, error)
	// ImageLayout parses `oci-layout` and returns the typed [v1.ImageLayout].
	ImageLayout() (v1.ImageLayout, error)
	// Blob returns a reader for the blob with the given digest, seeked
	// to offset. size is the total blob size (not bytes remaining from
	// offset); callers comparing against a descriptor compare
	// descriptor.Size against size, not against bytes consumed from rc.
	// Returns [os.ErrNotExist] when the blob is missing.
	Blob(
		ctx context.Context,
		d digest.Digest,
		offset int64,
	) (rc io.ReadCloser, size int64, err error)
}

var _ DirV1 = (*FsDir)(nil)

// FsDir is a [DirV1] backed by a [vroot.Fs[vroot.File]] rooted at an OCI dir.
// Blobs are read from the spec-default `blobs/<algo>/<hex>` location.
// Use a custom [DirV1] implementation when blobs live elsewhere
// (e.g. skopeo's --shared-blob-dir layout).
type FsDir struct {
	fs vroot.Fs[vroot.File]
}

// NewFsDir returns an [FsDir] reading from fs (rooted at an OCI dir).
func NewFsDir(fs vroot.Fs[vroot.File]) FsDir {
	return FsDir{fs: fs}
}

// Index implements [DirV1].
func (d FsDir) Index() (v1.Index, error) {
	data, err := vroot.ReadFile(d.fs, "index.json")
	if err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: read index.json: %w", err)
	}
	var idx v1.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: parse index.json: %w", err)
	}
	if err := ValidateIndex(idx); err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: %w", err)
	}
	return idx, nil
}

// ImageLayout implements [DirV1].
func (d FsDir) ImageLayout() (v1.ImageLayout, error) {
	data, err := vroot.ReadFile(d.fs, v1.ImageLayoutFile)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: read %s: %w", v1.ImageLayoutFile, err)
	}
	var l v1.ImageLayout
	if err := json.Unmarshal(data, &l); err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: parse %s: %w", v1.ImageLayoutFile, err)
	}
	if err := ValidateImageLayout(l); err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: %w", err)
	}
	return l, nil
}

// Blob implements [DirV1].
func (d FsDir) Blob(
	ctx context.Context,
	dg digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	_ = ctx
	algo, hex, err := SplitDigest(string(dg))
	if err != nil {
		return nil, 0, err
	}
	return OpenSeekedBlob(d.fs, path.Join("blobs", algo, hex), offset)
}

var _ DirV1 = (*SharedFsDir)(nil)

// SharedFsDir pairs a [DirV1] (typically an [FsDir] rooted at the
// dump dir, providing Index + ImageLayout) with a separate
// [vroot.Fs[vroot.File]] rooted at the shared blob pool. It models skopeo's
// `--shared-blob-dir` layout, where index.json + oci-layout live in
// one place and the per-image blobs live elsewhere.
//
// Blob reads `<blobs>/<algo>/<hex>`; Index and ImageLayout delegate
// to the dir field.
type SharedFsDir struct {
	dir   DirV1
	blobs vroot.Fs[vroot.File]
}

// NewSharedFsDir returns a [SharedFsDir] that delegates Index and
// ImageLayout to dir and reads blobs from blobs (rooted at the share
// pool, layout `<algo>/<hex>`).
func NewSharedFsDir(dir DirV1, blobs vroot.Fs[vroot.File]) SharedFsDir {
	return SharedFsDir{dir: dir, blobs: blobs}
}

// Index implements [DirV1].
func (d SharedFsDir) Index() (v1.Index, error) { return d.dir.Index() }

// ImageLayout implements [DirV1].
func (d SharedFsDir) ImageLayout() (v1.ImageLayout, error) { return d.dir.ImageLayout() }

// Blob implements [DirV1] reading from the dedicated blob FS.
func (d SharedFsDir) Blob(
	_ context.Context,
	dg digest.Digest,
	offset int64,
) (io.ReadCloser, int64, error) {
	algo, hex, err := SplitDigest(string(dg))
	if err != nil {
		return nil, 0, err
	}
	return OpenSeekedBlob(d.blobs, path.Join(algo, hex), offset)
}

// OpenSeekedBlob opens relPath on f, stats it for size, and seeks to
// offset. Returns [os.ErrNotExist] when the blob is missing. Helper
// for [DirV1] implementations backed by a [vroot.Fs[vroot.File]].
func OpenSeekedBlob(
	f vroot.Fs[vroot.File],
	relPath string,
	offset int64,
) (io.ReadCloser, int64, error) {
	file, err := f.OpenFile(relPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	size := fi.Size()
	if offset < 0 || offset > size {
		file.Close()
		return nil, 0, fmt.Errorf("ocidir: offset %d out of range for blob size %d", offset, size)
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			return nil, 0, err
		}
	}
	return file, size, nil
}

// ErrMissingManifestBlob is returned by [ReadManifest] when the
// manifest blob referenced by index.json is not present in the dir's
// blob pool.
var ErrMissingManifestBlob = errors.New("ocidir: manifest blob missing from blob pool")

// ReadManifest reads index.json from dir, resolves the (single) manifest
// descriptor, loads the manifest blob from the dir's blob pool, parses
// it, and returns the descriptor (size + digest + mediaType from the
// index) plus the parsed manifest body.
func ReadManifest(ctx context.Context, dir DirV1) (v1.Descriptor, v1.Manifest, error) {
	idx, err := dir.Index()
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, err
	}
	mDesc := idx.Manifests[0]
	if mDesc.Digest == "" {
		return v1.Descriptor{}, v1.Manifest{}, errors.New(
			"ocidir: index.json manifest has no digest",
		)
	}

	rc, _, err := dir.Blob(ctx, mDesc.Digest, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf(
				"%w: digest=%s",
				ErrMissingManifestBlob,
				mDesc.Digest,
			)
		}
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf(
			"ocidir: read manifest blob %s: %w",
			mDesc.Digest,
			err,
		)
	}
	defer rc.Close()

	mData, err := io.ReadAll(rc)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf(
			"ocidir: read manifest blob %s: %w",
			mDesc.Digest,
			err,
		)
	}
	man, err := ParseManifest(mData)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf(
			"ocidir: parse manifest %s: %w",
			mDesc.Digest,
			err,
		)
	}
	return mDesc, man, nil
}

// AllDescriptors returns mDesc + m.Config + m.Layers... — every
// descriptor reachable from a single image manifest. Use this when
// you need the digest set or size map of the closure.
func AllDescriptors(mDesc v1.Descriptor, m v1.Manifest) []v1.Descriptor {
	out := make([]v1.Descriptor, 0, 2+len(m.Layers))
	out = append(out, mDesc, m.Config)
	out = append(out, m.Layers...)
	return out
}
