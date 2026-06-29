// Package commands implements the cobra subcommands (push, pull, dump,
// version, config) for the oci-image-copy CLI binary.
package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/ngicks/go-common/contextkey"
	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/internal/loggerfactory"
)

func Execute(ctx context.Context) error {
	return rootCmd().ExecuteContext(ctx)
}

func rootCmd() *cobra.Command {
	var (
		logConfig   *loggerfactory.Config
		flagVersion bool
		flagConfig  string
	)

	cmd := &cobra.Command{
		Use:           "oci-image-copy",
		Short:         "Share OCI images between two hosts efficiently over SSH using skopeo + sftp.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// oci-image-copy emits its operational logs through slog, so logging
			// is ON by default here — a deliberate relaxation of loggerfactory's
			// opt-in default (which would otherwise discard every record unless
			// --log/--log-level or OCI_IMAGE_COPY_LOG_* is set). The flags and env
			// vars still override the format (default json) and level (default
			// info) on top of this enabled baseline.
			logConfig.Enabled = true
			if err := loggerfactory.ReadEnv(logConfig, "oci-image-copy", os.Environ()); err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
			}
			logger := loggerfactory.BuildLogger(logConfig)
			slog.SetDefault(logger)
			cmd.SetContext(contextkey.WithSlogLogger(cmd.Context(), logger))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagVersion {
				return runVersion(cmd, args)
			}
			return runRoot(cmd, args)
		},
	}

	logConfig = loggerfactory.RegisterFlags(cmd)
	cmd.Flags().BoolVar(&flagVersion, "version", false, "alias for the version subcommand")
	cmd.PersistentFlags().
		StringVar(&flagConfig, "config", "", "config file path; overrides the default location")

	versionCmd(cmd)
	configCmd(cmd, &flagConfig)
	pullCmd(cmd, &flagConfig)
	pushCmd(cmd, &flagConfig)
	dumpCmd(cmd, &flagConfig)

	return cmd
}

func runRoot(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}
