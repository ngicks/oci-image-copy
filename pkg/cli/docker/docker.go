// Package docker holds typed wrappers over the docker and podman
// CLIs. The two binaries are largely API-compatible (podman's
// `image ls --format json` differs in detail from docker's), so the
// wrappers share a package but not a single implementation: separate
// types make the on-disk JSON shape differences explicit.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ngicks/oci-image-copy/pkg/cli"
)

// Docker is a typed wrapper over the docker CLI.
type Docker struct {
	Invoker cli.Invoker
	// Exe is the docker executable name (or path). Empty defaults to
	// "docker".
	Exe string
}

// NewDocker returns a [Docker] driving inv.
func NewDocker(inv cli.Invoker) *Docker { return &Docker{Invoker: inv} }

func (d *Docker) exe() string {
	if d.Exe == "" {
		return "docker"
	}
	return d.Exe
}

// Version returns the trimmed `docker --version` output.
func (d *Docker) Version(ctx context.Context) (string, error) {
	out, err := d.Invoker.Command(ctx, d.exe(), "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerImage is the subset of `docker image ls --format json` (NDJSON)
// that we need: Repository + Tag.
type dockerImage struct {
	ID         string `json:"ID"`
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	Digest     string `json:"Digest"`
}

// dockerInspectImage is the subset of `docker image inspect` (single
// JSON array) we'd need if we ever switched to that strategy.
type dockerInspectImage struct {
	Id       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
}

// ImageLs returns image refs visible to docker. `docker image ls
// --format json` emits one JSON object per line (NDJSON). Refs are
// reconstructed as <Repository>:<Tag>; entries with `<none>`
// repo/tag (Docker's marker for dangling images) are skipped.
func (d *Docker) ImageLs(ctx context.Context) ([]string, error) {
	out, err := d.Invoker.Command(ctx, d.exe(), "image", "ls", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	imgs, err := ParseDockerImageLs(out)
	if err != nil {
		return nil, err
	}
	return imageRefsFromDockerList(imgs), nil
}

// ParseDockerImageLs parses Docker's NDJSON `image ls` output.
func ParseDockerImageLs(out []byte) ([]dockerImage, error) {
	var imgs []dockerImage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var img dockerImage
		if err := dec.Decode(&img); err != nil {
			if errors.Is(err, io.EOF) {
				return imgs, nil
			}
			return nil, fmt.Errorf("docker: parse image ls json: %w", err)
		}
		imgs = append(imgs, img)
	}
}

// ParseDockerImageInspect parses `docker image inspect` output
// (single JSON array). Exposed for fixture-based tests.
func ParseDockerImageInspect(out []byte) ([]dockerInspectImage, error) {
	var imgs []dockerInspectImage
	if err := json.Unmarshal(out, &imgs); err != nil {
		return nil, fmt.Errorf("docker: parse image inspect json: %w", err)
	}
	return imgs, nil
}

func imageRefsFromDockerList(imgs []dockerImage) []string {
	seen := map[string]struct{}{}
	var refs []string
	for _, img := range imgs {
		if img.Repository == "" || img.Repository == "<none>" {
			continue
		}
		if img.Tag == "" || img.Tag == "<none>" {
			continue
		}
		ref := img.Repository + ":" + img.Tag
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

// imageRefsFromDockerInspect flattens RepoTags across the image list,
// deduplicating and skipping `<none>:<none>` markers.
func imageRefsFromDockerInspect(imgs []dockerInspectImage) []string {
	seen := map[string]struct{}{}
	var refs []string
	for _, img := range imgs {
		for _, t := range img.RepoTags {
			if t == "" || t == "<none>:<none>" {
				continue
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			refs = append(refs, t)
		}
	}
	return refs
}
