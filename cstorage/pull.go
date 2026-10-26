package cstorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/storage"

	"github.com/ngicks/oci-image-copy/store"
	"github.com/ngicks/oci-image-copy/transfer"
	"github.com/ngicks/oci-image-copy/types"
)

// PlanPull diffs each entry's layer chain against containers-storage by chain ID;
// an entry missing from src is resolved through Options.Fallback and, when src is writable, pushed
// into it.
//
// In each entry, Missing holds the layers to download and Present the layers of the present chain
// prefix.
// The plan holds every entry that resolved; the error joins the failures of the others.
func (s *Storage) PlanPull(
	ctx context.Context,
	src *store.Store,
	set types.ImageSetManifest,
	p types.Platform,
) (transfer.Plan, error) {
	var (
		plan transfer.Plan
		errs []error
	)
	for _, e := range set.Images {
		ref, err := types.ParseReference(e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		entry, err := s.planEntry(ctx, src, ref, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("cstorage: %s for %s: %w", ref, p, err))
			continue
		}
		plan.Entries = append(plan.Entries, entry)
	}
	return plan, errors.Join(errs...)
}

func (s *Storage) planEntry(
	ctx context.Context,
	src *store.Store,
	ref types.Reference,
	p types.Platform,
) (transfer.Entry, error) {
	img, err := src.Resolve(ctx, ref, p)
	fallback := false
	if errors.Is(err, fs.ErrNotExist) && s.opt.Fallback != nil {
		img, err = s.opt.Fallback.Resolve(ctx, ref, p)
		fallback = true
		if err == nil {
			err = keepInStore(ctx, img, src)
		}
	}
	if err != nil {
		return transfer.Entry{}, err
	}

	m, cfg, err := readImage(ctx, img)
	if err != nil {
		return transfer.Entry{}, err
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return transfer.Entry{}, fmt.Errorf("config lists %d diffIDs for %d layers",
			len(cfg.RootFS.DiffIDs), len(m.Layers))
	}
	ids := chainIDs(cfg.RootFS.DiffIDs)
	k := 0
	for k < len(ids) {
		if _, err := s.store.Layer(ids[k]); err != nil {
			break
		}
		k++
	}
	return transfer.Entry{
		Image:    img,
		Present:  m.Layers[:k],
		Missing:  m.Layers[k:],
		Fallback: fallback,
	}, nil
}

// keepInStore pushes a fallback-resolved image into src so the next pull finds it there;
// a read-only store is left alone.
func keepInStore(ctx context.Context, img store.Image, src *store.Store) error {
	e, err := transfer.Diff(ctx, img, src)
	if err != nil {
		return err
	}
	err = transfer.Push(ctx, transfer.Plan{Entries: []transfer.Entry{e}}, src, nil)
	if errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	return err
}

func readBlob(ctx context.Context, img store.Image, dgst digest.Digest) ([]byte, error) {
	r, err := img.Dir.Blobs(ctx, dgst)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		return nil, err
	}
	if digest.FromBytes(b) != dgst {
		return nil, fmt.Errorf("blob %s does not match its digest", dgst)
	}
	return b, nil
}

func readImage(ctx context.Context, img store.Image) (ocispec.Manifest, ocispec.Image, error) {
	var (
		m   ocispec.Manifest
		cfg ocispec.Image
	)
	b, err := readBlob(ctx, img, img.Manifest.Digest)
	if err != nil {
		return m, cfg, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, cfg, err
	}
	b, err = readBlob(ctx, img, m.Config.Digest)
	if err != nil {
		return m, cfg, err
	}
	return m, cfg, json.Unmarshal(b, &cfg)
}

// Pull fetches missing layers into the blob cache, applies them, and writes the image records.
// An entry failure does not stop other entries.
func (s *Storage) Pull(
	ctx context.Context,
	p transfer.Plan,
	src *store.Store,
	opt *transfer.Options,
) error {
	if opt == nil {
		opt = &transfer.Options{}
	}
	var errs []error
	for _, e := range p.Entries {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := s.pullEntry(ctx, e, opt); err != nil {
			errs = append(
				errs,
				fmt.Errorf("cstorage: pulling %s for %s: %w", e.Image.Ref, e.Image.Platform, err),
			)
		}
	}
	return errors.Join(errs...)
}

