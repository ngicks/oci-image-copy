package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeShim writes a /bin/sh script named `name` into a temp dir and
// prepends that dir to $PATH.
func writeShim(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeShim: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func TestLocalInvoker_OK(t *testing.T) {
	writeShim(t, "fake", `printf "hello stdout"`)
	inv := NewLocalInvoker()
	out, err := inv.Command(context.Background(), "fake").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello stdout" {
		t.Errorf("got %q", out)
	}
}

func TestLocalInvoker_Error(t *testing.T) {
	writeShim(t, "fake", `printf "boom\n" >&2; exit 7`)
	inv := NewLocalInvoker()
	_, err := inv.Command(context.Background(), "fake", "--something").Output()
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CommandError, got %T", err)
	}
	if ce.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", ce.ExitCode)
	}
	if !strings.Contains(ce.StderrTail, "boom") {
		t.Errorf("stderr tail = %q", ce.StderrTail)
	}
	if !strings.Contains(ce.Error(), "exit 7") {
		t.Errorf("Error() does not contain exit code: %q", ce.Error())
	}
}

func TestLocalInvoker_Run(t *testing.T) {
	writeShim(t, "fake", `exit 0`)
	inv := NewLocalInvoker()
	if err := inv.Command(context.Background(), "fake").Run(); err != nil {
		t.Fatal(err)
	}
}

func TestRedactArgv(t *testing.T) {
	t.Parallel()
	in := []string{
		"skopeo", "copy",
		"--creds", "user:secret",
		"--src-creds=u:p",
		"--authfile=/path",
		"some-other-flag",
	}
	got := RedactArgv(in)
	if got[3] != "<redacted>" {
		t.Errorf("--creds value not redacted: %v", got)
	}
	if got[4] != "--src-creds=<redacted>" {
		t.Errorf("--src-creds= not redacted: %v", got)
	}
	if got[5] != "--authfile=<redacted>" {
		t.Errorf("--authfile= not redacted: %v", got)
	}
	if got[6] != "some-other-flag" {
		t.Errorf("benign flag was rewritten: %v", got)
	}
}

func TestTailBytes(t *testing.T) {
	t.Parallel()
	if got := TailBytes([]byte("abcdef"), 100); got != "abcdef" {
		t.Errorf("short input rewrote: %q", got)
	}
	if got := TailBytes([]byte("abcdef"), 3); got != "def" {
		t.Errorf("tail mismatch: %q", got)
	}
}
