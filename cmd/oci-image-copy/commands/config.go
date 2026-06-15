package commands

import (
	"encoding/json"
	"fmt"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/ngicks/oci-image-copy/pkg/ociimagecopy"
)

func configCmd(parent *cobra.Command, flagConfig *string) {
	var flagTemplate string

	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print the resolved configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfig(cmd, args, *flagConfig, flagTemplate)
		},
	}

	cmd.Flags().StringVarP(
		&flagTemplate, "template", "t", "",
		"Go text/template rendered against the config instead of JSON",
	)

	parent.AddCommand(cmd)
}

func runConfig(cmd *cobra.Command, args []string, flagConfig, flagTemplate string) error {
	cfg, err := ociimagecopy.LoadConfig(flagConfig)
	if err != nil {
		return err
	}

	if flagTemplate != "" {
		tmpl, err := template.New("config").Parse(flagTemplate)
		if err != nil {
			return err
		}
		if err := tmpl.Execute(cmd.OutOrStdout(), cfg); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout())
		return nil
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	cmd.Println(string(b))
	return nil
}
