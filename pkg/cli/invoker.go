// Package cli holds the external-command invoker abstraction shared
// by [./skopeo] and [./docker]. The [Invoker] interface is the
// minimal contract those wrappers depend on; [LocalInvoker] runs on
// this machine via [exec.CommandContext] and [SshInvoker] runs on a
// remote host by spawning the system ssh binary (see [./ssh]).
package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
)

// Invoker is a command factory. Callers supply the executable name and
// arguments on each [Invoker.Command] call; the invoker is not bound to
// any particular executable. The returned [Cmd] runs the command and
// captures its output.
type Invoker interface {
	// Command returns a [Cmd] that will run exe with the given args.
	// No subprocess is started until [Cmd.Output] or [Cmd.Run] is called.
	Command(ctx context.Context, exe string, args ...string) Cmd
}

// Cmd represents a prepared command. Methods on Cmd start the command
// and wait for it to finish.
type Cmd interface {
	// Output runs the command and returns its captured stdout.
	// On non-zero exit an [*CommandError] is returned.
	Output() ([]byte, error)
	// Run runs the command, discarding stdout.
	// On non-zero exit an [*CommandError] is returned.
	Run() error
}

// LocalInvoker is an [Invoker] backed by [exec.CommandContext]. The
// executable is looked up on $PATH on each call.
type LocalInvoker struct {
	// StderrTailBytes caps how much trailing stderr is included in
	// the returned [*CommandError] on non-zero exit. Default 4096.
	StderrTailBytes int
}

// NewLocalInvoker returns a [LocalInvoker] with StderrTailBytes = 4096.
func NewLocalInvoker() *LocalInvoker {
	return &LocalInvoker{StderrTailBytes: 4096}
}

// Command implements [Invoker].
func (inv *LocalInvoker) Command(ctx context.Context, exe string, args ...string) Cmd {
	return &localCmd{ctx: ctx, exe: exe, args: args, tailBytes: inv.tailBytes()}
}

func (inv *LocalInvoker) tailBytes() int {
	if inv.StderrTailBytes <= 0 {
		return 4096
	}
	return inv.StderrTailBytes
}

type localCmd struct {
	ctx       context.Context
	exe       string
	args      []string
	tailBytes int
}

