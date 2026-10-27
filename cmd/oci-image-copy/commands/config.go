package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
	"github.com/ngicks/oci-image-copy/ociimagecopy/cli"
)

// configLongFmt documents the resolved-config shape so users can write --format
// templates without reading the source. Keep the field list in sync with Config.
const configLongFmt = `config loads every layer (defaults < file < environment) and prints the
fully-resolved configuration. With no flags it prints indented JSON; with
--format it renders a Go text/template against the config value instead.

The default file is $XDG_CONFIG_HOME/oci-image-copy/config.json; --config or
OCI_IMAGE_COPY_CONF names another. Every field is also settable through an
OCI_IMAGE_COPY_ environment variable named after its key, e.g.
OCI_IMAGE_COPY_CONCURRENCY or OCI_IMAGE_COPY_S3_ENDPOINT.

The value passed to --format has this shape (Go field name, type, JSON key):

  Config
  ├─ .CacheDir          string    # downloaded blob cache        (cache_dir)
  ├─ .Registries        []string  # pull fallback mirrors        (registries)
  ├─ .CompressionLevel  string    # fastest|default|better|best  (compression_level)
  ├─ .Concurrency       int       # parallel blob transfers      (concurrency)
  └─ .S3                          # S3-compatible server         (s3)
      ├─ .Endpoint      string    #   endpoint URL               (s3.endpoint)
      ├─ .Region        string    #   region override            (s3.region)
      └─ .UsePathStyle  bool      #   path-style addressing      (s3.use_path_style)

Use the Go field names in --format (e.g. {{.CacheDir}}, or {{.S3.Endpoint}});
the default JSON output uses the lower-case keys shown in parentheses. The
template also sees these helper functions:

%s`

const configExample = `  oci-image-copy config
  oci-image-copy config --format '{{.CacheDir}}'
  oci-image-copy config --format '{{ json .S3 }}'`

func configCmd(parent *cobra.Command, flagConfig *string) {
	var flagFormat string

	cmd := &cobra.Command{
		Use:               "config",
		Short:             "Print the resolved configuration",
		Long:              fmt.Sprintf(configLongFmt, cli.TemplateFuncHelp()),
		Example:           configExample,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfig(cmd, args, *flagConfig, flagFormat)
		},
	}

	cmd.Flags().StringVarP(
		&flagFormat,
		"format",
		"f",
		"",
		"Go text/template rendered against the resolved config instead of JSON",
	)

	parent.AddCommand(cmd)
}

func runConfig(cmd *cobra.Command, _ []string, flagConfig, flagFormat string) error {
	cfg, err := ociimagecopy.LoadConfig(flagConfig)
	if err != nil {
		return err
	}
	// cmd.Println would route to stderr.
	return cli.RenderConfig(cmd.OutOrStdout(), cfg, flagFormat)
}
