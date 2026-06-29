package ociimagecopy

// spec.go parses the URI-style --local flag value introduced in the CLI flag
// refactor (Goal 3 / Plan 3). The --remote spec parser and its types live in
// package remote (co-located with the remote factories), keeping core free of
// any dependency on package remote.
//
// Grammar (canonical forms):
//
//	--local <local-spec>
//	  containers-storage:          (bare name also accepted: containers-storage)
//	  docker-daemon:               (bare: docker-daemon)
//	  oci:/path/to/dir
//	  docker:                      (push/dump source only; bare: docker)

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
)

// LocalSpec holds the parsed components of a --local flag value.
type LocalSpec struct {
	// Transport is one of the supported local transports.
	Transport skopeo.Transport
	// Path is the OCI layout dir (only for TransportOci).
	Path string
}

// ParseLocalSpec parses a --local flag value into a [LocalSpec].
//
// Accepted forms (canonical form has the trailing colon; bare form also works):
//
//	containers-storage:    (or: containers-storage)
//	docker-daemon:         (or: docker-daemon)
//	oci:/path/to/dir
//	docker:                (or: docker) — push/dump source only; pull validation rejects it
func ParseLocalSpec(s string) (LocalSpec, error) {
	if s == "" {
		return LocalSpec{}, errors.New("--local: empty spec")
	}

	// oci: requires a path argument.
	if p, ok := strings.CutPrefix(s, "oci:"); ok {
		if p == "" {
			return LocalSpec{}, errors.New(
				"--local oci: requires a path (e.g. oci:/path/to/dir)",
			)
		}
		return LocalSpec{Transport: skopeo.TransportOci, Path: p}, nil
	}

	// docker: prefix (or bare "docker")
	if s == "docker" || s == "docker:" {
		return LocalSpec{Transport: skopeo.TransportDocker}, nil
	}

	// containers-storage: (with or without trailing colon)
	if s == "containers-storage" || s == "containers-storage:" {
		return LocalSpec{Transport: skopeo.TransportContainersStorage}, nil
	}

	// docker-daemon: (with or without trailing colon)
	if s == "docker-daemon" || s == "docker-daemon:" {
		return LocalSpec{Transport: skopeo.TransportDockerDaemon}, nil
	}

	// Unknown spec.
	return LocalSpec{}, fmt.Errorf(
		"--local: unrecognised spec %q "+
			"(accepted forms: containers-storage:, docker-daemon:, oci:/path, docker:)",
		s,
	)
}
