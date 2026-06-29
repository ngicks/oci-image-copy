package ocidir

import (
	"context"
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

// RawAccessor is an optional interface that DirV1 implementations may
// satisfy to return verbatim raw bytes for index.json and oci-layout,
// bypassing re-marshalling. Used by pull to mirror tag files byte-identically.
type RawAccessor interface {
	RawIndex() ([]byte, error)
	RawImageLayout() ([]byte, error)
}

// Compile-time assertion: FsDir must implement RawAccessor.
var _ RawAccessor = FsDir{}

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
	idx, err := ParseIndex(data)
	if err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: parse index.json: %w", err)
	}
	return idx, nil
}

// ImageLayout implements [DirV1].
func (d FsDir) ImageLayout() (v1.ImageLayout, error) {
	data, err := vroot.ReadFile(d.fs, v1.ImageLayoutFile)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: read %s: %w", v1.ImageLayoutFile, err)
	}
	l, err := ParseImageLayout(data)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: parse %s: %w", v1.ImageLayoutFile, err)
	}
	return l, nil
}

// RawIndex implements [RawAccessor].
// Returns the verbatim raw bytes of index.json without re-marshalling.
func (d FsDir) RawIndex() ([]byte, error) {
	return vroot.ReadFile(d.fs, "index.json")
}

