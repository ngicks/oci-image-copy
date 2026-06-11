package skopeo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli"
)

// stubCmd is a canned [cli.Cmd].
type stubCmd struct {
	out []byte
	err error
}

func (c *stubCmd) Output() ([]byte, error) { return c.out, c.err }
func (c *stubCmd) Run() error              { _, err := c.Output(); return err }

// stubInvoker records the (exe, args) of every Command call as a flat
// argv slice, and returns canned output / error.
type stubInvoker struct {
	got [][]string
	out []byte
	err error
}

func (r *stubInvoker) Command(_ context.Context, exe string, args ...string) cli.Cmd {
	argv := append([]string{exe}, args...)
	dup := make([]string, len(argv))
	copy(dup, argv)
	r.got = append(r.got, dup)
	return &stubCmd{out: r.out, err: r.err}
}

func newSkopeo(r *stubInvoker) *Skopeo {
	return &Skopeo{Invoker: r}
}

func TestSkopeo_Version_TrimsOutput(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{out: []byte("skopeo version 1.20.0\n")}
	s := newSkopeo(r)
	v, err := s.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "skopeo version 1.20.0" {
		t.Errorf("Version = %q", v)
	}
	if !reflect.DeepEqual(r.got, [][]string{{"skopeo", "--version"}}) {
		t.Errorf("argv: got %v", r.got)
	}
}

func TestSkopeo_Inspect_Raw_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{out: []byte(`{"schemaVersion":2}`)}
	s := newSkopeo(r)
	_, err := s.Inspect(context.Background(), TransportRef{
		Transport: TransportContainersStorage,
		Arg1:      "myimg:latest",
	}, true, "")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"skopeo", "inspect", "--raw", "containers-storage:myimg:latest"}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v, want %v", r.got, want)
	}
}

func TestSkopeo_Inspect_NoRaw_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{out: []byte(`{}`)}
	s := newSkopeo(r)
	_, err := s.Inspect(context.Background(), TransportRef{
		Transport: TransportContainersStorage,
		Arg1:      "myimg:latest",
	}, false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"skopeo", "inspect", "containers-storage:myimg:latest"}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v, want %v", r.got, want)
	}
}

func TestSkopeo_Inspect_SharedBlobDir_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{}
	s := newSkopeo(r)
	_, err := s.Inspect(context.Background(),
		TransportRef{Transport: TransportOci, Arg1: "/tmp/oci/_tags/v1", Arg2: "ghcr.io/a/b:c"},
		true, "/tmp/share")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{
			"skopeo",
			"inspect",
			"--raw",
			"--shared-blob-dir",
			"/tmp/share",
			"oci:/tmp/oci/_tags/v1:ghcr.io/a/b:c",
		},
	}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v, want %v", r.got, want)
	}
}

func TestSkopeo_Inspect_ExtraArgs_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{}
	s := newSkopeo(r)
	_, err := s.Inspect(context.Background(),
		TransportRef{Transport: TransportContainersStorage, Arg1: "myimg:latest"},
		false, "", "--config", "--format", "{{.Digest}}")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{
			"skopeo",
			"inspect",
			"--config",
			"--format",
			"{{.Digest}}",
			"containers-storage:myimg:latest",
		},
	}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v, want %v", r.got, want)
	}
}

func TestSkopeo_Copy_ToOCI_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{}
	s := newSkopeo(r)
	err := s.Copy(context.Background(),
		TransportRef{Transport: TransportContainersStorage, Arg1: "ghcr.io/a/b:c"},
		TransportRef{Transport: TransportOci, Arg1: "/tmp/oci/_tags/c", Arg2: "ghcr.io/a/b:c"},
		"/tmp/share")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"skopeo", "copy",
		"--dest-shared-blob-dir", "/tmp/share",
		"containers-storage:ghcr.io/a/b:c",
		"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
	}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_Copy_FromOCI_Argv(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{}
	s := newSkopeo(r)
	err := s.Copy(context.Background(),
		TransportRef{Transport: TransportOci, Arg1: "/tmp/oci/_tags/c", Arg2: "ghcr.io/a/b:c"},
		TransportRef{Transport: TransportContainersStorage, Arg1: "ghcr.io/a/b:c"},
		"/tmp/share")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"skopeo", "copy",
		"--src-shared-blob-dir", "/tmp/share",
		"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
		"containers-storage:ghcr.io/a/b:c",
	}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_Copy_NoShareDir_OmitsFlag(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{}
	s := newSkopeo(r)
	err := s.Copy(context.Background(),
		TransportRef{Transport: TransportDocker, Arg1: "registry/foo:latest"},
		TransportRef{Transport: TransportContainersStorage, Arg1: "registry/foo:latest"},
		"")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"skopeo", "copy",
		"docker://registry/foo:latest",
		"containers-storage:registry/foo:latest",
	}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_Copy_RejectsShareDirWithoutOciSide(t *testing.T) {
	t.Parallel()
	s := newSkopeo(&stubInvoker{})
	err := s.Copy(context.Background(),
		TransportRef{Transport: TransportDocker, Arg1: "a/b:c"},
		TransportRef{Transport: TransportContainersStorage, Arg1: "a/b:c"},
		"/share")
	if err == nil {
		t.Error("expected error when sharedBlobDir is set but neither side is oci")
	}
}

func TestSkopeo_Copy_RejectsEmptyOciDir(t *testing.T) {
	t.Parallel()
	s := newSkopeo(&stubInvoker{})
	err := s.Copy(context.Background(),
		TransportRef{Transport: TransportContainersStorage, Arg1: "x:y"},
		TransportRef{Transport: TransportOci, Arg2: "x:y"},
		"/share")
	if err == nil {
		t.Error("expected error for empty ociDir")
	}
}

func TestSkopeo_CompressionArgs(t *testing.T) {
	t.Parallel()
	r := &stubInvoker{}
	s := newSkopeo(r)
	s.CompressionFormat = "zstd"
	s.CompressionLevel = 19
	src := TransportRef{Transport: TransportContainersStorage, Arg1: "ghcr.io/a/b:c"}
	dst := TransportRef{Transport: TransportOci, Arg1: "/tmp/oci/_tags/c", Arg2: "ghcr.io/a/b:c"}
	if err := s.Copy(context.Background(), src, dst, "/tmp/share"); err != nil {
		t.Fatal(err)
	}
	if err := s.Copy(context.Background(), dst, src, "/tmp/share"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{
			"skopeo", "copy",
			"--dest-compress-format", "zstd",
			"--dest-compress-level", "19",
			"--dest-shared-blob-dir", "/tmp/share",
			"containers-storage:ghcr.io/a/b:c",
			"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
		},
		{
			"skopeo", "copy",
			"--dest-compress-format", "zstd",
			"--dest-compress-level", "19",
			"--src-shared-blob-dir", "/tmp/share",
			"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
			"containers-storage:ghcr.io/a/b:c",
		},
	}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_PropagatesRunnerError(t *testing.T) {
	t.Parallel()
	want := errors.New("simulated runner error")
	r := &stubInvoker{err: want}
	s := newSkopeo(r)
	_, err := s.Inspect(context.Background(), TransportRef{
		Transport: TransportContainersStorage, Arg1: "x:y",
	}, true, "")
	if !errors.Is(err, want) {
		t.Errorf("expected wrapped %v, got %v", want, err)
	}
	if !strings.Contains(err.Error(), "simulated runner error") {
		t.Errorf("error text = %q", err.Error())
	}
}
