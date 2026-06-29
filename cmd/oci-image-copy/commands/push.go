package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

func pushCmd(parent *cobra.Command, flagConfig *string) {
	var (
		flagLocal              string
		flagRemote             string
		flagLocalDumpDir       string
		flagDryRun             bool
		flagRemoteHeaders      []string
		flagRemoteChunkSize    string
		flagRemoteNamingPrefix string
		flagAssumeRemoteHas    []string
		flagKeepGoing          bool
	)

	cmd := &cobra.Command{
		Use:               "push IMAGE [IMAGE...]",
		Short:             "Push images from local to remote.",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPush(cmd, args, *flagConfig,
				flagLocal, flagRemote, flagLocalDumpDir, flagDryRun,
				flagRemoteHeaders, flagRemoteChunkSize, flagRemoteNamingPrefix,
				flagAssumeRemoteHas, flagKeepGoing)
		},
	}

	f := cmd.Flags()
	f.StringVar(
		&flagLocal,
		"local",
		"containers-storage:",
		"local transport spec: containers-storage:|docker-daemon:|oci:/path|docker:",
	)
	f.StringVar(
		&flagRemote,
		"remote",
		"",
		"remote spec: ssh://[user@]host[:port][/<transport>], "+
			"file-server:<url>, or oci:/path",
	)
	f.StringVar(&flagLocalDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")
	f.BoolVar(&flagDryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringArrayVar(
		&flagRemoteHeaders,
		"remote-header",
		nil,
		"extra request header for file-server remote (repeatable, e.g. 'Authorization: Bearer tok')",
	)
	f.StringVar(
		&flagRemoteChunkSize,
		"remote-chunk-size",
		"",
		"file-server chunk size (human-readable, e.g. 100MiB; default 100MiB)",
	)
	f.StringVar(
		&flagRemoteNamingPrefix,
		"remote-naming-prefix",
		"",
		"file-server naming convention prefix (default \"\")",
	)
	f.StringSliceVar(
		&flagAssumeRemoteHas,
		"assume-remote-has",
		nil,
		"raw blob digests the peer already has (skips enumeration)",
	)
	f.BoolVar(&flagKeepGoing, "keep-going", false, "continue on per-image failure")

	parent.AddCommand(cmd)
}

func runPush(
	cmd *cobra.Command,
	args []string,
	flagConfig string,
	flagLocal string,
	flagRemote string,
	flagLocalDumpDir string,
	flagDryRun bool,
	flagRemoteHeaders []string,
	flagRemoteChunkSize string,
	flagRemoteNamingPrefix string,
	flagAssumeRemoteHas []string,
	flagKeepGoing bool,
) error {
	ctx := cmd.Context()

	if flagRemote == "" {
		return fmt.Errorf("--remote is required")
	}

	cfg, err := ociimagecopy.LoadConfig(flagConfig)
	if err != nil {
		return err
	}
	if cmd.Flags().Changed("local") {
		cfg.Local = flagLocal
	}
	if cmd.Flags().Changed("local-dumpdir") {
		cfg.LocalDumpDir = flagLocalDumpDir
	}
	if cmd.Flags().Changed("remote-header") {
		cfg.FileServer.Headers = flagRemoteHeaders
	}
	if cmd.Flags().Changed("remote-chunk-size") {
		cfg.FileServer.ChunkSize = flagRemoteChunkSize
	}
	if cmd.Flags().Changed("remote-naming-prefix") {
		cfg.FileServer.NamingPrefix = flagRemoteNamingPrefix
	}

	// Validate --local spec (source-capable transports for push).
	ls, err := ociimagecopy.ParseLocalSpec(cfg.Local)
	if err != nil {
		return err
	}
	if err := validateSourceLocal("--local", ls); err != nil {
		return err
	}

	share, err := initShare(ctx, cfg.Local, flagRemote, cfg.LocalDumpDir,
		cfg.Compression,
		fileServerOpts{
			headers:      cfg.FileServer.Headers,
			chunkSize:    cfg.FileServer.ChunkSize,
			namingPrefix: cfg.FileServer.NamingPrefix,
			auth:         cfg.FileServer.Auth,
		})
	if err != nil {
		return err
	}
	defer share.Close()

	res, err := share.Push(ctx, ociimagecopy.PushArgs{
		Images:          args,
		DryRun:          flagDryRun,
		AssumeRemoteHas: flagAssumeRemoteHas,
		KeepGoing:       flagKeepGoing,
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
