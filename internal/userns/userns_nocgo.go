//go:build !linux || !cgo

package userns

import "os"

// Enter returns ErrUnsupported for a non-root caller: go.podman.io/storage/pkg/unshare sets up
// the namespace in a cgo constructor, so a build without cgo cannot enter one.
func Enter() error {
	if os.Geteuid() == 0 {
		return nil
	}
	return ErrUnsupported
}
