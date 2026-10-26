package s3dir_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/internal/storetest"
	"github.com/ngicks/oci-image-copy/ocidir/s3dir"
	"github.com/ngicks/oci-image-copy/store"
)

var bucketSeq atomic.Int64

func newBucket(t *testing.T, client *s3.Client) string {
	t.Helper()
	bucket := fmt.Sprintf("store-%d", bucketSeq.Add(1))
	if _, err := client.CreateBucket(
		t.Context(),
		&s3.CreateBucketInput{Bucket: &bucket},
	); err != nil {
		t.Fatalf("CreateBucket(%q): %v", bucket, err)
	}
	return bucket
}

func TestStore_Local(t *testing.T) {
	client := startVersitygw(t)

	storetest.TestBackend(t, func(t *testing.T) *store.Store {
		return store.New(s3dir.NewStore(client, s3dir.FlatStore(newBucket(t, client), "root/")))
	}, true)

	t.Run("exclusive create", func(t *testing.T) {
		b := s3dir.NewStore(client, s3dir.FlatStore(newBucket(t, client), ""))
		ctx := t.Context()
		data := []byte("hello")
		d := digest.FromBytes(data)
		if err := b.Create(ctx, "x", bytes.NewReader(data), int64(len(data)), d); err != nil {
			t.Fatal(err)
		}
		err := b.Create(ctx, "x", bytes.NewReader(data), int64(len(data)), d)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("second Create: want fs.ErrExist, got %v", err)
		}
	})

	t.Run("server-side checksum", func(t *testing.T) {
		b := s3dir.NewStore(client, s3dir.FlatStore(newBucket(t, client), ""))
		other := []byte("other")
		// A seekable body skips local verification, so only S3 can catch the mismatch.
		err := b.Create(
			t.Context(),
			"y",
			bytes.NewReader(other),
			int64(len(other)),
			digest.FromString("hello"),
		)
		if err == nil {
			t.Fatal("Create with a mismatching sha256: want error")
		}
		if _, err := b.Open(t.Context(), "y"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("mismatching object was stored: %v", err)
		}
	})

	t.Run("FlatStore Name", func(t *testing.T) {
		n := s3dir.FlatStore("b", "root/")
		if name, ok := n.Name("b", "root/shared/blobs/x"); !ok || name != "shared/blobs/x" {
			t.Errorf("Name = %q, %v", name, ok)
		}
		if _, ok := n.Name("other", "root/x"); ok {
			t.Error("Name accepted another bucket")
		}
		if _, ok := n.Name("b", "x"); ok {
			t.Error("Name accepted a key outside the prefix")
		}
	})
}
