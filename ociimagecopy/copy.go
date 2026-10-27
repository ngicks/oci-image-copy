package ociimagecopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

var (
	ErrUnsupportedDirection = errors.New("ociimagecopy: unsupported source and destination pair")
	ErrReadOnly             = errors.New("ociimagecopy: destination is read-only")
	ErrInvalidReference     = errors.New("ociimagecopy: invalid image reference")
	ErrInvalidPlatform      = errors.New("ociimagecopy: invalid platform")
)

// IsUsage reports whether err comes from the caller's input rather than from a transfer.
func IsUsage(err error) bool {
	for _, target := range []error{
		ErrInvalidLocation,
		ErrInvalidReference,
		ErrInvalidPlatform,
		ErrInvalidConfig,
		ErrUnsupportedDirection,
		ErrReadOnly,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

type CopyOption struct {
	// Platforms lists the platforms copied for every image; nil means the host platform.
	Platforms []types.Platform
	// DryRun plans without writing to the destination or the source store.
	// Planning may still fill the blob cache: a push compresses layers to learn their digests,
	// and a registry fallback downloads the image.
	DryRun bool
	// Planned, when non-nil, receives the result of planning before anything is transferred.
	Planned func(Result)
	// Progress is called from multiple goroutines.
	Progress func(Event)
}

type Event = transfer.Event

type EntryResult struct {
	Ref      types.Reference
	Platform types.Platform
	// Manifest is the manifest digest stored or imported; a push that re-compresses layers writes
	// a new manifest.
	Manifest digest.Digest
	// Missing and Present are the planned sizes of the blobs the destination lacked and held.
	// A blob shared by several entries counts in each.
	Missing  int64
	Present  int64
	Fallback bool // resolved through the registry fallback
	Err      error
}

type Result struct {
	Plan transfer.Plan
	// Entries holds one result per image and platform, in request order.
	Entries []EntryResult
}

// Copy copies set from src to dst for each platform.
// It supports containers-storage to store and store to containers-storage.
// A usage error (see IsUsage) returns before anything is opened.
// Otherwise the Result lists every entry; the error joins the entry failures and any failure that
// stopped the copy as a whole.
func (s *Service) Copy(
	ctx context.Context,
	src, dst Location,
	set types.ImageSetManifest,
	opt CopyOption,
) (Result, error) {
	if err := src.validate(); err != nil {
		return Result{}, err
	}
	if err := dst.validate(); err != nil {
		return Result{}, err
	}
	push := src.Kind == KindContainersStorage && dst.Kind == KindStore
	pull := src.Kind == KindStore && dst.Kind == KindContainersStorage
	if !push && !pull {
		return Result{}, fmt.Errorf("%w: %s to %s", ErrUnsupportedDirection, src, dst)
	}
	if !dst.Writable() {
		return Result{}, fmt.Errorf("%w: %s", ErrReadOnly, dst)
	}
	refs, err := parseSet(set)
	if err != nil {
		return Result{}, err
	}
	platforms := opt.Platforms
	if len(platforms) == 0 {
		platforms = []types.Platform{types.HostPlatform()}
	}

	cs, err := s.containersStorage()
	if err != nil {
		return Result{}, err
	}
	storeLoc := dst
	if pull {
		storeLoc = src
	}
	st, closeStore, err := s.openStore(ctx, storeLoc, push)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = closeStore() }()

	normalized := types.ImageSetManifest{}
	for _, r := range refs {
		normalized.Images = append(normalized.Images, r.String())
	}
	var (
		plan    transfer.Plan
		planErr error
	)
	if push {
		plan, planErr = transfer.PlanPush(ctx, cs.PushSource(st), st, normalized, platforms)
	} else {
		var errs []error
		for _, p := range platforms {
			pp, err := cs.PlanPull(ctx, st, normalized, p)
			plan.Entries = append(plan.Entries, pp.Entries...)
			errs = append(errs, err)
		}
		planErr = errors.Join(errs...)
	}

	res := newResult(refs, platforms, plan)
	if err := res.record(planErr); err != nil {
		return res, err
	}
	if opt.Planned != nil {
		opt.Planned(res)
	}
	if opt.DryRun {
		return res, res.err()
	}

	topt := &transfer.Options{Concurrency: s.cfg.Concurrency, Progress: opt.Progress}
	var runErr error
	if push {
		runErr = transfer.Push(ctx, plan, st, topt)
	} else {
		runErr = cs.Pull(ctx, plan, st, topt)
	}
	if err := res.record(runErr); err != nil {
		return res, errors.Join(err, res.err())
	}
	return res, res.err()
}

// ParsePlatforms parses os/arch[/variant] strings.
func ParsePlatforms(ss []string) ([]types.Platform, error) {
	var (
		out  []types.Platform
		errs []error
	)
	for _, s := range ss {
		p, err := types.ParsePlatform(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", ErrInvalidPlatform, err))
			continue
		}
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}

// ReadImageSet merges images with the images of each image set file at paths, in that order.
func ReadImageSet(images []string, paths []string) (types.ImageSetManifest, error) {
	set := types.ImageSetManifest{Images: append([]string{}, images...)}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return types.ImageSetManifest{}, fmt.Errorf("%w: %w", ErrInvalidReference, err)
		}
		var f types.ImageSetManifest
		if err := json.Unmarshal(b, &f); err != nil {
			return types.ImageSetManifest{}, fmt.Errorf(
				"%w: image set %s: %w",
				ErrInvalidReference,
				p,
				err,
			)
		}
		set.Images = append(set.Images, f.Images...)
	}
	return set, nil
}

