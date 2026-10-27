package commands

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
	"github.com/ngicks/oci-image-copy/ociimagecopy/cli"
)

const copyLong = `copy moves images between an image store and containers-storage (podman's
local image store), in either direction.

SRC and DST are locations:

  store:/path                   a store directory on the local filesystem
  store:s3://bucket/prefix      a store in an S3 bucket (AWS default credentials)
  store:https://host/base       a read-only store served over HTTP(S)
  containers-storage:           the local containers-storage, as podman sees it

One of SRC and DST must be containers-storage:.

Images are references such as docker.io/library/ubuntu:26.04 or
example.com/app@sha256:<digest>, given as arguments and/or in --set files
holding {"images": [...]}. Both combine; duplicates are dropped.

copy plans first and transfers only what the destination lacks. Copying into
containers-storage skips layers the host already has and resumes interrupted
downloads from the blob cache; an image missing from the store is pulled from
its registry and kept in the store when the store is writable. Copying into a
store keeps the original compressed layers when they are reachable and
compresses the others with zstd.

Exit status is 0 when every entry succeeded, 1 for a usage error, and 2 when
any entry or the copy as a whole failed.`

const copyExample = `  oci-image-copy copy containers-storage: store:/srv/images ubuntu:26.04 app:1
  oci-image-copy copy store:s3://bucket/images containers-storage: --set set.json
  oci-image-copy copy store:https://mirror/images containers-storage: --set set.json --dry-run`

type copyFlags struct {
	sets             []string
	platforms        []string
	dryRun           bool
	quiet            bool
	json             bool
	compressionLevel string
	concurrency      int
}

func copyCmd(parent *cobra.Command, flagConfig *string) {
	var flags copyFlags

	cmd := &cobra.Command{
		Use:               "copy SRC DST [IMAGE...]",
		Short:             "Copy images between an image store and containers-storage",
		Long:              copyLong,
		Example:           copyExample,
		Args:              usageArgs(cobra.MinimumNArgs(2)),
		ValidArgsFunction: completeLocation,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCopy(cmd, args, *flagConfig, flags)
		},
	}

	f := cmd.Flags()
	f.StringArrayVar(
		&flags.sets,
		"set",
		nil,
		"image set file holding {\"images\": [...]}; repeatable",
	)
	f.StringArrayVar(
		&flags.platforms,
		"platform",
		nil,
		"os/arch[/variant] to copy; repeatable; default: the host platform",
	)
	f.BoolVar(&flags.dryRun, "dry-run", false, "print the plan without transferring")
	f.BoolVarP(&flags.quiet, "quiet", "q", false, "print only errors")
	f.BoolVar(&flags.json, "json", false, "print one JSON summary at the end")
	f.StringVar(
		&flags.compressionLevel,
		"compression-level",
		"best",
		"zstd level for newly compressed layers: fastest, default, better or best",
	)
	f.IntVar(&flags.concurrency, "concurrency", 4, "blobs transferred in parallel")
	_ = cmd.RegisterFlagCompletionFunc(
		"compression-level",
		cobra.FixedCompletions(
			[]cobra.Completion{"fastest", "default", "better", "best"},
			cobra.ShellCompDirectiveNoFileComp,
		),
	)

	parent.AddCommand(cmd)
}

func runCopy(cmd *cobra.Command, args []string, flagConfig string, flags copyFlags) error {
	// Checked here rather than with MarkFlagsMutuallyExclusive, whose error bypasses the flag
	// error hook that marks usage errors.
	if flags.quiet && flags.json {
		return cli.Usage(errors.New("--quiet and --json cannot be used together"))
	}
	cfg, err := ociimagecopy.LoadConfig(flagConfig)
	if err != nil {
		return err
	}
	if cmd.Flags().Changed("compression-level") {
		cfg.CompressionLevel = flags.compressionLevel
	}
	if cmd.Flags().Changed("concurrency") {
		cfg.Concurrency = flags.concurrency
	}
	src, err := ociimagecopy.ParseLocation(args[0])
	if err != nil {
		return err
	}
	dst, err := ociimagecopy.ParseLocation(args[1])
	if err != nil {
		return err
	}
	set, err := ociimagecopy.ReadImageSet(args[2:], flags.sets)
	if err != nil {
		return err
	}
	platforms, err := ociimagecopy.ParsePlatforms(flags.platforms)
	if err != nil {
		return err
	}
	svc, err := ociimagecopy.New(cfg, nil)
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()

	out := cli.OutputText
	switch {
	case flags.quiet:
		out = cli.OutputQuiet
	case flags.json:
		out = cli.OutputJson
	}
	return cli.Copy(
		cmd.Context(),
		cmd.OutOrStdout(),
		cmd.ErrOrStderr(),
		out,
		svc,
		src,
		dst,
		set,
		ociimagecopy.CopyOption{Platforms: platforms, DryRun: flags.dryRun},
	)
}
