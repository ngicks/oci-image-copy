// Package ssh wraps the system ssh binary. SSH transport, auth,
// host-key verification and ProxyCommand/Include flow through the
// user's ssh client config — this package never touches keys, agents
// or known_hosts directly.
package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ngicks/go-common/contextkey"
)

// Target is either a raw OpenSSH destination name or an explicit
// host/user/port target. Auth, host-key verification, ProxyCommand,
// Include and friends are delegated to the user's ssh client config.
type Target struct {
	Name string
	User string
	Host string
	Port int
	// ConfigFile, when non-empty, passes `-F ConfigFile` to the ssh binary
	// so that a custom client configuration file is used instead of the
	// user's default ~/.ssh/config. Useful in tests that need an isolated
	// ssh config without touching the user's home directory.
	ConfigFile string
}

// String reassembles the target for display/config input. OpenSSH CLI
// invocations must use [BinaryArgs], because ssh takes the port via
// "-p PORT" rather than "user@host:port".
func (t Target) String() string {
	if t.Name != "" {
		return t.Name
	}
	dst := t.Host
	if t.User != "" {
		dst = t.User + "@" + dst
	}
	if t.Port != 0 && t.Port != 22 {
		return fmt.Sprintf("%s:%d", dst, t.Port)
	}
	return dst
}

// BinaryArgs returns the ssh CLI args that select target — i.e.
// either "name" or "[-p PORT] [user@]host". When [Target.ConfigFile]
// is non-empty, "-F ConfigFile" is prepended. Callers append either
// "-s sftp" (for the SFTP subsystem) or "-- argv..." (for one-shot
// remote commands).
func BinaryArgs(t Target) []string {
	var args []string
	if t.ConfigFile != "" {
		args = append(args, "-F", t.ConfigFile)
	}
	if t.Name != "" {
		return append(args, t.Name)
	}
	if t.Port != 0 && t.Port != 22 {
		args = append(args, "-p", strconv.Itoa(t.Port))
	}
	dst := t.Host
	if t.User != "" {
		dst = t.User + "@" + dst
	}
	args = append(args, dst)
	return args
}

// Subsystem starts `ssh ... -s sftp` and returns the running command
// together with stdout (server→client) and stdin (client→server).
//
// Callers feed the returned reader/writer into
// [github.com/pkg/sftp.NewClientPipe]. Stderr from the ssh process is
// written to stderr; when nil, it is discarded.
//
// The subprocess survives parent ctx cancellation — callers are
// expected to manage shutdown explicitly via cmd.Process.Kill or by
// closing the sftp.Client (which closes stdin and lets ssh exit).
func Subsystem(
	ctx context.Context,
	target Target,
	stderr io.Writer,
) (*exec.Cmd, io.Reader, io.WriteCloser, error) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, nil, nil, fmt.Errorf("ssh: ssh binary not on PATH: %w", err)
	}

	args := append(BinaryArgs(target), "-s", "sftp")
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "ssh.subsystem.spawn",
		slog.Any("argv", append([]string{"ssh"}, args...)),
	)

	cmd := exec.Command("ssh", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ssh: stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ssh: stdin pipe: %w", err)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("ssh: start sftp subsystem: %w", err)
	}
	return cmd, stdout, stdin, nil
}

// Probe executes `ssh -G ... <target>` and `ssh ... <target> true` and reports
// errors. This surfaces ProxyCommand/Include/etc. issues via the
// user's normal ssh config codepath before any later use.
func Probe(ctx context.Context, target Target) error {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "ssh.probe.start",
		slog.String("target", target.String()),
	)

	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("ssh probe: ssh binary not on PATH: %w", err)
	}

	{
		args := append([]string{"-G"}, BinaryArgs(target)...)
		cmd := exec.CommandContext(ctx, "ssh", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ssh probe: ssh %s failed: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
	}
	{
		args := append([]string{"-o", "BatchMode=yes"}, BinaryArgs(target)...)
		args = append(args, "true")
		cmd := exec.CommandContext(ctx, "ssh", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ssh probe: ssh %s failed: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
