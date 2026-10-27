package cli

import (
	"errors"
	"testing"

	"github.com/ngicks/oci-image-copy/internal/imagetest"
	"github.com/ngicks/oci-image-copy/ociimagecopy"
)

type failingWriter struct{}

var errFull = errors.New("device full")

func (failingWriter) Write([]byte) (int, error) { return 0, errFull }

func TestList_writeError(t *testing.T) {
	dir := t.TempDir()
	imagetest.WriteStore(t, dir, "example.com/cli/list:1")
	loc, err := ociimagecopy.ParseLocation("store:" + dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ociimagecopy.DefaultConfig()
	cfg.CacheDir = t.TempDir()
	svc, err := ociimagecopy.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	for _, asJson := range []bool{false, true} {
		err := List(t.Context(), failingWriter{}, asJson, svc, loc, ociimagecopy.ListOption{})
		if !errors.Is(err, errFull) || ExitCode(err) != ExitFailure {
			t.Errorf("json=%t: got %v, want the write error with exit 2", asJson, err)
		}
	}
}
