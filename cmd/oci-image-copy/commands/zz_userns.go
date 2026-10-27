package commands

import (
	"github.com/ngicks/oci-image-copy/internal/userns"
	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

// enterUserNamespace enters a user namespace when one of locs is containers-storage.
// It must run before anything is opened: in the parent process it re-executes the binary with the
// same arguments and exits with the child's status.
func enterUserNamespace(locs ...ociimagecopy.Location) error {
	for _, l := range locs {
		if l.Kind == ociimagecopy.KindContainersStorage {
			return userns.Enter()
		}
	}
	return nil
}
