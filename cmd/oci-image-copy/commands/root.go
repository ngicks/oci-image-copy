// Package commands implements the cobra subcommands (push, pull, dump)
// for the oci-image-copy CLI binary.
package commands

import (
	"context"
	"fmt"

	"github.com/ngicks/oci-image-copy/pkg/cli/ssh"
	"github.com/ngicks/oci-image-copy/pkg/imagecopy"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

var rootCmd = &cobra.Command{
	Use:           "oci-image-copy",
	Short:         "Share OCI images between two hosts efficiently over SSH using skopeo + sftp.",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	RunE:          runRoot,
}

func runRoot(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}

var remoteTarget ssh.Target

func bindRemoteTargetFlags(f *pflag.FlagSet) {
	f.StringVar(&remoteTarget.Name, "remote-name", "", "ssh config destination name")
	f.StringVar(&remoteTarget.User, "remote-user", "", "remote ssh user (optional)")
	f.StringVar(&remoteTarget.Host, "remote-host", "", "remote ssh hostname/address")
	f.IntVar(&remoteTarget.Port, "remote-port", 0, "remote ssh port (0 uses ssh default/config)")
}

// validTransports is the set of transports valid wherever the local
// side must be enumerable (pull --local-transport) and for every
// --remote-transport.
var validTransports = map[string]struct{}{
	"containers-storage": {},
	"docker-daemon":      {},
	"oci":                {},
}

// validSourceTransports additionally allows the docker (registry)
// transport, which can act as a dump/push source but cannot be
// enumerated or loaded into.
var validSourceTransports = map[string]struct{}{
	"containers-storage": {},
	"docker-daemon":      {},
	"oci":                {},
	"docker":             {},
}

// validateTransport returns a clear error when t is not a supported
// enumerable transport.
func validateTransport(flag, t string) error {
	if _, ok := validTransports[t]; !ok {
		return fmt.Errorf(
			"%s: unsupported transport %q (want one of: containers-storage, docker-daemon, oci)",
			flag,
			t,
		)
	}
	return nil
}

// validateSourceTransport is validateTransport for flags whose
// transport only ever acts as a dump/push source.
func validateSourceTransport(flag, t string) error {
	if _, ok := validSourceTransports[t]; !ok {
		return fmt.Errorf(
			"%s: unsupported transport %q (want one of: containers-storage, docker-daemon, oci, docker)",
			flag,
			t,
		)
	}
	return nil
}

func validateRemoteTarget(t ssh.Target) error {
	if t.Name != "" {
		if t.User != "" || t.Host != "" || t.Port != 0 {
			return fmt.Errorf(
				"--remote-name cannot be combined with --remote-user, --remote-host, or --remote-port",
			)
		}
		return nil
	}
	if t.Host == "" {
		return fmt.Errorf("--remote-name or --remote-host is required")
	}
	if t.Port < 0 {
		return fmt.Errorf("--remote-port must be non-negative")
	}
	return nil
}

// initShare builds a [*imagecopy.Local] + SSH-backed
// [imagecopy.Remote] and wraps them in a [*imagecopy.Share].
// Validates the remote target, runs an ssh probe, and dials sftp.
// remoteCfg.Target is filled in from the bound CLI flags. Caller is
// responsible for share.Close().
func initShare(
	ctx context.Context,
	localCfg imagecopy.LocalConfig,
	remoteCfg imagecopy.RemoteConfig,
) (*imagecopy.Share, error) {
	remoteCfg.Target = remoteTarget
	if err := validateRemoteTarget(remoteCfg.Target); err != nil {
		return nil, err
	}
	local, err := imagecopy.NewLocal(ctx, localCfg)
	if err != nil {
		return nil, err
	}
	if err := ssh.Probe(ctx, remoteCfg.Target); err != nil {
		return nil, fmt.Errorf("ssh probe: %w", err)
	}
	remote, err := imagecopy.NewRemote(ctx, remoteCfg)
	if err != nil {
		return nil, err
	}
	return imagecopy.NewShare(local, remote), nil
}
