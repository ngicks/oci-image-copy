package ociimagecopy

import (
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
)

// NewOsFs creates a vroot.Fs[vroot.File] backed by the OS filesystem rooted
// at path: the type-erased counterpart of [osfs.NewFs], so OS-backed and
// SFTP-backed filesystems can share the fields of [Local], [FsOciDirs], etc.
func NewOsFs(path string) (vroot.Fs[vroot.File], error) {
	f, err := osfs.NewFs(path)
	if err != nil {
		return nil, err
	}
	return vroot.Widen(f), nil
}