// Output implements [Cmd].
func (c *localCmd) Output() ([]byte, error) {
	argv := append([]string{c.exe}, c.args...)
	logger := contextkey.ValueSlogLoggerDefault(c.ctx)
	logger.LogAttrs(c.ctx, slog.LevelDebug, "exec",
		slog.String("exe", c.exe),
		slog.Any("argv", RedactArgv(argv)),
	)

	cmd := exec.CommandContext(c.ctx, c.exe, c.args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if stderr.Len() > 0 {
		logger.LogAttrs(c.ctx, slog.LevelDebug, "exec stderr",
			slog.String("exe", c.exe),
			slog.String("stderr", stderr.String()),
		)
	}

	if err != nil {
		exit := -1
		if cmd.ProcessState != nil {
			exit = cmd.ProcessState.ExitCode()
		}
		return stdout.Bytes(), &CommandError{
			Argv:       RedactArgv(argv),
			ExitCode:   exit,
			StderrTail: TailBytes(stderr.Bytes(), c.tailBytes),
			Err:        err,
		}
	}
	return stdout.Bytes(), nil
}

// Run implements [Cmd].
func (c *localCmd) Run() error {
	_, err := c.Output()
	return err
}

// SshInvoker is an [Invoker] that spawns a fresh `ssh ... -- <argv>`
// subprocess per command. SSH transport, auth, host-key verification and
// ProxyCommand/Include flow through the system ssh binary's normal
// config codepath — this invoker never touches keys, agents or
// known_hosts directly.
//
// The remote shell is `sh -c <argv>`: argv tokens are single-quoted
// before transmission so meta-characters are inert.
type SshInvoker struct {
	Target ssh.Target
	// StderrTailBytes caps how much trailing stderr is included in
	// the returned [*CommandError] on non-zero exit. Default 4096.
	StderrTailBytes int
}

// NewSshInvoker returns an [SshInvoker] for target.
func NewSshInvoker(target ssh.Target) *SshInvoker {
	return &SshInvoker{Target: target, StderrTailBytes: 4096}
}

// Command implements [Invoker].
func (inv *SshInvoker) Command(ctx context.Context, exe string, args ...string) Cmd {
	return &sshCmd{
		ctx:       ctx,
		target:    inv.Target,
		exe:       exe,
		args:      args,
		tailBytes: inv.tailBytes(),
	}
}

func (inv *SshInvoker) tailBytes() int {
	if inv.StderrTailBytes <= 0 {
		return 4096
	}
	return inv.StderrTailBytes
}

type sshCmd struct {
	ctx       context.Context
	target    ssh.Target
	exe       string
	args      []string
	tailBytes int
}

// Output implements [Cmd]. The full argv (exe + args) is sent to the
// remote shell; each token is single-quoted so meta-characters are inert.
func (c *sshCmd) Output() ([]byte, error) {
	argv := append([]string{c.exe}, c.args...)

	logger := contextkey.ValueSlogLoggerDefault(c.ctx)
	logger.LogAttrs(c.ctx, slog.LevelDebug, "ssh.exec",
		slog.Any("argv", RedactArgv(argv)),
		slog.String("host", c.target.String()),
	)

	sshArgs := append(ssh.BinaryArgs(c.target), "--", shellQuote(argv))
	cmd := exec.CommandContext(c.ctx, "ssh", sshArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if stderr.Len() > 0 {
		logger.LogAttrs(c.ctx, slog.LevelDebug, "ssh.exec.stderr",
			slog.String("stderr", stderr.String()),
		)
	}

	if err != nil {
		exit := -1
		if cmd.ProcessState != nil {
			exit = cmd.ProcessState.ExitCode()
		}
		return stdout.Bytes(), &CommandError{
			Argv:       RedactArgv(argv),
			ExitCode:   exit,
			StderrTail: TailBytes(stderr.Bytes(), c.tailBytes),
			Err:        err,
		}
	}
	return stdout.Bytes(), nil
}

// Run implements [Cmd].
func (c *sshCmd) Run() error {
	_, err := c.Output()
	return err
}

// shellQuote builds a single sh-safe word from argv. Each token is
// single-quoted so meta-characters (`'$|;&`) are inert; the whole
// string is fed to ssh as one argument so the remote sshd hands it to
// `sh -c`.
func shellQuote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}

// CommandError wraps a non-zero exit from an external process. The
// Err field is the underlying error from [exec.Cmd.Run] (or
// equivalent).
type CommandError struct {
	Argv       []string
	ExitCode   int
	StderrTail string
	Err        error
}

// Error implements error.
func (e *CommandError) Error() string {
	return fmt.Sprintf(
		"command %q failed: exit %d: %s: %v",
		strings.Join(e.Argv, " "),
		e.ExitCode,
		strings.TrimSpace(e.StderrTail),
		e.Err,
	)
}

// Unwrap implements errors.Unwrap.
func (e *CommandError) Unwrap() error { return e.Err }

// SensitiveFlags lists argv flags whose value should be redacted in
// debug logs (and in [*CommandError]).
var SensitiveFlags = map[string]struct{}{
	"--creds":          {},
	"--src-creds":      {},
	"--dest-creds":     {},
	"--authfile":       {},
	"--password":       {},
	"--password-stdin": {},
}

// RedactArgv returns a copy of argv with values of [SensitiveFlags]
// replaced by "<redacted>". Both `--flag value` and `--flag=value`
// forms are handled.
func RedactArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = a
		if eq := strings.IndexByte(a, '='); eq > 0 {
			if _, sensitive := SensitiveFlags[a[:eq]]; sensitive {
				out[i] = a[:eq] + "=<redacted>"
				continue
			}
		}
	}
	for i := 1; i < len(out); i++ {
		if _, sensitive := SensitiveFlags[out[i-1]]; sensitive {
			out[i] = "<redacted>"
		}
	}
	return out
}

// TailBytes returns at most max trailing bytes of b as a string.
// Used to cap the size of stderr captured into a [*CommandError].
func TailBytes(b []byte, max int) string {
	if max <= 0 || len(b) <= max {
		return string(b)
	}
	return string(b[len(b)-max:])
}
