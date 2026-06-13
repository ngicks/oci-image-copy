package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
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

// TestSshCmd_OutputAndArgs runs the ssh command path against a fake `ssh`
// shim on $PATH. The shim records the argv it was invoked with so the test can
// assert that the non-interactive + keepalive flags and the shellQuote'd remote
// word reach ssh, and that stdout round-trips.
func TestSshCmd_OutputAndArgs(t *testing.T) {
	dir := writeShim(t, "ssh", `printf '%s\n' "$@" > "$SSH_ARGS_FILE"; printf "remote stdout"`)
	argsFile := filepath.Join(dir, "ssh_args.txt")
	t.Setenv("SSH_ARGS_FILE", argsFile)

	inv := NewSshInvoker(ssh.Target{Name: "prod"})
	out, err := inv.Command(context.Background(), "skopeo", "inspect", "x:y").Output()
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(out) != "remote stdout" {
		t.Errorf("stdout = %q, want %q", out, "remote stdout")
	}

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(recorded), "\n"), "\n")

	// Non-interactive + keepalive flags must be present.
	for _, want := range []string{"-n", "BatchMode=yes"} {
		if !slices.Contains(got, want) {
			t.Errorf("ssh argv missing %q; got %v", want, got)
		}
	}
	// The remote argv must arrive as one shellQuote'd word after "--".
	wantWord := shellQuote([]string{"skopeo", "inspect", "x:y"})
	dashIdx := slices.Index(got, "--")
	if dashIdx < 0 || dashIdx+1 >= len(got) {
		t.Fatalf("ssh argv missing `-- <word>`; got %v", got)
	}
	if got[dashIdx+1] != wantWord {
		t.Errorf("remote word = %q, want %q", got[dashIdx+1], wantWord)
	}
}

// TestSshCmd_Cancel verifies the ssh command path honors context
// cancellation: a shim that sleeps is torn down and Output returns promptly
// with an error rather than blocking for the full sleep.
func TestSshCmd_Cancel(t *testing.T) {
	writeShim(t, "ssh", `sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	inv := NewSshInvoker(ssh.Target{Name: "prod"})

	done := make(chan error, 1)
	go func() {
		_, err := inv.Command(ctx, "skopeo", "inspect", "x:y").Output()
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected error after cancellation, got nil")
		}
	case <-time.After(SshWaitDelay + 5*time.Second):
		t.Fatal("ssh command did not terminate after cancellation")
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
