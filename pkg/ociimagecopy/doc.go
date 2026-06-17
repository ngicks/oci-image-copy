// Package ociimagecopy implements the oci-image-copy service backing the
// binary of the same name. It contains the library code that drives the
// oci-image-copy CLI: OCI image reference parsing, on-disk layout
// management, manifest closure walking, external-process wrappers
// (skopeo / podman / docker), SSH+SFTP-backed remote helpers, the
// resumable transfer engine, peer enumeration, and the push/pull
// orchestration entry points. It also holds the released [Version] and the
// layered [Config].
//
// The OCI store is split by concern into two small consumer interfaces:
// [BlobStore] (the content-addressed blob pool — large, streamed, resumable
// bytes keyed by digest) and [TagStoreV1] (the per-tag index.json / oci-layout
// pointer files — tiny, verbatim bytes). [StoreV1] combines both;
// [NewImageView] bridges them back into an [ocidir.DirV1] for the
// manifest-read choke points.
package ociimagecopy
