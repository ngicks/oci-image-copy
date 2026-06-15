package imagecopy

// Fixtures shared across the orchestrator tests. The OCI parser tests
// own their own copies in pkg/ocidir; these are kept here so the
// imagecopy tests don't reach across package boundaries for
// internal-test data.

// ociManifestFixture uses fake (non-SHA256) digests and sizes.  It is used
// only by enumerate_test.go tests which exercise directory-walking logic and
// do NOT perform actual blob transfers.  The fixture constant must NOT be used
// by transfer tests (push/pull) because the sizes and digests are
// intentionally inconsistent.
const ociManifestFixture = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "size": 1234
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "size": 5
    },
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "size": 6
    }
  ]
}`

// Real blob contents used by transfer tests.  The SHA-256 digests and sizes
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
	// realManifestHex.  It references the three blobs above with correct sizes.
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
