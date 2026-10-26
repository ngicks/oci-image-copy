package httpdir

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/ngicks/go-fsys-helper/stream/httprange"
	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
)

var _ store.Backend = (*Store)(nil)

// StoreNamer translates a logical store path to a URL.
type StoreNamer func(name string) string

// BaseURLStore maps every logical path under base.
func BaseURLStore(base string) StoreNamer {
	base = strings.TrimSuffix(base, "/")
	return func(name string) string { return base + "/" + name }
}

// Store is a read-only store.Backend; Create returns errors.ErrUnsupported and it does not list.
type Store struct {
	namer StoreNamer
	cfg   httprange.Config
}

// NewStore returns a Store reading entries at the URLs namer gives; cfg may be nil.
func NewStore(namer StoreNamer, cfg *httprange.Config) *Store {
	s := &Store{namer: namer}
	if cfg != nil {
		s.cfg = *cfg
	}
	return s
}

func (s *Store) Open(ctx context.Context, name string) (ocidir.ReadAtCloser, error) {
	return openURL(ctx, name, s.namer(name), &s.cfg)
}

func (s *Store) Create(context.Context, string, io.Reader, int64, digest.Digest) error {
	return errors.ErrUnsupported
}
