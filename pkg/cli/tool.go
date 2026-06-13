package cli

import (
	"context"
	"fmt"
	"strings"
)

// Tool is an embeddable base for the typed external-CLI wrappers
// ([github.com/ngicks/oci-image-copy/pkg/cli/docker.Docker],
// [github.com/ngicks/oci-image-copy/pkg/cli/docker.Podman] and
// [github.com/ngicks/oci-image-copy/pkg/cli/skopeo.Skopeo]). It carries the
// [Invoker] and the executable name those wrappers all share, and provides the
// common [Tool.Exe] / [Tool.Version] behavior so each wrapper does not
// re-implement it.
//
// Wrappers embed Tool and add their own typed methods (ImageLs, Copy, …):
//
//	type Docker struct {
//		cli.Tool
//	}
//
// The DefaultExe field is the fallback executable name used when Name is empty
// (e.g. "docker"); it is set by the wrapper's constructor.
type Tool struct {
	// Invoker runs the external command. Required.
	Invoker Invoker
	// Name is the executable name (or path). When empty, [Tool.Exe] returns
	// DefaultExe.
	Name string
	// DefaultExe is the fallback executable name used when Name is empty.
	DefaultExe string
}

// Exe returns the configured executable name, or DefaultExe when Name is empty.
func (t Tool) Exe() string {
	if t.Name == "" {
		return t.DefaultExe
	}
	return t.Name
}

// Version returns the trimmed `<exe> --version` output. The exec error is
// wrapped with operation context at the wrapper boundary so callers see which
// tool failed (parse-free: --version output is returned verbatim, trimmed).
func (t Tool) Version(ctx context.Context) (string, error) {
	out, err := t.Invoker.Command(ctx, t.Exe(), "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", t.Exe(), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Run runs `<exe> <args...>`, discarding stdout, wrapping a non-zero exit with
// operation context (the joined argv) at the wrapper boundary so a bare exec
// error is never returned without saying which command produced it.
func (t Tool) Run(ctx context.Context, args ...string) error {
	if err := t.Invoker.Command(ctx, t.Exe(), args...).Run(); err != nil {
		return fmt.Errorf("%s %s: %w", t.Exe(), strings.Join(args, " "), err)
	}
	return nil
}

// Output runs `<exe> <args...>` and returns captured stdout, wrapping a
// non-zero exit with operation context (the joined argv) at the wrapper
// boundary.
func (t Tool) Output(ctx context.Context, args ...string) ([]byte, error) {
	out, err := t.Invoker.Command(ctx, t.Exe(), args...).Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", t.Exe(), strings.Join(args, " "), err)
	}
	return out, nil
}
