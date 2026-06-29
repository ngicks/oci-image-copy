package docker

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

func TestPodman_Version_TrimsOutput(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{out: []byte("podman version 5.8.0\n")}
	p := NewPodman(r)
	v, err := p.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "podman version 5.8.0" {
		t.Errorf("Version = %q", v)
	}
}

func TestPodman_ImageLs_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{out: []byte("[]")}
	p := NewPodman(r)
	if _, err := p.ImageLs(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"podman", "image", "ls", "--format", "json"}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v", r.got)
	}
}

func TestPodman_ImageLs_Fixture(t *testing.T) {
	t.Parallel()
	p := NewPodman(&stubInvoker{out: readFixture(t, "podman-image-ls-json.json")})
	refs, err := p.ImageLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantOne := "docker.io/library/ubuntu:noble-20260410"
	if !slices.Contains(refs, wantOne) {
		t.Errorf("expected fixture to yield %q in result", wantOne)
	}
}

func TestParsePodmanImageLs_Empty(t *testing.T) {
	t.Parallel()
	imgs, err := ParsePodmanImageLs([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 0 {
		t.Errorf("expected empty, got %v", imgs)
	}
}

func TestParsePodmanImageLs_DeduplicatesNames(t *testing.T) {
	t.Parallel()
	json := []byte(`[
	  {"Id":"a","Names":["x:1","x:1","y:2"]},
	  {"Id":"b","Names":["x:1"]}
	]`)
	imgs, err := ParsePodmanImageLs(json)
	if err != nil {
		t.Fatal(err)
	}
	got := imageRefsFromPodmanList(imgs)
	if !reflect.DeepEqual(got, []string{"x:1", "y:2"}) {
		t.Errorf("got %v", got)
	}
}
