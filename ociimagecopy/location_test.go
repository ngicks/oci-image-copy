package ociimagecopy

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLocation(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in       string
		kind     Kind
		url      string
		writable bool
	}{
		{"containers-storage:", KindContainersStorage, "", true},
		{"store:/srv/images", KindStore, "file:///srv/images", true},
		{"store:file:///srv/images", KindStore, "file:///srv/images", true},
		{
			"store:rel/dir",
			KindStore,
			(&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(wd, "rel/dir"))}).String(),
			true,
		},
		{"store:s3://bucket/images", KindStore, "s3://bucket/images", true},
		{"store:s3://bucket", KindStore, "s3://bucket", true},
		{"store:https://mirror.example/base", KindStore, "https://mirror.example/base", false},
		{"store:http://localhost:8080", KindStore, "http://localhost:8080", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			l, err := ParseLocation(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if l.Kind != tc.kind {
				t.Errorf("Kind = %d, want %d", l.Kind, tc.kind)
			}
			if tc.url != "" && l.URL.String() != tc.url {
				t.Errorf("URL = %s, want %s", l.URL, tc.url)
			}
			if l.Writable() != tc.writable {
				t.Errorf("Writable = %t, want %t", l.Writable(), tc.writable)
			}
			again, err := ParseLocation(l.String())
			if err != nil || again.String() != l.String() {
				t.Errorf("String %q does not parse back: %v, %v", l.String(), again, err)
			}
		})
	}
}

func TestParseLocation_invalid(t *testing.T) {
	for _, in := range []string{
		"",
		"store:",
		"/srv/images",
		"oci:/srv/images",
		"containers-storage:[vfs@/var/lib]",
		"store:s3:///images",
		"store:https://mirror.example/base?token=1",
		"store:https://mirror.example/base#frag",
		"store:ftp://host/dir",
	} {
		if _, err := ParseLocation(in); !errors.Is(err, ErrInvalidLocation) {
			t.Errorf("ParseLocation(%q) = %v, want ErrInvalidLocation", in, err)
		}
	}
}

func TestLocation_validateHandBuilt(t *testing.T) {
	for _, l := range []Location{
		{},
		{Kind: KindStore},
		{Kind: KindStore, URL: &url.URL{Scheme: "file", Path: "relative"}},
		{Kind: Kind(42)},
	} {
		if err := l.validate(); !errors.Is(err, ErrInvalidLocation) {
			t.Errorf("validate(%#v) = %v, want ErrInvalidLocation", l, err)
		}
		if l.Writable() && l.Kind != KindContainersStorage {
			t.Errorf("%#v reports writable", l)
		}
	}
}
