package types

import (
	"fmt"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/docker/reference"
)

// Reference is a parsed ImageSetManifest entry.
type Reference struct {
	Repository string        // normalized, e.g. "docker.io/library/ubuntu"
	Tag        string        // empty when the entry has only a digest
	Digest     digest.Digest // empty when the entry has only a tag
}

// ParseReference accepts repo:tag, repo@digest and repo:tag@digest; a bare repo means :latest.
// Short names are normalized the way podman and docker do, e.g. "ubuntu" becomes
// "docker.io/library/ubuntu".
func ParseReference(s string) (Reference, error) {
	named, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return Reference{}, fmt.Errorf("types: parsing reference %q: %w", s, err)
	}
	var ref Reference
	ref.Repository = named.Name()
	if tagged, ok := named.(reference.Tagged); ok {
		ref.Tag = tagged.Tag()
	}
	if digested, ok := named.(reference.Digested); ok {
		ref.Digest = digested.Digest()
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = "latest"
	}
	return ref, nil
}

func (r Reference) String() string {
	s := r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest.String()
	}
	return s
}
