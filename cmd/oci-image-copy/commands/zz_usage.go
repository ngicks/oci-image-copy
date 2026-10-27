package commands

import (
	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/ociimagecopy/cli"
)

// usageArgs marks positional-argument errors as usage errors, so they exit with status 1.
func usageArgs(args cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		return cli.Usage(args(cmd, a))
	}
}

func usageFlagError(_ *cobra.Command, err error) error {
	return cli.Usage(err)
}

// completeLocation offers the location prefixes for the first two positional arguments.
func completeLocation(
	_ *cobra.Command,
	args []string,
	_ string,
) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) >= 2 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []cobra.Completion{"store:", "containers-storage:"},
		cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
}
