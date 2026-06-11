package imagecopy

// fsadapter.go provides a type-erasing wrapper around [vroot.Fs[F]]
// implementations that return a concrete file type (e.g. *os.File).
// The wrapped type satisfies [vroot.Fs[vroot.File]] so it can be stored
// in the fields of [Local], [FsOciDirs], etc. alongside SFTP-backed
// filesystems (which already return [vroot.File]).

import (
	"io/fs"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
)

// osfsWrapper wraps *osfs.Fs and satisfies vroot.Fs[vroot.File] by
// converting *os.File returns to the vroot.File interface.
type osfsWrapper struct {
	inner *osfs.Fs
}

// NewOsFs creates a vroot.Fs[vroot.File] backed by the OS filesystem
// rooted at path. This is the type-erased counterpart of [osfs.NewFs].
func NewOsFs(path string) (vroot.Fs[vroot.File], error) {
	f, err := osfs.NewFs(path)
	if err != nil {
		return nil, err
	}
	return &osfsWrapper{inner: f}, nil
}

func (w *osfsWrapper) Chmod(name string, mode fs.FileMode) error {
	return w.inner.Chmod(name, mode)
}
func (w *osfsWrapper) Chown(name string, uid int, gid int) error {
	return w.inner.Chown(name, uid, gid)
}
func (w *osfsWrapper) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return w.inner.Chtimes(name, atime, mtime)
}
func (w *osfsWrapper) Close() error {
	return w.inner.Close()
}
func (w *osfsWrapper) Create(name string) (vroot.File, error) {
	return w.inner.Create(name)
}
func (w *osfsWrapper) Lchown(name string, uid int, gid int) error {
	return w.inner.Lchown(name, uid, gid)
}
func (w *osfsWrapper) Link(oldname, newname string) error {
	return w.inner.Link(oldname, newname)
}
func (w *osfsWrapper) Lstat(name string) (fs.FileInfo, error) {
	return w.inner.Lstat(name)
}
func (w *osfsWrapper) Mkdir(name string, perm fs.FileMode) error {
	return w.inner.Mkdir(name, perm)
}
func (w *osfsWrapper) MkdirAll(name string, perm fs.FileMode) error {
	return w.inner.MkdirAll(name, perm)
}
func (w *osfsWrapper) Name() string {
	return w.inner.Name()
}
func (w *osfsWrapper) Open(name string) (vroot.File, error) {
	return w.inner.Open(name)
}
func (w *osfsWrapper) OpenFile(name string, flag int, perm fs.FileMode) (vroot.File, error) {
	return w.inner.OpenFile(name, flag, perm)
}
func (w *osfsWrapper) ReadLink(name string) (string, error) {
	return w.inner.ReadLink(name)
}
func (w *osfsWrapper) Remove(name string) error {
	return w.inner.Remove(name)
}
func (w *osfsWrapper) RemoveAll(name string) error {
	return w.inner.RemoveAll(name)
}
func (w *osfsWrapper) Rename(oldname, newname string) error {
	return w.inner.Rename(oldname, newname)
}
func (w *osfsWrapper) Stat(name string) (fs.FileInfo, error) {
	return w.inner.Stat(name)
}
func (w *osfsWrapper) Symlink(oldname, newname string) error {
	return w.inner.Symlink(oldname, newname)
}

// ReadDir implements the vroot.ReadDirFs optional optimization.
func (w *osfsWrapper) ReadDir(name string) ([]fs.DirEntry, error) {
	f, err := w.inner.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// ReadFile implements the vroot.ReadFileFs optional optimization.
func (w *osfsWrapper) ReadFile(name string) ([]byte, error) {
	return w.inner.ReadFile(name)
}

var _ vroot.Fs[vroot.File] = (*osfsWrapper)(nil)
