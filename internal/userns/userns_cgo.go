//go:build linux && cgo

package userns

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"

	"go.podman.io/storage/pkg/unshare"
)

// Enter re-executes the binary in a new user namespace mapping the caller to root and its
// /etc/subuid and /etc/subgid ranges after it, then exits with the child's status.
// It returns at once for root and inside the re-executed child, whose effective UID is 0.
func Enter() error {
	if os.Geteuid() == 0 {
		return nil
	}
	if err := preflight(); err != nil {
		return err
	}
	unshare.MaybeReexecUsingUserNamespace(false)
	return nil
}

// preflight checks what MaybeReexecUsingUserNamespace would otherwise report by exiting with
// status 1, which reads as a usage error. A missing subordinate range is not checked: upstream
// then falls back to mapping the caller alone, as podman does.
func preflight() error {
	if _, err := user.Current(); err != nil {
		return fmt.Errorf("userns: determining the current user: %w", err)
	}
	for _, tool := range []string{"newuidmap", "newgidmap"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("userns: %s is needed for a rootless user namespace: %w", tool, err)
		}
	}
	return nil
}
