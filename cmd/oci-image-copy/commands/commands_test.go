package commands

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.podman.io/storage/pkg/reexec"

	"github.com/ngicks/oci-image-copy/internal/imagetest"
	"github.com/ngicks/oci-image-copy/ociimagecopy/cli"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// TestMain points containers-storage, the config file and the blob cache at a throwaway
// directory. containers-storage reads CONTAINERS_STORAGE_CONF once per process, so every test
// shares the one vfs store.
func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "oci-image-copy-commands-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	conf := filepath.Join(dir, "storage.conf")
	body := fmt.Sprintf(
		"[storage]\ndriver = \"vfs\"\ngraphroot = %q\nrunroot = %q\n",
		filepath.Join(dir, "graph"),
		filepath.Join(dir, "run"),
	)
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for k, v := range map[string]string{
		"CONTAINERS_STORAGE_CONF":  conf,
		"XDG_CONFIG_HOME":          filepath.Join(dir, "config"),
		"OCI_IMAGE_COPY_CACHE_DIR": filepath.Join(dir, "cache"),
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}

// execute runs the command line in-process and returns stdout and the exit status main would use.
func execute(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := rootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	if err != nil {
		t.Logf("%v: %v\nstderr:\n%s", args, err, stderr.String())
	}
	return stdout.String(), cli.ExitCode(err)
}

func storeWith(t *testing.T, refs ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, r := range refs {
		imagetest.WriteStore(t, dir, r)
	}
	return "store:" + dir
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch:\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestCopy_jsonAndRerun(t *testing.T) {
	src := storeWith(t, "example.com/cli/json:1")
	out, code := execute(t, "copy", src, "containers-storage:",
		"example.com/cli/json:1", "--platform", "linux/amd64", "--json")
	if code != cli.ExitOk {
		t.Fatalf("exit %d, want 0", code)
	}
	golden(t, "copy.json", strings.ReplaceAll(out, src, "store:SRC"))

	out, code = execute(t, "copy", src, "containers-storage:",
		"example.com/cli/json:1", "--platform", "linux/amd64")
	if code != cli.ExitOk || !strings.Contains(out, "nothing to transfer") {
		t.Errorf("re-run: exit %d, output:\n%s\nwant exit 0 and nothing to transfer", code, out)
	}
}

func TestCopy_partialFailure(t *testing.T) {
	src := storeWith(t, "example.com/cli/partial:1")
	set := filepath.Join(t.TempDir(), "set.json")
	// localhost:1 refuses connections, so the registry fallback fails fast.
	err := os.WriteFile(set, []byte(`{"images":["localhost:1/absent:1"]}`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	out, code := execute(t, "copy", src, "containers-storage:", "example.com/cli/partial:1",
		"--set", set, "--platform", "linux/amd64")
	if code != cli.ExitFailure {
		t.Errorf("exit %d, want 2", code)
	}
	results := map[string]string{}
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) >= 2 && (f[0] == "ok" || f[0] == "failed") {
			results[f[1]] = f[0]
		}
	}
	if results["example.com/cli/partial:1"] != "ok" || results["localhost:1/absent:1"] != "failed" {
		t.Errorf(
			"results %v, want the present image ok and the absent one failed:\n%s",
			results,
			out,
		)
	}
	if !strings.Contains(out, "copied 1 of 2 entries") {
		t.Errorf("output lacks the summary:\n%s", out)
	}
}

func TestList(t *testing.T) {
	src := storeWith(t, "example.com/cli/a:1", "example.com/cli/b:1")
	out, code := execute(t, "list", src, "--repository", "example.com/cli/b")
	if code != cli.ExitOk {
		t.Fatalf("exit %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 ||
		!strings.HasPrefix(lines[0], "example.com/cli/b:1  linux/amd64  sha256:") {
		t.Errorf("list output:\n%s\nwant one line for example.com/cli/b:1", out)
	}
}

func TestExitCodes_usage(t *testing.T) {
	for _, args := range [][]string{
		{"nosuch"},
		{"copy", "--bogus", "a", "b"},
		{"copy", "containers-storage:"},
		{"copy", "oci:/x", "containers-storage:", "a:1"},
		{"copy", "containers-storage:", "store:https://h/base", "a:1"},
		{"copy", "store:/a", "store:/b", "a:1"},
		{"copy", "containers-storage:", "store:/a", "Bad/Ref:1"},
		{"copy", "containers-storage:", "store:/a"},
		{"copy", "containers-storage:", "store:/a", "a:1", "--platform", "linux"},
		{"copy", "containers-storage:", "store:/a", "a:1", "--compression-level", "max"},
		{"copy", "containers-storage:", "store:/a", "a:1", "--quiet", "--json"},
		{"list"},
		{"list", "store:https://h/base"},
	} {
		if _, code := execute(t, args...); code != cli.ExitUsage {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
	}
}
