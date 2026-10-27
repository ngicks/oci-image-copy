//go:build linux && cgo

package userns

import (
	"os"

	"go.podman.io/storage/pkg/unshare"
)

// Enter re-executes the binary in a new user namespace mapping the caller to root and its
// /etc/subuid and /etc/subgid ranges after it, then exits with the child's status.
// It returns at once for root and inside the re-executed child, whose effective UID is 0.
func Enter() error {
	if os.Geteuid() == 0 {
		return nil
	}
	unshare.MaybeReexecUsingUserNamespace(false)
	return nil
}
