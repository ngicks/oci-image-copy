// Package cli holds oci-image-copy's CLI-presentation code: rendering resolved
// configuration, plans, progress and results. The ./cmd wiring layer calls into
// this package and returns its error.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/template"

	"github.com/ngicks/oci-image-copy/internal/templateutil"
	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

// TemplateFuncHelp returns the aligned help block for the helper functions
// available to a --format template.
func TemplateFuncHelp() string {
	return templateutil.FuncHelp()
}

// RenderConfig writes cfg to w as indented JSON, or, with a non-empty format, as
// the output of that Go text/template executed against cfg.
func RenderConfig(w io.Writer, cfg ociimagecopy.Config, format string) error {
	if format != "" {
		tmpl, err := template.New("config").
			Funcs(templateutil.FuncMap()).
			Parse(format)
		if err != nil {
			return fmt.Errorf("--format: %w", err)
		}
		if err := tmpl.Execute(w, cfg); err != nil {
			return fmt.Errorf("--format: %w", err)
		}
		fmt.Fprintln(w)
		return nil
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, string(b))
	return nil
}