// RawImageLayout implements [RawAccessor].
// Returns the verbatim raw bytes of oci-layout without re-marshalling.
func (d FsDir) RawImageLayout() ([]byte, error) {
	return vroot.ReadFile(d.fs, v1.ImageLayoutFile)
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

// RawIndex implements [RawAccessor] by delegating to the inner DirV1.
// Returns an error if the inner DirV1 does not implement RawAccessor.
func (d SharedFsDir) RawIndex() ([]byte, error) {
	if raw, ok := d.dir.(RawAccessor); ok {
		return raw.RawIndex()
	}
	return nil, fmt.Errorf("ocidir: SharedFsDir: inner DirV1 does not implement RawAccessor")
}

// RawImageLayout implements [RawAccessor] by delegating to the inner DirV1.
// Returns an error if the inner DirV1 does not implement RawAccessor.
func (d SharedFsDir) RawImageLayout() ([]byte, error) {
	if raw, ok := d.dir.(RawAccessor); ok {
		return raw.RawImageLayout()
	}
	return nil, fmt.Errorf("ocidir: SharedFsDir: inner DirV1 does not implement RawAccessor")
}

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

// ErrManifestDigestMismatch is returned by [ReadManifest] (and
// [VerifyBlobBytes]) when the bytes read for a content-addressed blob do
// not hash to the digest that named them. It signals a corrupt or
// tampered blob pool: every downstream digest (config, layers) is taken
// from the manifest body, so a manifest that does not match its own
// descriptor digest cannot be trusted as the root of that closure.
var ErrManifestDigestMismatch = errors.New("ocidir: manifest digest mismatch")

// VerifyBlobBytes checks that data hashes to want, using want's own
// algorithm via the go-digest [digest.Digest.Verifier] API. It returns
// [ErrManifestDigestMismatch] (wrapped, with the expected digest) when
// the content does not match, an error when want is malformed, and nil
// when the bytes verify.
//
// This is the shared trust-root check for content-addressed reads: a
// blob read by digest is only trustworthy once its bytes are confirmed
// to hash back to that digest.
func VerifyBlobBytes(want digest.Digest, data []byte) error {
	if err := want.Validate(); err != nil {
		return fmt.Errorf("ocidir: verify blob: malformed digest %q: %w", want, err)
	}
	v := want.Verifier()
	if _, err := v.Write(data); err != nil {
		return fmt.Errorf("ocidir: verify blob %s: %w", want, err)
	}
	if !v.Verified() {
		return fmt.Errorf(
			"%w: descriptor=%s actual=%s",
			ErrManifestDigestMismatch,
			want,
			want.Algorithm().FromBytes(data),
		)
	}
	return nil
}

// ErrEmptyIndex is returned by [ReadManifest] when index.json has no
// manifest entries.
var ErrEmptyIndex = errors.New("ocidir: index.json has no manifests")

// ErrMultiManifestIndex is returned by [ReadManifest] when index.json
// lists more than one manifest. This tool implements the single-manifest
// (no multi-arch fan-out) contract documented in the README; erroring is
// honest, silently taking entry [0] is not (per plan 01 decision D12).
var ErrMultiManifestIndex = errors.New(
	"ocidir: index.json lists multiple manifests; single-manifest images only (no multi-arch)",
)

// ErrNestedIndex is returned by [ReadManifest] when the single index
// entry is itself an image-index / manifest-list rather than an image
// manifest. Nested indexes (a manifest-list pointed at by index.json)
// imply multi-arch fan-out, which is unsupported.
var ErrNestedIndex = errors.New(
	"ocidir: index.json entry is a nested image-index/manifest-list; single image manifest only",
)

// ReadRawManifest reads index.json from dir, resolves the single manifest
// descriptor under the single-manifest contract, loads the manifest blob
// from the dir's blob pool, and returns the descriptor plus the verified
// raw manifest bytes (sha256(returned bytes) == mDesc.Digest).
//
// It is the single index-walk + manifest-read choke point: [ReadManifest]
// parses on top of it, and the InspectImage paths (which must return the
// raw bytes so digest math over them holds) use it directly instead of
// re-implementing the walk.
//
// The single-manifest contract is enforced explicitly: an empty index
// ([ErrEmptyIndex]), a multi-manifest index ([ErrMultiManifestIndex]),
// or a single entry whose mediaType is an image-index / manifest-list
// ([ErrNestedIndex]) are all rejected rather than silently resolving to
// `Manifests[0]`. Multi-arch fan-out is not supported (see the README).
func ReadRawManifest(ctx context.Context, dir DirV1) (v1.Descriptor, []byte, error) {
	idx, err := dir.Index()
	if err != nil {
		return v1.Descriptor{}, nil, err
	}
	switch len(idx.Manifests) {
	case 0:
		return v1.Descriptor{}, nil, ErrEmptyIndex
	case 1:
		// ok
	default:
		return v1.Descriptor{}, nil, fmt.Errorf(
			"%w: found %d", ErrMultiManifestIndex, len(idx.Manifests),
		)
	}
	mDesc := idx.Manifests[0]
	switch mDesc.MediaType {
	case v1.MediaTypeImageIndex, MediaTypeDockerList:
		return v1.Descriptor{}, nil, fmt.Errorf(
			"%w: mediaType=%s", ErrNestedIndex, mDesc.MediaType,
		)
	}
	if mDesc.Digest == "" {
		return v1.Descriptor{}, nil, errors.New(
			"ocidir: index.json manifest has no digest",
		)
	}

	rc, _, err := dir.Blob(ctx, mDesc.Digest, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v1.Descriptor{}, nil, fmt.Errorf(
				"%w: digest=%s",
				ErrMissingManifestBlob,
				mDesc.Digest,
			)
		}
		return v1.Descriptor{}, nil, fmt.Errorf(
			"ocidir: read manifest blob %s: %w",
			mDesc.Digest,
			err,
		)
	}
	defer rc.Close()

	mData, err := io.ReadAll(rc)
	if err != nil {
		return v1.Descriptor{}, nil, fmt.Errorf(
			"ocidir: read manifest blob %s: %w",
			mDesc.Digest,
			err,
		)
	}
	// Verify the manifest bytes hash back to the descriptor digest before
	// trusting any digest derived from them. The manifest is the one
	// blindly-trusted content-addressed read in the pipeline: every
	// downstream config/layer digest comes out of this body, so a
	// corrupt or tampered manifest must be rejected here.
	if err := VerifyBlobBytes(mDesc.Digest, mData); err != nil {
		return v1.Descriptor{}, nil, err
	}
	return mDesc, mData, nil
}

// ReadManifest reads index.json from dir, resolves the single manifest
// descriptor, loads the manifest blob from the dir's blob pool, verifies
// and parses it, and returns the descriptor (size + digest + mediaType
// from the index) plus the parsed manifest body. See [ReadRawManifest]
// for the single-manifest-contract semantics it shares.
func ReadManifest(ctx context.Context, dir DirV1) (v1.Descriptor, v1.Manifest, error) {
	mDesc, mData, err := ReadRawManifest(ctx, dir)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, err
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
