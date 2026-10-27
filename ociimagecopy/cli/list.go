package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

// ListEntry is one element of the --json output of list.
type ListEntry struct {
	Ref      string `json:"ref"`
	Platform string `json:"platform"`
	Manifest string `json:"manifest"`
}

// List prints one line, or with asJson one array element, per image and platform in loc.
// Leaves that cannot be read are reported in the returned error after the rest are printed.
func List(
	ctx context.Context,
	w io.Writer,
	asJson bool,
	svc *ociimagecopy.Service,
	loc ociimagecopy.Location,
	opt ociimagecopy.ListOption,
) error {
	var (
		entries []ListEntry
		errs    []error
	)
	for img, err := range svc.List(ctx, loc, opt) {
		if err != nil {
			if ociimagecopy.IsUsage(err) {
				return err
			}
			errs = append(errs, err)
			continue
		}
		entries = append(entries, ListEntry{
			Ref:      img.Ref.String(),
			Platform: img.Platform.String(),
			Manifest: img.Manifest.Digest.String(),
		})
	}
	ow := &errWriter{w: w}
	if asJson {
		if entries == nil {
			entries = []ListEntry{}
		}
		b, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(ow, string(b))
	} else {
		tw := tabwriter.NewWriter(ow, 0, 0, 2, ' ', 0)
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Ref, e.Platform, e.Manifest)
		}
		_ = tw.Flush() // ow keeps the error
	}
	return errors.Join(append(errs, ow.err)...)
}
