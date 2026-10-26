package types

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Platform is a builder-notation platform, os/arch[/variant].
//
// Values are kept exactly as given: "linux/arm64" and "linux/arm64/v8" are different platforms,
// because the string becomes a directory in the store tree and must never change meaning.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

// ParsePlatform parses builder notation os/arch[/variant] without normalizing it.
func ParsePlatform(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return Platform{}, fmt.Errorf("types: platform %q: want os/arch[/variant]", s)
	}
	if slices.Contains(parts, "") {
		return Platform{}, fmt.Errorf("types: platform %q: empty component", s)
	}
	p := Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}

// String returns os/arch[/variant]; it is also the leaf's platform path.
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// HostPlatform returns runtime.GOOS / runtime.GOARCH, plus the GOARM variant on arm.
//
// Go reports only its build target, not the CPU's capabilities,
// so an arm64 v9 machine still reports linux/arm64.
func HostPlatform() Platform {
	p := Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	if runtime.GOARCH == "arm" {
		p.Variant = goarmVariant()
	}
	return p
}

func goarmVariant() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "GOARM" {
			// e.g. "7" or "7,softfloat"
			v, _, _ := strings.Cut(s.Value, ",")
			return "v" + v
		}
	}
	return ""
}

// Matches compares os and architecture exactly and variants only when both state one.
func (p Platform) Matches(o ocispec.Platform) bool {
	if p.OS != o.OS || p.Architecture != o.Architecture {
		return false
	}
	return p.Variant == "" || o.Variant == "" || p.Variant == o.Variant
}