func parseSet(set types.ImageSetManifest) ([]types.Reference, error) {
	if len(set.Images) == 0 {
		return nil, fmt.Errorf("%w: no images given", ErrInvalidReference)
	}
	var (
		refs []types.Reference
		seen = map[string]bool{}
		errs []error
	)
	for _, s := range set.Images {
		ref, err := types.ParseReference(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", ErrInvalidReference, err))
			continue
		}
		if !seen[ref.String()] {
			seen[ref.String()] = true
			refs = append(refs, ref)
		}
	}
	return refs, errors.Join(errs...)
}

// newResult lays out one EntryResult per requested image and platform and fills in the plan entry
// that answers each.
func newResult(refs []types.Reference, platforms []types.Platform, plan transfer.Plan) Result {
	res := Result{Plan: plan}
	used := make([]bool, len(plan.Entries))
	for _, ref := range refs {
		for _, p := range platforms {
			er := EntryResult{Ref: ref, Platform: p}
			for i, e := range plan.Entries {
				if used[i] || e.Image.Ref != ref ||
					!p.Matches(ocispec.Platform{
						OS:           e.Image.Platform.OS,
						Architecture: e.Image.Platform.Architecture,
						Variant:      e.Image.Platform.Variant,
					}) {
					continue
				}
				used[i] = true
				er.Platform = e.Image.Platform
				er.Manifest = e.Image.Manifest.Digest
				er.Missing = sumSize(e.Missing)
				er.Present = sumSize(e.Present)
				er.Fallback = e.Fallback
				break
			}
			res.Entries = append(res.Entries, er)
		}
	}
	return res
}

func sumSize(descs []ocispec.Descriptor) int64 {
	var n int64
	for _, d := range descs {
		n += d.Size
	}
	return n
}

// record attaches each *transfer.EntryError in err to its entry and returns the rest.
// An entry error matches by reference and by the platform either requested or resolved.
func (r *Result) record(err error) error {
	var rest []error
	for _, e := range flatten(err) {
		entryErr, ok := errors.AsType[*transfer.EntryError](e)
		if !ok {
			rest = append(rest, e)
			continue
		}
		matched := false
		for i := range r.Entries {
			er := &r.Entries[i]
			if er.Err == nil && er.Ref == entryErr.Ref && er.Platform == entryErr.Platform {
				er.Err = entryErr
				matched = true
				break
			}
		}
		if !matched {
			rest = append(rest, entryErr)
		}
	}
	return errors.Join(rest...)
}

func flatten(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []error{err}
}

func (r Result) err() error {
	var errs []error
	for _, e := range r.Entries {
		if e.Err != nil {
			errs = append(errs, e.Err)
		}
	}
	return errors.Join(errs...)
}

// ListOption narrows List.
type ListOption struct {
	// Repository, when set, keeps only the images of that repository.
	Repository string
}

// List yields every image and platform in a filesystem or S3 store.
// Any other location yields a single ErrInvalidLocation.
func (s *Service) List(
	ctx context.Context,
	loc Location,
	opt ListOption,
) iter.Seq2[store.Image, error] {
	return func(yield func(store.Image, error) bool) {
		if err := loc.validate(); err != nil {
			yield(store.Image{}, err)
			return
		}
		if loc.Kind != KindStore || (loc.URL.Scheme != "file" && loc.URL.Scheme != "s3") {
			yield(store.Image{}, fmt.Errorf(
				"%w: %s cannot be listed; list takes a filesystem or S3 store",
				ErrInvalidLocation,
				loc,
			))
			return
		}
		st, closeStore, err := s.openStore(ctx, loc, false)
		if err != nil {
			yield(store.Image{}, err)
			return
		}
		defer func() { _ = closeStore() }()
		for img, err := range st.Walk(ctx) {
			if err == nil && opt.Repository != "" && img.Ref.Repository != opt.Repository {
				continue
			}
			if !yield(img, err) {
				return
			}
		}
	}
}
