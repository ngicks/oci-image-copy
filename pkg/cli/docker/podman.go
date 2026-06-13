package docker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ngicks/oci-image-copy/pkg/cli"
)

// Podman is a typed wrapper over the podman CLI. Podman is largely
// docker-compatible but `podman image ls --format json` returns a
// JSON array (not NDJSON like docker) and uses Names[] for refs
// instead of Repository/Tag — hence a separate type. It embeds [cli.Tool]
// for the shared Invoker / Exe / Version plumbing.
type Podman struct {
	cli.Tool
}

// NewPodman returns a [Podman] driving inv (executable "podman").
func NewPodman(inv cli.Invoker) *Podman {
	return &Podman{Tool: cli.Tool{Invoker: inv, DefaultExe: "podman"}}
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
	out, err := p.Output(ctx, "image", "ls", "--format", "json")
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
