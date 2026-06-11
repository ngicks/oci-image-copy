package commands

import (
	"fmt"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
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
	localTransport  string
	localPath       string
	remoteTransport string
	remotePath      string
	localDumpDir    string
	dryRun          bool
	assumeRemoteHas []string
	keepGoing       bool
}

func init() {
	rootCmd.AddCommand(pushCmd)

	f := pushCmd.Flags()
	f.StringVar(
		&pushFlags.localTransport,
		"local-transport",
		"containers-storage",
		"containers-storage|docker-daemon|oci|docker",
	)
	f.StringVar(
		&pushFlags.localPath,
		"local-path",
		"",
		"local oci: dir (only when --local-transport=oci)",
	)
	bindRemoteTargetFlags(f)
	f.StringVar(
		&pushFlags.remoteTransport,
		"remote-transport",
		"containers-storage",
		"containers-storage|docker-daemon|oci",
	)
	f.StringVar(
		&pushFlags.remotePath,
		"remote-path",
		"",
		"remote oci: dir (only when --remote-transport=oci)",
	)
	f.StringVar(&pushFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")
	f.BoolVar(&pushFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
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

	if err := validateSourceTransport("--local-transport", pushFlags.localTransport); err != nil {
		return err
	}
	if err := validateTransport("--remote-transport", pushFlags.remoteTransport); err != nil {
		return err
	}

	share, err := initShare(ctx,
		imagecopy.LocalConfig{
			BaseDir:   pushFlags.localDumpDir,
			Transport: skopeo.Transport(pushFlags.localTransport),
			OCIPath:   pushFlags.localPath,
		},
		imagecopy.RemoteConfig{
			Transport: skopeo.Transport(pushFlags.remoteTransport),
			OCIPath:   pushFlags.remotePath,
		},
	)
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
