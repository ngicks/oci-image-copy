package s3dir

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/opencontainers/go-digest"

	"github.com/ngicks/oci-image-copy/ocidir"
	"github.com/ngicks/oci-image-copy/store"
)

var (
	_ store.Backend = (*Store)(nil)
	_ store.Lister  = (*Store)(nil)
)

// StoreNamer translates between logical store paths and S3 locations.
// Name inverts Locate for keys under a located prefix; it reports false for keys outside the store.
type StoreNamer struct {
	Locate func(name string) (bucket, key string)
	Name   func(bucket, key string) (name string, ok bool)
}

// FlatStore keeps the tree under prefix in one bucket.
// prefix is prepended verbatim; include a trailing "/" to make it a directory.
func FlatStore(bucket, prefix string) StoreNamer {
	return StoreNamer{
		Locate: func(name string) (string, string) { return bucket, prefix + name },
		Name: func(b, key string) (string, bool) {
			if b != bucket {
				return "", false
			}
			return strings.CutPrefix(key, prefix)
		},
	}
}

type StoreClient interface {
	Client
	PutObject(
		ctx context.Context,
		in *s3.PutObjectInput,
		optFns ...func(*s3.Options),
	) (*s3.PutObjectOutput, error)
	ListObjectsV2(
		ctx context.Context,
		in *s3.ListObjectsV2Input,
		optFns ...func(*s3.Options),
	) (*s3.ListObjectsV2Output, error)
}

type Store struct {
	client StoreClient
	namer  StoreNamer
}

func NewStore(client StoreClient, namer StoreNamer) *Store {
	return &Store{client: client, namer: namer}
}

func (s *Store) Open(ctx context.Context, name string) (ocidir.ReadAtCloser, error) {
	bucket, key := s.namer.Locate(name)
	return openObject(ctx, s.client, name, bucket, key)
}

// Create uploads with If-None-Match: * so an existing key is never replaced.
// A sha256 dgst is sent as x-amz-checksum-sha256, which makes S3 reject mismatching content.
func (s *Store) Create(
	ctx context.Context,
	name string,
	r io.Reader,
	size int64,
	dgst digest.Digest,
) error {
	body, length, cleanup, err := seekable(r, size, dgst)
	if err != nil {
		return fmt.Errorf("s3dir: %s: %w", name, err)
	}
	defer cleanup()

	bucket, key := s.namer.Locate(name)
	in := &s3.PutObjectInput{
		Bucket:        &bucket,
		Key:           &key,
		Body:          body,
		ContentLength: &length,
		IfNoneMatch:   new("*"),
	}
	if isSHA256(dgst) {
		raw, err := hex.DecodeString(dgst.Encoded())
		if err != nil {
			return err
		}
		in.ChecksumSHA256 = new(base64.StdEncoding.EncodeToString(raw))
	}
	_, err = s.client.PutObject(ctx, in)
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) && status.HTTPStatusCode() == http.StatusPreconditionFailed {
		return fmt.Errorf("s3dir: %s: %w: %w", name, fs.ErrExist, err)
	}
	if err != nil {
		return wrapErr(name, err)
	}
	return nil
}

// seekable returns a seekable body: r itself when it can seek, otherwise a spooled temporary file.
// While spooling it checks size and a non-sha256 dgst, which S3 cannot verify.
func seekable(r io.Reader, size int64, dgst digest.Digest) (io.ReadSeeker, int64, func(), error) {
	if rs, ok := r.(io.ReadSeeker); ok && size >= 0 && (dgst == "" || isSHA256(dgst)) {
		return rs, size, func() {}, nil
	}
	f, err := os.CreateTemp("", "s3dir-upload-*")
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	var w io.Writer = f
	var digester digest.Digester
	if dgst != "" {
		digester = dgst.Algorithm().Digester()
		w = io.MultiWriter(f, digester.Hash())
	}
	n, err := io.Copy(w, r)
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("read %d bytes, want %d", n, size)
	}
	if err == nil && digester != nil && digester.Digest() != dgst {
		err = fmt.Errorf("content digest %s, want %s", digester.Digest(), dgst)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return f, n, cleanup, nil
}

// List yields the logical names of all objects under prefix.
func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		bucket, keyPrefix := s.namer.Locate(prefix)
		in := &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &keyPrefix}
		for {
			out, err := s.client.ListObjectsV2(ctx, in)
			if err != nil {
				yield("", wrapErr(prefix, err))
				return
			}
			for _, obj := range out.Contents {
				if obj.Key == nil {
					continue
				}
				name, ok := s.namer.Name(bucket, *obj.Key)
				if !ok {
					continue
				}
				if !yield(name, nil) {
					return
				}
			}
			if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
				return
			}
			in.ContinuationToken = out.NextContinuationToken
		}
	}
}

func isSHA256(dgst digest.Digest) bool {
	return dgst != "" && dgst.Algorithm() == digest.SHA256
}
