package commands

import (
	"fmt"

	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
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
	localTransport  string
	localPath       string
	remoteTransport string
	remotePath      string
	localDumpDir    string
	dryRun          bool
	assumeLocalHas  []string
	keepGoing       bool
}

func init() {
	rootCmd.AddCommand(pullCmd)

	f := pullCmd.Flags()
	f.StringVar(
		&pullFlags.localTransport,
		"local-transport",
		"containers-storage",
		"containers-storage|docker-daemon|oci",
	)
	f.StringVar(
		&pullFlags.localPath,
		"local-path",
		"",
		"local oci: dir (only when --local-transport=oci)",
	)
	bindRemoteTargetFlags(f)
	f.StringVar(
		&pullFlags.remoteTransport,
		"remote-transport",
		"containers-storage",
		"containers-storage|docker-daemon|oci",
	)
	f.StringVar(
		&pullFlags.remotePath,
		"remote-path",
		"",
		"remote oci: dir (only when --remote-transport=oci)",
	)
	f.StringVar(&pullFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")
	f.BoolVar(&pullFlags.dryRun, "dry-run", false, "no mutation; emit a plan instead")
	f.StringSliceVar(
		&pullFlags.assumeLocalHas,
		"assume-local-has",
		nil,
		"raw blob digests local already has (skips enumeration)",
	)
	f.BoolVar(&pullFlags.keepGoing, "keep-going", false, "continue on per-image failure")
}

func runPull(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if err := validateTransport("--local-transport", pullFlags.localTransport); err != nil {
		return err
	}
	if err := validateTransport("--remote-transport", pullFlags.remoteTransport); err != nil {
		return err
	}

	share, err := initShare(ctx,
		imagecopy.LocalConfig{
			BaseDir:   pullFlags.localDumpDir,
			Transport: skopeo.Transport(pullFlags.localTransport),
			OCIPath:   pullFlags.localPath,
		},
		imagecopy.RemoteConfig{
			Transport: skopeo.Transport(pullFlags.remoteTransport),
			OCIPath:   pullFlags.remotePath,
		},
	)
	if err != nil {
		return err
	}
	defer share.Close()

	res, err := share.Pull(ctx, imagecopy.PullArgs{
		Images:         args,
		DryRun:         pullFlags.dryRun,
		AssumeLocalHas: pullFlags.assumeLocalHas,
		KeepGoing:      pullFlags.keepGoing,
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
