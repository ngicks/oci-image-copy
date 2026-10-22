// Package oci_image_copy is a go:generate anchor for populating
// internal/testdata/ocidir with real OCI image dumps.
package oci_image_copy

// The tag is date-fixed so the dumped blobs stay stable across regenerations.
//go:generate podman pull docker.io/library/ubuntu:resolute-20261006
//go:generate mkdir -p internal/testdata/ocidir
//go:generate podman save --format oci-dir -o internal/testdata/ocidir/ubuntu docker.io/library/ubuntu:resolute-20261006
