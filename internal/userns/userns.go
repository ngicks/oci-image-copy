// Package userns puts a non-root process into a user namespace before it opens
// containers-storage, the way podman does for rootless users.
package userns

import "errors"

// ErrUnsupported reports that this build cannot create the user namespace a non-root user needs.
var ErrUnsupported = errors.New(
	"userns: a non-root user needs a user namespace for containers-storage, " +
		"which only a cgo build on Linux can create; use the dynamic build or run as root",
)
