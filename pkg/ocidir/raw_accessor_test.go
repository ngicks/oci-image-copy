package ocidir

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestFsDir_RawIndex verifies that FsDir.RawIndex() returns byte-identical
// content to what was written to index.json.
func TestFsDir_RawIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	want := []byte(indexJSONFixture)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	fs := mustFs(t, dir)
	d := NewFsDir(fs)

	got, err := d.RawIndex()
	if err != nil {
		t.Fatalf("RawIndex: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RawIndex() bytes differ:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestFsDir_RawImageLayout verifies that FsDir.RawImageLayout() returns
// byte-identical content to what was written to oci-layout.
func TestFsDir_RawImageLayout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	want := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	fs := mustFs(t, dir)
	d := NewFsDir(fs)

	got, err := d.RawImageLayout()
	if err != nil {
		t.Fatalf("RawImageLayout: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RawImageLayout() bytes differ:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestSharedFsDir_RawAccessor verifies that SharedFsDir delegates RawIndex
// and RawImageLayout to its inner DirV1 when it implements RawAccessor.
func TestSharedFsDir_RawAccessor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wantIdx := []byte(indexJSONFixture)
	wantLayout := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), wantIdx, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), wantLayout, 0o644); err != nil {
		t.Fatal(err)
	}

	innerFs := mustFs(t, dir)
	blobsFs := mustFs(t, dir) // blobs pool (irrelevant for these calls)
	shared := NewSharedFsDir(NewFsDir(innerFs), blobsFs)

	// SharedFsDir should implement RawAccessor.
	raw, ok := any(shared).(RawAccessor)
	if !ok {
		t.Fatal("SharedFsDir does not implement RawAccessor")
	}

	gotIdx, err := raw.RawIndex()
	if err != nil {
		t.Fatalf("RawIndex: %v", err)
	}
	if !bytes.Equal(gotIdx, wantIdx) {
		t.Errorf("RawIndex bytes differ:\ngot:  %q\nwant: %q", gotIdx, wantIdx)
	}

	gotLayout, err := raw.RawImageLayout()
	if err != nil {
		t.Fatalf("RawImageLayout: %v", err)
	}
	if !bytes.Equal(gotLayout, wantLayout) {
		t.Errorf("RawImageLayout bytes differ:\ngot:  %q\nwant: %q", gotLayout, wantLayout)
	}
}
