package commands

import (
	"github.com/ngicks/oci-image-copy/internal/userns"
	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

// enterUserNamespace enters a user namespace when one of locs is containers-storage.
// It must run before anything is opened: in the parent process it re-executes the binary with the
// same arguments and exits with the child's status. A failure to start the child also exits with
// status 1 from inside go.podman.io/storage/pkg/unshare; userns.Enter checks the usual causes
// first.
func enterUserNamespace(locs ...ociimagecopy.Location) error {
	for _, l := range locs {
		if l.Kind == ociimagecopy.KindContainersStorage {
			return userns.Enter()
		}
	}
	return nil
}
