package fsdir

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"strings"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
)

var (
	_ store.Backend  = (*Store[vroot.File])(nil)
	_ store.Lister   = (*Store[vroot.File])(nil)
	_ store.DirMaker = (*Store[vroot.File])(nil)
)

// Store is a store.Backend over fsys rooted at the logical root.
type Store[F vroot.File] struct {
	fsys vroot.Fs[F]
}

func NewStore[F vroot.File](fsys vroot.Fs[F]) *Store[F] {
	return &Store[F]{fsys: fsys}
}

func (s *Store[F]) Open(ctx context.Context, name string) (ocidir.ReadAtCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return file[F]{f: f, size: info.Size()}, nil
}

// Create writes r to a temporary file beside name, verifies it, and hard-links it into place,
// which fails when name already exists.
func (s *Store[F]) Create(
	ctx context.Context,
	name string,
	r io.Reader,
	size int64,
	dgst digest.Digest,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.fsys.Lstat(name); err == nil {
		return &fs.PathError{Op: "create", Path: name, Err: fs.ErrExist}
	}
	if err := s.fsys.MkdirAll(path.Dir(name), fs.ModePerm); err != nil {
		return err
	}
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	tmp := path.Join(path.Dir(name), ".tmp-"+path.Base(name)+"-"+hex.EncodeToString(suffix[:]))
	f, err := s.fsys.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = s.fsys.Remove(tmp) }()

	err = writeVerified(f, r, size, dgst)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("fsdir: creating %s: %w", name, err)
	}
	return s.fsys.Link(tmp, name)
}

func writeVerified(w io.Writer, r io.Reader, size int64, dgst digest.Digest) error {
	var digester digest.Digester
	if dgst != "" {
		if err := dgst.Validate(); err != nil {
			return err
		}
		digester = dgst.Algorithm().Digester()
		w = io.MultiWriter(w, digester.Hash())
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return err
	}
	if size >= 0 && n != size {
		return fmt.Errorf("wrote %d bytes, want %d", n, size)
	}
	if digester != nil && digester.Digest() != dgst {
		return fmt.Errorf("content digest %s, want %s", digester.Digest(), dgst)
	}
	return nil
}

func (s *Store[F]) MkdirAll(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fsys.MkdirAll(name, fs.ModePerm)
}

// List yields the regular files under prefix, recursively; temporary files of Create are skipped.
func (s *Store[F]) List(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		root := strings.TrimSuffix(prefix, "/")
		stop := errors.New("stop")
		err := vroot.WalkDir(
			s.fsys,
			root,
			nil,
			func(p, _ string, info fs.FileInfo, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) && p == root {
						return fs.SkipAll
					}
					if !yield("", err) {
						return stop
					}
					return nil
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if !info.Mode().IsRegular() || strings.HasPrefix(path.Base(p), ".tmp-") {
					return nil
				}
				if !yield(p, nil) {
					return stop
				}
				return nil
			},
		)
		if err != nil && !errors.Is(err, stop) {
			yield("", err)
		}
	}
}
