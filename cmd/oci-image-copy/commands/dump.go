package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/pkg/imageref"
	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

func dumpCmd(parent *cobra.Command, flagConfig *string) {
	var (
		flagLocal        string
		flagLocalDumpDir string
	)

	cmd := &cobra.Command{
		Use:               "dump IMAGE [IMAGE...]",
		Short:             "Dump local images into the on-disk OCI store layout.",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDump(cmd, args, *flagConfig, flagLocal, flagLocalDumpDir)
		},
	}

	f := cmd.Flags()
	f.StringVar(
		&flagLocal,
		"local",
		"containers-storage:",
		"local transport spec: containers-storage:|docker-daemon:|oci:/path|docker:",
	)
	f.StringVar(&flagLocalDumpDir, "local-dumpdir", "",
		"base of the local on-disk store layout; "+
			"when empty, falls back to ${XDG_DATA_HOME:-$HOME/.local/share}/oci-image-copy")

	parent.AddCommand(cmd)
}

func runDump(
	cmd *cobra.Command,
	args []string,
	flagConfig string,
	flagLocal string,
	flagLocalDumpDir string,
) error {
	ctx := cmd.Context()

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

	ls, err := ociimagecopy.ParseLocalSpec(cfg.Local)
	if err != nil {
		return err
	}
	if err := validateSourceLocal("--local", ls); err != nil {
		return err
	}

	local, err := ociimagecopy.NewLocal(ctx, ociimagecopy.LocalConfig{
		BaseDir:   cfg.LocalDumpDir,
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
