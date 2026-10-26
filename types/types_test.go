package types_test

import (
	"encoding/json"
	"reflect"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/types"
)

const (
	ubuntuIndex    = "sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944"
	ubuntuManifest = "sha256:42527649dbc4f9197a6073f426f488cbff0e9cc9f2afce32d91c383e4875634a"
)

func TestImageSetManifest_json(t *testing.T) {
	var m types.ImageSetManifest
	if err := json.Unmarshal([]byte(`{"images":["ubuntu:26.04","busybox"]}`), &m); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ubuntu:26.04", "busybox"}; !reflect.DeepEqual(m.Images, want) {
		t.Errorf("Images = %v, want %v", m.Images, want)
	}
}

func TestParseReference(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want types.Reference
		str  string
	}{
		{
			"docker.io/library/ubuntu:26.04",
			types.Reference{Repository: "docker.io/library/ubuntu", Tag: "26.04"},
			"docker.io/library/ubuntu:26.04",
		},
		{
			"ubuntu",
			types.Reference{Repository: "docker.io/library/ubuntu", Tag: "latest"},
			"docker.io/library/ubuntu:latest",
		},
		{
			"ubuntu@" + ubuntuIndex,
			types.Reference{Repository: "docker.io/library/ubuntu", Digest: ubuntuIndex},
			"docker.io/library/ubuntu@" + ubuntuIndex,
		},
		{
			"ubuntu:26.04@" + ubuntuManifest,
			types.Reference{Repository: "docker.io/library/ubuntu", Tag: "26.04", Digest: ubuntuManifest},
			"docker.io/library/ubuntu:26.04@" + ubuntuManifest,
		},
		{
			"localhost:5000/team/app:v1",
			types.Reference{Repository: "localhost:5000/team/app", Tag: "v1"},
			"localhost:5000/team/app:v1",
		},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := types.ParseReference(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ParseReference(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
			if got.String() != tc.str {
				t.Errorf("String() = %q, want %q", got.String(), tc.str)
			}
		})
	}
}

func TestParseReference_invalid(t *testing.T) {
	for _, in := range []string{"", "Ubuntu:26.04", "ubuntu@sha256:short", "ubuntu:"} {
		if _, err := types.ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q): want error", in)
		}
	}
}

func TestParsePlatform(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want types.Platform
	}{
		{"linux/amd64", types.Platform{OS: "linux", Architecture: "amd64"}},
		{"linux/arm64/v8", types.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
		{"linux/arm64", types.Platform{OS: "linux", Architecture: "arm64"}},
	} {
		got, err := types.ParsePlatform(tc.in)
		if err != nil {
			t.Fatalf("ParsePlatform(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParsePlatform(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
		if got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
	}
	for _, in := range []string{"linux", "linux/", "/amd64", "linux/arm64/v8/x", ""} {
		if _, err := types.ParsePlatform(in); err == nil {
			t.Errorf("ParsePlatform(%q): want error", in)
		}
	}
}

func TestPlatform_Matches(t *testing.T) {
	arm64 := types.Platform{OS: "linux", Architecture: "arm64"}
	arm64v8 := types.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	spec := func(os, arch, variant string) ocispec.Platform {
		return ocispec.Platform{OS: os, Architecture: arch, Variant: variant}
	}
	for _, tc := range []struct {
		name string
		p    types.Platform
		o    ocispec.Platform
		want bool
	}{
		{"variant only on index side", arm64, spec("linux", "arm64", "v8"), true},
		{"variant only on request side", arm64v8, spec("linux", "arm64", ""), true},
		{"same variant", arm64v8, spec("linux", "arm64", "v8"), true},
		{"different variants", arm64v8, spec("linux", "arm64", "v9"), false},
		{"different arch", arm64, spec("linux", "amd64", ""), false},
		{"different os", arm64, spec("windows", "arm64", ""), false},
	} {
		if got := tc.p.Matches(tc.o); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHostPlatform(t *testing.T) {
	p := types.HostPlatform()
	if p.OS == "" || p.Architecture == "" {
		t.Fatalf("HostPlatform() = %#v", p)
	}
	if _, err := types.ParsePlatform(p.String()); err != nil {
		t.Errorf("HostPlatform().String() = %q does not parse: %v", p.String(), err)
	}
}
