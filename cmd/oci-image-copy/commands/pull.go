package commands

import (
	"fmt"
	"os"

	"github.com/ngicks/oci-image-copy/pkg/imagecopy"
	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull IMAGE [IMAGE...]",
	Short: "Pull images from remote to local.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runPull,
}

var pullFlags struct {
	local        string
	remote       string
	localDumpDir string
	dryRun       bool
	// Companion flags for the file-server remote.
	remoteHeaders      []string
	remoteChunkSize    string
	remoteNamingPrefix string
	assumeLocalHas     []string
	keepGoing          bool
	verifyReusedBlobs  bool
}

func init() {
	rootCmd.AddCommand(pullCmd)

	f := pullCmd.Flags()
	f.StringVar(
		&pullFlags.local,
		"local",
		"containers-storage:",
		"local transport spec: containers-storage:|docker-daemon:|oci:/path "+
			"(docker: not supported for pull — cannot be loaded into)",
	)
	f.StringVar(
		&pullFlags.remote,
		"remote",
		"",
		"remote spec: ssh://[user@]host[:port][/<transport>], "+
			"file-server:<url>, or oci:/path",
	)
	f.StringVar(&pullFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")
	f.BoolVar(&pullFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringArrayVar(
		&pullFlags.remoteHeaders,
		"remote-header",
		nil,
		"extra request header for file-server remote (repeatable, e.g. 'Authorization: Bearer tok')",
	)
	f.StringVar(
		&pullFlags.remoteChunkSize,
		"remote-chunk-size",
		"",
		"file-server chunk size (human-readable, e.g. 100MiB; default 100MiB)",
	)
	f.StringVar(
		&pullFlags.remoteNamingPrefix,
		"remote-naming-prefix",
		"",
		"file-server naming convention prefix (default \"\")",
	)
	f.StringSliceVar(
		&pullFlags.assumeLocalHas,
		"assume-local-has",
		nil,
		"raw blob digests local already has (skips enumeration)",
	)
	f.BoolVar(&pullFlags.keepGoing, "keep-going", false, "continue on per-image failure")
	f.BoolVar(&pullFlags.verifyReusedBlobs, "verify-reused-blobs", false,
		"sha256-verify local blobs that are already at the expected size before reusing; "+
			"re-downloads corrupt blobs")
}

func runPull(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if pullFlags.remote == "" {
		return fmt.Errorf("--remote is required")
	}

	// Validate --local spec: pull cannot use docker: transport (not enumerable).
	ls, err := imagecopy.ParseLocalSpec(pullFlags.local)
	if err != nil {
		return err
	}
	if err := validateEnumerableLocal("--local", ls); err != nil {
		return err
	}

	share, err := initShare(ctx, pullFlags.local, pullFlags.remote, pullFlags.localDumpDir,
		fileServerOpts{
			headers:      pullFlags.remoteHeaders,
			chunkSize:    pullFlags.remoteChunkSize,
			namingPrefix: pullFlags.remoteNamingPrefix,
			authFromEnv:  os.Getenv(FileServerAuthEnvVar),
		})
	if err != nil {
		return err
	}
	defer share.Close()

	res, err := share.Pull(ctx, imagecopy.PullArgs{
		Images:            args,
		DryRun:            pullFlags.dryRun,
		AssumeLocalHas:    pullFlags.assumeLocalHas,
		KeepGoing:         pullFlags.keepGoing,
		VerifyReusedBlobs: pullFlags.verifyReusedBlobs,
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
