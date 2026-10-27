// Package cstorage moves images between a store and containers-storage (podman, CRI-O).
//
// It drives go.podman.io/storage directly: a pull skips every layer whose chain ID the host already
// has, downloads only the rest into a blob cache, applies them with PutLayer, and writes the image
// record
// the way go.podman.io/image's storage destination does.
//
// Programs importing cstorage must call reexec.Init() (go.podman.io/storage/pkg/reexec) first in
// main,
// because containers-storage re-executes the binary to apply layers.
package cstorage

import (
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"go.podman.io/storage"

	"github.com/ngicks/oci-image-copy/blobcache"
	"github.com/ngicks/oci-image-copy/transfer"
)

type Options struct {
	// Fallback resolves entries missing from the store during pull, normally a *registry.Source;
	// nil disables it.
	Fallback transfer.Source
	// CompressionLevel is the zstd level for layers push compresses anew; 0 means
	// zstd.SpeedBestCompression.
	// zstd output changes with the library version and level, and nothing relies on it, but a
	// fixed level keeps repeated pushes of one image from one build identical in practice.
	CompressionLevel zstd.EncoderLevel
}

type Storage struct {
	store storage.Store
	cache *blobcache.Cache
	opt   Options
	owned bool
}

// Open opens the store from podman's configuration; adjust may change the options first.
func Open(
	adjust func(*storage.StoreOptions),
	cache *blobcache.Cache,
	opt *Options,
) (*Storage, error) {
	opts, err := storage.DefaultStoreOptions()
	if err != nil {
		return nil, err
	}
	if adjust != nil {
		adjust(&opts)
	}
	st, err := storage.GetStore(opts)
	if err != nil {
		return nil, err
	}
	s := New(st, cache, opt)
	s.owned = true
	return s, nil
}

// New wraps a store opened by the caller; Close does not shut it down.
func New(st storage.Store, cache *blobcache.Cache, opt *Options) *Storage {
	s := &Storage{store: st, cache: cache}
	if opt != nil {
		s.opt = *opt
	}
	return s
}

func (s *Storage) zstdLevel() zstd.EncoderLevel {
	if s.opt.CompressionLevel == 0 {
		return zstd.SpeedBestCompression
	}
	return s.opt.CompressionLevel
}

func (s *Storage) Close() error {
	if !s.owned {
		return nil
	}
	_, err := s.store.Shutdown(false)
	return err
}

// chainIDs computes containers-storage layer IDs from diffIDs, as go.podman.io/image's storage
// destination does.
func chainIDs(diffIDs []digest.Digest) []string {
	ids := make([]string, len(diffIDs))
	for i, d := range diffIDs {
		if i == 0 {
			ids[i] = d.Encoded()
			continue
		}
		ids[i] = digest.Canonical.FromString(ids[i-1] + "+" + d.Encoded()).Encoded()
	}
	return ids
}
