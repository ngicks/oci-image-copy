// Package skopeo is a typed wrapper over the skopeo CLI. It does not
// look at flag spellings or the installed skopeo version; runtime
// errors surface via the [Invoker] implementation.
package skopeo

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ngicks/oci-image-copy/pkg/cli"
)

type Transport string

const (
	TransportDir               Transport = "dir"
	TransportContainersStorage Transport = "containers-storage"
	TransportDocker            Transport = "docker"
	TransportDockerArchive     Transport = "docker-archive"
	TransportDockerDaemon      Transport = "docker-daemon"
	TransportOci               Transport = "oci"
)

type TransportRef struct {
	Transport Transport
	// ref for "containers-storage", "docker" and "docker-daemon", path for "dir", "docker-archive"
	// and "oci"
	Arg1 string
	// tag for "oci", optional docker-reference for "docker-archive"
	Arg2 string
}

func (r TransportRef) Format() (string, error) {
	return appendTransportRefTag(r.Transport, r.Arg1, r.Arg2)
}

// appendTransportRefTag appends ref to transport.
// See https://github.com/containers/skopeo/blob/main/docs/skopeo.1.md#image-names
func appendTransportRefTag(transport Transport, arg1, arg2 string) (string, error) {
	if arg1 == "" {
		return "", fmt.Errorf("empty ref: %q:%q:%q", transport, arg1, arg2)
	}
	switch transport {
	case TransportContainersStorage, TransportDir, TransportDockerDaemon:
		// containers-storage:docker-reference
		// dir:path
		// docker-daemon:docker-reference
		return string(transport) + ":" + arg1, nil
	case TransportDocker:
		// docker://docker-reference
		return string(transport) + "://" + arg1, nil
	case TransportDockerArchive:
		// docker-archive:path[:docker-reference]
		if arg2 != "" {
			return string(transport) + ":" + arg1 + ":" + arg2, nil
		}
		return string(transport) + ":" + arg1, nil
	case TransportOci:
		// oci:path:tag
		if arg2 == "" {
			return "", fmt.Errorf("empty tag: %q:%q:%q", transport, arg1, arg2)
		}
		return string(transport) + ":" + arg1 + ":" + arg2, nil
	default:
		return "", fmt.Errorf("unkonwn transport: %q:%q:%q", transport, arg1, arg2)
	}
}

// Skopeo is a typed wrapper over the skopeo CLI. It embeds [cli.Tool] for the
// shared Invoker / Exe / Version plumbing and adds skopeo-specific methods and
// compression settings.
type Skopeo struct {
	cli.Tool

	// CompressionFormat sets `--dest-compress-format <format>` on
	// every copy operation when non-empty. Recognized by skopeo:
	// "gzip", "zstd", "zstd:chunked".
	CompressionFormat string
	// CompressionLevel sets `--dest-compress-level <n>` on every copy
	// operation when non-zero. Range is format-specific; consult
	// skopeo and the underlying compressor for valid values.
	CompressionLevel int
}

// NewSkopeo returns a [Skopeo] driving inv (executable "skopeo").
func NewSkopeo(inv cli.Invoker) *Skopeo {
	return &Skopeo{Tool: cli.Tool{Invoker: inv, DefaultExe: "skopeo"}}
}

// Inspect runs `skopeo inspect` and returns its stdout. When raw is
// true, `--raw` is added so the output is the raw manifest bytes;
// when false, it is the JSON image-inspection report. A non-empty
// sharedBlobDir adds `--shared-blob-dir <dir>` (only meaningful when
// src.Transport == [TransportOci]). extraArgs are appended verbatim
// before the source argument.
func (s *Skopeo) Inspect(
	ctx context.Context,
	src TransportRef,
	raw bool,
	sharedBlobDir string,
	extraArgs ...string,
) ([]byte, error) {
	srcStr, err := src.Format()
	if err != nil {
		return nil, err
	}
	args := []string{"inspect"}
	if raw {
		args = append(args, "--raw")
	}
	if sharedBlobDir != "" {
		args = append(args, "--shared-blob-dir", sharedBlobDir)
	}
	args = append(args, extraArgs...)
	args = append(args, srcStr)
	return s.Output(ctx, args...)
}

// Copy copies src into dst using the shared blob pool at
// sharedBlobDir. Wraps `skopeo copy`. The shared-blob-dir flag is
// applied as `--src-shared-blob-dir` when src.Transport ==
// [TransportOci], or `--dest-shared-blob-dir` when dst.Transport ==
// [TransportOci]; passing a non-empty sharedBlobDir requires exactly
// one side to be OCI.
func (s *Skopeo) Copy(ctx context.Context, src, dst TransportRef, sharedBlobDir string) error {
	srcStr, err := src.Format()
	if err != nil {
		return err
	}
	dstStr, err := dst.Format()
	if err != nil {
		return err
	}

	if srcStr == dstStr {
		return fmt.Errorf("src and dst is same: %q", srcStr)
	}

	args := []string{"copy"}
	args = append(args, s.compressionArgs()...)
	if sharedBlobDir != "" {
		switch {
		case dst.Transport == TransportOci:
			// Destination is OCI: blobs should be deposited into the shared
			// blob pool. This covers both non-oci→oci and oci→oci copies.
			args = append(args, "--dest-shared-blob-dir", sharedBlobDir)
		case src.Transport == TransportOci:
			// Only source is OCI (destination is non-oci): read blobs from
			// the shared pool.
			args = append(args, "--src-shared-blob-dir", sharedBlobDir)
		default:
			return fmt.Errorf(
				"skopeo: sharedBlobDir requires one side to be %q (got src=%q dst=%q)",
				TransportOci,
				src.Transport,
				dst.Transport,
			)
		}
	}
	args = append(args, srcStr, dstStr)
	return s.Run(ctx, args...)
}

func (s *Skopeo) compressionArgs() []string {
	var args []string
	if s.CompressionFormat != "" {
		args = append(args, "--dest-compress-format", s.CompressionFormat)
	}
	if s.CompressionLevel != 0 {
		args = append(args, "--dest-compress-level", strconv.Itoa(s.CompressionLevel))
	}
	return args
}
