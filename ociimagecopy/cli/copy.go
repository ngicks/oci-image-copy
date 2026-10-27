package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/ngicks/oci-image-copy/ociimagecopy"
	"github.com/ngicks/oci-image-copy/types"
)

type Output int

const (
	// OutputText prints the plan and the results, with progress on stderr.
	OutputText Output = iota
	// OutputQuiet prints nothing; the returned error carries every failure.
	OutputQuiet
	// OutputJson prints one CopyReport to stdout at the end.
	OutputJson
)

// CopyReport is the --json output of copy.
type CopyReport struct {
	Source      string        `json:"source"`
	Destination string        `json:"destination"`
	DryRun      bool          `json:"dry_run"`
	Bytes       int64         `json:"bytes"` // planned bytes to transfer, each blob counted once
	Entries     []EntryReport `json:"entries"`
	Failed      int           `json:"failed"`
	// Error is a failure that stopped the copy as a whole.
	Error string `json:"error,omitzero"`
}

type EntryReport struct {
	Ref      string `json:"ref"`
	Platform string `json:"platform"`
	Manifest string `json:"manifest,omitzero"`
	Missing  int64  `json:"missing"`
	Present  int64  `json:"present"`
	Fallback bool   `json:"fallback"`
	Error    string `json:"error,omitzero"`
}

// Copy runs svc.Copy and renders it to stdout and stderr in the given output.
// In text and JSON output, which already show each entry's failure, the returned error only counts
// the failed entries; a failure of the copy as a whole is returned as is.
func Copy(
	ctx context.Context,
	stdout, stderr io.Writer,
	out Output,
	svc *ociimagecopy.Service,
	src, dst ociimagecopy.Location,
	set types.ImageSetManifest,
	opt ociimagecopy.CopyOption,
) error {
	var prog *progress
	if out == OutputText {
		prog = newProgress(stderr, isTerminal(stderr))
		opt.Planned = func(res ociimagecopy.Result) { renderPlan(stdout, res, opt.DryRun) }
		opt.Progress = prog.event
	}
	res, err := svc.Copy(ctx, src, dst, set, opt)
	if prog != nil {
		prog.finish()
	}
	if err != nil && ociimagecopy.IsUsage(err) {
		return err
	}

	failed := 0
	for _, e := range res.Entries {
		if e.Err != nil {
			failed++
		}
	}
	// The error beyond entry failures, if any, stopped the copy as a whole.
	var whole error
	if err != nil && failed == 0 {
		whole = err
	} else if err != nil {
		whole = errors.Join(unattributed(err, res)...)
	}

	switch out {
	case OutputQuiet:
		return err
	case OutputJson:
		if err := renderReport(stdout, src, dst, opt.DryRun, res, failed, whole); err != nil {
			return err
		}
	default:
		if !opt.DryRun && len(res.Entries) > 0 {
			renderResults(stdout, res, failed)
		}
	}
	if whole != nil {
		return whole
	}
	if failed > 0 {
		return fmt.Errorf("%d of %s failed", failed, plural(len(res.Entries), "entry"))
	}
	return nil
}

// unattributed returns the parts of err that are not one entry's failure.
func unattributed(err error, res ociimagecopy.Result) []error {
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil
	}
	var out []error
outer:
	for _, e := range j.Unwrap() {
		for _, er := range res.Entries {
			if er.Err != nil && errors.Is(e, er.Err) {
				continue outer
			}
		}
		out = append(out, e)
	}
	return out
}

func entryName(ref types.Reference, p types.Platform) string {
	return ref.String() + "\t" + p.String()
}

func renderPlan(w io.Writer, res ociimagecopy.Result, dryRun bool) {
	blobs := map[string]bool{}
	for _, e := range res.Plan.Entries {
		for _, d := range e.Missing {
			blobs[d.Digest.String()] = true
		}
	}
	if len(blobs) == 0 {
		fmt.Fprintf(w, "plan: %s, nothing to transfer\n", plural(len(res.Entries), "entry"))
	} else {
		fmt.Fprintf(w, "plan: %s, %s (%s) to transfer\n",
			plural(len(res.Entries), "entry"), plural(len(blobs), "blob"),
			humanBytes(res.Plan.Bytes()))
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range res.Entries {
		if e.Err != nil {
			fmt.Fprintf(tw, "  %s\terror: %v\n", entryName(e.Ref, e.Platform), e.Err)
			continue
		}
		note := ""
		if e.Fallback {
			note = "\tfrom registry"
		}
		fmt.Fprintf(tw, "  %s\tmissing %s\tpresent %s%s\n",
			entryName(e.Ref, e.Platform), humanBytes(e.Missing), humanBytes(e.Present), note)
	}
	_ = tw.Flush()
	if dryRun {
		fmt.Fprintln(w, "dry run: nothing transferred")
	}
}

func renderResults(w io.Writer, res ociimagecopy.Result, failed int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range res.Entries {
		if e.Err != nil {
			fmt.Fprintf(tw, "failed\t%s\t%v\n", entryName(e.Ref, e.Platform), e.Err)
			continue
		}
		fmt.Fprintf(tw, "ok\t%s\t%s\n", entryName(e.Ref, e.Platform), e.Manifest)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "copied %d of %s\n", len(res.Entries)-failed, plural(len(res.Entries), "entry"))
}

func renderReport(
	w io.Writer,
	src, dst ociimagecopy.Location,
	dryRun bool,
	res ociimagecopy.Result,
	failed int,
	whole error,
) error {
	r := CopyReport{
		Source:      src.String(),
		Destination: dst.String(),
		DryRun:      dryRun,
		Bytes:       res.Plan.Bytes(),
		Entries:     []EntryReport{},
		Failed:      failed,
	}
	if whole != nil {
		r.Error = whole.Error()
	}
	for _, e := range res.Entries {
		er := EntryReport{
			Ref:      e.Ref.String(),
			Platform: e.Platform.String(),
			Manifest: e.Manifest.String(),
			Missing:  e.Missing,
			Present:  e.Present,
			Fallback: e.Fallback,
		}
		if e.Err != nil {
			er.Error = e.Err.Error()
		}
		r.Entries = append(r.Entries, er)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
