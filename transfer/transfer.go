// Package transfer plans and executes pushes of image sets into a store.
//
// A push from one store into another is a mirror: *store.Store is itself a Source.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/types"
)

// Source resolves an entry to a single-platform image.
// *store.Store and *cstorage.Storage implement it; FromOciDir adapts a plain oci-dir.
type Source interface {
	Resolve(ctx context.Context, ref types.Reference, p types.Platform) (store.Image, error)
}

var _ Source = (*store.Store)(nil)

type Entry struct {
	Image    store.Image
	Missing  []ocispec.Descriptor // to transfer, dependencies first
	Present  []ocispec.Descriptor // already on the target
	Fallback bool                 // resolved through a fallback source because the store lacked it
}

type Plan struct {
	Entries []Entry
}

// Bytes is the size of the missing blobs, each digest counted once.
func (p Plan) Bytes() int64 {
	seen := map[digest.Digest]bool{}
	var n int64
	for _, e := range p.Entries {
		for _, d := range e.Missing {
			if !seen[d.Digest] {
				seen[d.Digest] = true
				n += d.Size
			}
		}
	}
	return n
}

// EntryError is the failure of one entry; PlanPush and Push join one per failed entry, so a caller
// can tell which entries failed with errors.As on each joined error.
type EntryError struct {
	Ref      types.Reference
	Platform types.Platform
	Err      error
}

func (e *EntryError) Error() string {
	return e.Err.Error()
}

func (e *EntryError) Unwrap() error {
	return e.Err
}

type Event struct {
	Ref      types.Reference
	Platform types.Platform
	Blob     digest.Digest
	Done     int64
	Total    int64
}

type Options struct {
	Concurrency int         // 0 means 4
	Progress    func(Event) // called from multiple goroutines
}

// ImageBlobs returns img's layers, config and manifest descriptors, dependencies first.
func ImageBlobs(ctx context.Context, img store.Image) ([]ocispec.Descriptor, error) {
	r, err := img.Dir.Blobs(ctx, img.Manifest.Digest)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	_ = r.Close()
	if err != nil {
		return nil, err
	}
	if digest.FromBytes(b) != img.Manifest.Digest {
		return nil, fmt.Errorf(
			"transfer: manifest %s does not match its digest",
			img.Manifest.Digest,
		)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("transfer: manifest %s: %w", img.Manifest.Digest, err)
	}
	out := append([]ocispec.Descriptor{}, m.Layers...)
	return append(out, m.Config, img.Manifest), nil
}

// PlanPush resolves every entry of set for each platform (nil means HostPlatform) and diffs blobs
// against dst.
// The plan holds every entry that resolved; the error joins the failures of the others.
func PlanPush(
	ctx context.Context,
	src Source,
	dst *store.Store,
	set types.ImageSetManifest,
	platforms []types.Platform,
) (Plan, error) {
	if len(platforms) == 0 {
		platforms = []types.Platform{types.HostPlatform()}
	}
	var (
		plan Plan
		errs []error
	)
	for _, s := range set.Images {
		ref, err := types.ParseReference(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range platforms {
			img, err := src.Resolve(ctx, ref, p)
			if err != nil {
				errs = append(errs, entryError(ref, p, err))
				continue
			}
			e, err := Diff(ctx, img, dst)
			if err != nil {
				errs = append(errs, entryError(ref, p, err))
				continue
			}
			plan.Entries = append(plan.Entries, e)
		}
	}
	return plan, errors.Join(errs...)
}

func entryError(ref types.Reference, p types.Platform, err error) error {
	return &EntryError{
		Ref:      ref,
		Platform: p,
		Err:      fmt.Errorf("transfer: %s for %s: %w", ref, p, err),
	}
}

// Diff splits img's blobs into those dst lacks and those it holds.
func Diff(ctx context.Context, img store.Image, dst *store.Store) (Entry, error) {
	descs, err := ImageBlobs(ctx, img)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Image: img}
	for _, d := range descs {
		ok, err := dst.HasBlob(ctx, d.Digest)
		if err != nil {
			return Entry{}, err
		}
		if ok {
			e.Present = append(e.Present, d)
		} else {
			e.Missing = append(e.Missing, d)
		}
	}
	return e, nil
}

type upload struct {
	entry int
	desc  ocispec.Descriptor
}

// Push uploads missing blobs and commits each leaf; an entry failure does not stop other entries.
// A failed upload is reported against the first entry that needs the blob; the other entries
// needing it fail on commit.
func Push(ctx context.Context, p Plan, dst *store.Store, opt *Options) error {
	if opt == nil {
		opt = &Options{}
	}
	concurrency := opt.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	var uploads []upload
	seen := map[digest.Digest]bool{}
	for i, e := range p.Entries {
		for _, d := range e.Missing {
			if !seen[d.Digest] {
				seen[d.Digest] = true
				uploads = append(uploads, upload{entry: i, desc: d})
			}
		}
	}

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
		sem  = make(chan struct{}, concurrency)
	)
	for _, u := range uploads {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			e := p.Entries[u.entry]
			if err := putBlob(ctx, e, u.desc, dst, opt.Progress); err != nil {
				mu.Lock()
				errs = append(
					errs,
					&EntryError{Ref: e.Image.Ref, Platform: e.Image.Platform, Err: err},
				)
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	for _, e := range p.Entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := dst.Commit(ctx, e.Image.Ref, e.Image.Platform, e.Image.Manifest); err != nil {
			errs = append(errs, &EntryError{
				Ref:      e.Image.Ref,
				Platform: e.Image.Platform,
				Err: fmt.Errorf("transfer: committing %s for %s: %w",
					e.Image.Ref, e.Image.Platform, err),
			})
		}
	}
	return errors.Join(errs...)
}

func putBlob(
	ctx context.Context,
	e Entry,
	d ocispec.Descriptor,
	dst *store.Store,
	progress func(Event),
) error {
	r, err := e.Image.Dir.Blobs(ctx, d.Digest)
	if err != nil {
		return fmt.Errorf("transfer: reading %s: %w", d.Digest, err)
	}
	defer func() { _ = r.Close() }()
	var body io.Reader = io.NewSectionReader(r, 0, r.Size())
	if progress != nil {
		body = &progressReader{r: body, ev: Event{
			Ref: e.Image.Ref, Platform: e.Image.Platform, Blob: d.Digest, Total: d.Size,
		}, fn: progress}
	}
	if err := dst.PutBlob(ctx, d, body); err != nil {
		return fmt.Errorf("transfer: uploading %s: %w", d.Digest, err)
	}
	return nil
}

type progressReader struct {
	r  io.Reader
	ev Event
	fn func(Event)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.ev.Done += int64(n)
		p.fn(p.ev)
	}
	return n, err
}
