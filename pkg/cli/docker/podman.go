package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/cli"
)

// Podman is a typed wrapper over the podman CLI. Podman is largely
// docker-compatible but `podman image ls --format json` returns a
// JSON array (not NDJSON like docker) and uses Names[] for refs
// instead of Repository/Tag — hence a separate type.
type Podman struct {
	Invoker cli.Invoker
	// Exe is the podman executable name (or path). Empty defaults to
	// "podman".
	Exe string
}

// NewPodman returns a [Podman] driving inv.
func NewPodman(inv cli.Invoker) *Podman { return &Podman{Invoker: inv} }

func (p *Podman) exe() string {
	if p.Exe == "" {
		return "podman"
	}
	return p.Exe
}

// Version returns the trimmed `podman --version` output.
func (p *Podman) Version(ctx context.Context) (string, error) {
	out, err := p.Invoker.Command(ctx, p.exe(), "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// podmanImage is the subset of `podman image ls --format json` we
// need: per-image refs (Names) plus RepoDigests as a fallback for
// dangling images.
type podmanImage struct {
	Id          string   `json:"Id"`
	Names       []string `json:"Names"`
	RepoDigests []string `json:"RepoDigests"`
}

// ImageLs returns the union of all image refs visible to podman.
// Output of `podman image ls --format json` is a JSON array; refs
// come from each image's Names list (RepoTag-style).
func (p *Podman) ImageLs(ctx context.Context) ([]string, error) {
	out, err := p.Invoker.Command(ctx, p.exe(), "image", "ls", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	imgs, err := ParsePodmanImageLs(out)
	if err != nil {
		return nil, err
	}
	return imageRefsFromPodmanList(imgs), nil
}

// ParsePodmanImageLs parses `podman image ls --format json` output.
// Exposed for fixture-based tests.
func ParsePodmanImageLs(out []byte) ([]podmanImage, error) {
	var imgs []podmanImage
	if err := json.Unmarshal(out, &imgs); err != nil {
		return nil, fmt.Errorf("podman: parse image ls json: %w", err)
	}
	return imgs, nil
}

func imageRefsFromPodmanList(imgs []podmanImage) []string {
	seen := map[string]struct{}{}
	var refs []string
	for _, img := range imgs {
		for _, n := range img.Names {
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			refs = append(refs, n)
		}
	}
	return refs
}
