package commands

import (
	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
	"github.com/ngicks/oci-image-copy/ociimagecopy/cli"
)

const listExample = `  oci-image-copy list store:/srv/images
  oci-image-copy list store:s3://bucket/images --repository docker.io/library/ubuntu --json`

func listCmd(parent *cobra.Command, flagConfig *string) {
	var (
		flagRepository string
		flagJson       bool
	)

	cmd := &cobra.Command{
		Use:   "list STORE",
		Short: "List the images and platforms in a filesystem or S3 store",
		Long: `list prints one line per image and platform in STORE: the reference, the
platform and the manifest digest. STORE is store:/path or store:s3://bucket/prefix;
HTTP(S) stores cannot be listed.`,
		Example:           listExample,
		Args:              usageArgs(cobra.ExactArgs(1)),
		ValidArgsFunction: completeLocation,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, args, *flagConfig, flagRepository, flagJson)
		},
	}

	cmd.Flags().StringVar(&flagRepository, "repository", "", "list only this repository")
	cmd.Flags().BoolVar(&flagJson, "json", false, "print a JSON array")

	parent.AddCommand(cmd)
}

func runList(
	cmd *cobra.Command,
	args []string,
	flagConfig, flagRepository string,
	flagJson bool,
) error {
	cfg, err := ociimagecopy.LoadConfig(flagConfig)
	if err != nil {
		return err
	}
	loc, err := ociimagecopy.ParseLocation(args[0])
	if err != nil {
		return err
	}
	svc, err := ociimagecopy.New(cfg, nil)
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	return cli.List(
		cmd.Context(),
		cmd.OutOrStdout(),
		flagJson,
		svc,
		loc,
		ociimagecopy.ListOption{Repository: flagRepository},
	)
}
