package commands

import (
	"fmt"

	"github.com/ngicks/oci-image-copy/pkg/imagecopy"
	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/spf13/cobra"
)

var dumpCmd = &cobra.Command{
	Use:   "dump IMAGE [IMAGE...]",
	Short: "Dump local images into the on-disk OCI store layout.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runDump,
}

var dumpFlags struct {
	local        string
	localDumpDir string
}

func init() {
	rootCmd.AddCommand(dumpCmd)

	f := dumpCmd.Flags()
	f.StringVar(
		&dumpFlags.local,
		"local",
		"containers-storage:",
		"local transport spec: containers-storage:|docker-daemon:|oci:/path|docker:",
	)
	f.StringVar(&dumpFlags.localDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")
}

func runDump(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	ls, err := imagecopy.ParseLocalSpec(dumpFlags.local)
	if err != nil {
		return err
	}
	if err := validateSourceLocal("--local", ls); err != nil {
		return err
	}

	local, err := imagecopy.NewLocal(ctx, imagecopy.LocalConfig{
		BaseDir:   dumpFlags.localDumpDir,
		Transport: ls.Transport,
		OCIPath:   ls.Path,
	})
	if err != nil {
		return err
	}

	var failed int
	for _, raw := range args {
		ref, err := imageref.Parse(raw)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s ERROR: %v\n", raw, err)
			failed++
			continue
		}
		tagDir, err := local.Dump(ctx, ref)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s ERROR: %v\n", ref.String(), err)
			failed++
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s\n", ref.String(), tagDir)
	}
	if failed > 0 {
		return fmt.Errorf("%d image(s) failed", failed)
	}
	return nil
}
