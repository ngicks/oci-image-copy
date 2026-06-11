package commands

import (
	"fmt"
	"os"

	"github.com/ngicks/oci-image-copy/pkg/imagecopy"
	"github.com/spf13/cobra"
)

var pushCmd = &cobra.Command{
	Use:   "push IMAGE [IMAGE...]",
	Short: "Push images from local to remote.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runPush,
}

var pushFlags struct {
	local        string
	remote       string
	localDumpDir string
	dryRun       bool
	// Companion flags for the file-server remote.
	remoteHeaders      []string
	remoteChunkSize    string
	remoteNamingPrefix string
	assumeRemoteHas    []string
	keepGoing          bool
}

func init() {
	rootCmd.AddCommand(pushCmd)

	f := pushCmd.Flags()
	f.StringVar(
		&pushFlags.local,
		"local",
		"containers-storage:",
		"local transport spec: containers-storage:|docker-daemon:|oci:/path|docker:",
	)
	f.StringVar(
		&pushFlags.remote,
		"remote",
		"",
		"remote spec: ssh://[user@]host[:port][/<transport>], "+
			"file-server:<url>, or oci:/path",
	)
	f.StringVar(&pushFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")
	f.BoolVar(&pushFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringArrayVar(
		&pushFlags.remoteHeaders,
		"remote-header",
		nil,
		"extra request header for file-server remote (repeatable, e.g. 'Authorization: Bearer tok')",
	)
	f.StringVar(
		&pushFlags.remoteChunkSize,
		"remote-chunk-size",
		"",
		"file-server chunk size (human-readable, e.g. 100MiB; default 100MiB)",
	)
	f.StringVar(
		&pushFlags.remoteNamingPrefix,
		"remote-naming-prefix",
		"",
		"file-server naming convention prefix (default \"\")",
	)
	f.StringSliceVar(
		&pushFlags.assumeRemoteHas,
		"assume-remote-has",
		nil,
		"raw blob digests the peer already has (skips enumeration)",
	)
	f.BoolVar(&pushFlags.keepGoing, "keep-going", false, "continue on per-image failure")
}

func runPush(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if pushFlags.remote == "" {
		return fmt.Errorf("--remote is required")
	}

	// Validate --local spec (source-capable transports for push).
	ls, err := imagecopy.ParseLocalSpec(pushFlags.local)
	if err != nil {
		return err
	}
	if err := validateSourceLocal("--local", ls); err != nil {
		return err
	}

	share, err := initShare(ctx, pushFlags.local, pushFlags.remote, pushFlags.localDumpDir,
		fileServerOpts{
			headers:      pushFlags.remoteHeaders,
			chunkSize:    pushFlags.remoteChunkSize,
			namingPrefix: pushFlags.remoteNamingPrefix,
			authFromEnv:  os.Getenv(FileServerAuthEnvVar),
		})
	if err != nil {
		return err
	}
	defer share.Close()

	res, err := share.Push(ctx, imagecopy.PushArgs{
		Images:          args,
		DryRun:          pushFlags.dryRun,
		AssumeRemoteHas: pushFlags.assumeRemoteHas,
		KeepGoing:       pushFlags.keepGoing,
	})
	for _, r := range res.Reports {
		fmt.Fprintln(cmd.OutOrStdout(), r.SummaryLine())
	}
	if err != nil {
		return err
	}
	if res.FailedCount > 0 {
		return fmt.Errorf("%d image(s) failed", res.FailedCount)
	}
	return nil
}