func (s *Storage) pullEntry(ctx context.Context, e transfer.Entry, opt *transfer.Options) error {
	img := e.Image
	m, cfg, err := readImage(ctx, img)
	if err != nil {
		return err
	}
	ids := chainIDs(cfg.RootFS.DiffIDs)

	for _, l := range e.Missing {
		if err := s.fetch(ctx, img, l, opt.Progress); err != nil {
			return err
		}
	}

	parent := ""
	for i, l := range m.Layers {
		if _, err := s.store.Layer(ids[i]); err == nil {
			parent = ids[i]
			continue
		}
		if err := s.applyLayer(ids[i], parent, l, cfg.RootFS.DiffIDs[i]); err != nil {
			return err
		}
		parent = ids[i]
	}
	return s.writeImage(ctx, img, m, parent)
}

func (s *Storage) fetch(
	ctx context.Context,
	img store.Image,
	l ocispec.Descriptor,
	progress func(transfer.Event),
) error {
	if s.cache.Has(l.Digest) {
		return nil
	}
	r, err := img.Dir.Blobs(ctx, l.Digest)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	var report func(int64)
	if progress != nil {
		report = func(done int64) {
			progress(
				transfer.Event{
					Ref:      img.Ref,
					Platform: img.Platform,
					Blob:     l.Digest,
					Done:     done,
					Total:    l.Size,
				},
			)
		}
	}
	return s.cache.Fetch(ctx, l, r, report)
}

func (s *Storage) applyLayer(id, parent string, l ocispec.Descriptor, diffID digest.Digest) error {
	r, err := s.cache.Open(l.Digest)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	size := l.Size
	layer, _, err := s.store.PutLayer(id, parent, nil, "", false, &storage.LayerOptions{
		OriginalDigest:     l.Digest,
		OriginalSize:       &size,
		UncompressedDigest: diffID,
	}, io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		return fmt.Errorf("applying layer %s: %w", l.Digest, err)
	}
	if layer.UncompressedDigest != diffID {
		_ = s.store.DeleteLayer(layer.ID)
		return fmt.Errorf(
			"layer %s: diffID %s, config says %s",
			l.Digest,
			layer.UncompressedDigest,
			diffID,
		)
	}
	return nil
}

// writeImage creates or extends the image record the way go.podman.io/image's storage destination
// does: image ID = config digest, config under its digest string, the manifest under
// manifest-<digest> and "manifest".
func (s *Storage) writeImage(
	ctx context.Context,
	img store.Image,
	m ocispec.Manifest,
	top string,
) error {
	mBytes, err := readBlob(ctx, img, img.Manifest.Digest)
	if err != nil {
		return err
	}
	cBytes, err := readBlob(ctx, img, m.Config.Digest)
	if err != nil {
		return err
	}
	var cfg ocispec.Image
	if err := json.Unmarshal(cBytes, &cfg); err != nil {
		return err
	}
	opts := &storage.ImageOptions{
		BigData: []storage.ImageBigDataOption{
			{Key: m.Config.Digest.String(), Data: cBytes, Digest: m.Config.Digest},
			{
				Key:    storage.ImageDigestManifestBigDataNamePrefix + "-" + img.Manifest.Digest.String(),
				Data:   mBytes,
				Digest: img.Manifest.Digest,
			},
			{Key: storage.ImageDigestBigDataKey, Data: mBytes, Digest: img.Manifest.Digest},
		},
		Metadata: "{}",
	}
	if cfg.Created != nil {
		opts.CreationDate = *cfg.Created
	}

	id := m.Config.Digest.Encoded()
	rec, err := s.store.CreateImage(id, nil, top, "", opts)
	if errors.Is(err, storage.ErrDuplicateID) {
		rec, err = s.store.Image(id)
		if err != nil {
			return err
		}
		if rec.TopLayer != top {
			return fmt.Errorf(
				"image %s exists with a different top layer: %w",
				id,
				storage.ErrDuplicateID,
			)
		}
		for _, bd := range opts.BigData {
			if err := s.store.SetImageBigData(id, bd.Key, bd.Data, manifest.Digest); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	return s.store.AddNames(rec.ID, []string{img.Ref.String()})
}
