package s3dir_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ngicks/go-fsys-helper/vroot/memfs"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ngicks/oci-image-copy/internal/ocidirtest"
	"github.com/ngicks/oci-image-copy/ocidir/fsdir"
	"github.com/ngicks/oci-image-copy/ocidir/s3dir"
)

const (
	// Pinned to a release at least two weeks old so test runs stay reproducible.
	versitygwImage = "docker.io/versity/versitygw:v1.8.0"
	accessKey      = "ocidirtest"
	secretKey      = "ocidirtest-secret"
)

// startVersitygw runs versitygw with the posix backend on a temp dir and returns a client for it.
func startVersitygw(t *testing.T) *s3.Client {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not installed")
	}

	// Host networking with a pre-picked port, because containers.conf may force netns=host,
	// which silently drops -p port publishing.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	out, err := exec.Command(
		"podman", "run", "-d", "--rm",
		"--network", "host",
		"-v", t.TempDir()+":/data",
		versitygwImage,
		"--access", accessKey, "--secret", secretKey,
		"--port", addr,
		"posix", "/data",
	).Output()
	if err != nil {
		t.Fatalf("podman run: %v: %s", err, stderrOf(err))
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "-f", id).Run() })

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + addr),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}, nil
		}),
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("podman", "logs", id).CombinedOutput()
			t.Fatalf("versitygw not ready: %v\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func stderrOf(err error) []byte {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.Stderr
	}
	return nil
}

// upload stores files, keyed by oci-dir relative path, where namer places them.
func upload(t *testing.T, client *s3.Client, namer s3dir.Namer, files map[string][]byte) {
	t.Helper()
	created := map[string]bool{}
	for name, b := range files {
		var bucket, key string
		switch name {
		case ocispec.ImageLayoutFile:
			bucket, key = namer.OciLayout()
		case ocispec.ImageIndexFile:
			bucket, key = namer.Index()
		default:
			dgst, ok := ocidirtest.BlobDigest(name)
			if !ok {
				continue
			}
			bucket, key = namer.Blob(dgst)
		}
		if !created[bucket] {
			_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: &bucket})
			if err != nil {
				t.Fatalf("CreateBucket(%q): %v", bucket, err)
			}
			created[bucket] = true
		}
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: &bucket,
			Key:    &key,
			Body:   bytes.NewReader(b),
		})
		if err != nil {
			t.Fatalf("PutObject(%q, %q): %v", bucket, key, err)
		}
	}
}

func splitNamer(metaBucket, blobBucket string) s3dir.Namer {
	return s3dir.Namer{
		OciLayout: func() (string, string) { return metaBucket, ocispec.ImageLayoutFile },
		Index:     func() (string, string) { return metaBucket, ocispec.ImageIndexFile },
		Blob: func(dgst digest.Digest) (string, string) {
			return blobBucket, dgst.Algorithm().String() + "/" + dgst.Encoded()
		},
	}
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestDir_Local(t *testing.T) {
	client := startVersitygw(t)
	f := ocidirtest.New()
	fsys := memfs.New("fixture")
	if err := ocidirtest.Write(fsys, f.Files); err != nil {
		t.Fatal(err)
	}
	want := fsdir.New(fsys)

	for _, tc := range []struct {
		name  string
		namer s3dir.Namer
	}{
		{"flat", s3dir.Flat("flat", "layout/")},
		{"split", splitNamer("meta", "blobs")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upload(t, client, tc.namer, f.Files)
			dir := s3dir.New(client, tc.namer)
			ocidirtest.TestOciDir(t, dir, f)
			ocidirtest.TestSameAs(t, want, dir)
		})
	}

	missing := s3dir.New(client, s3dir.Flat("flat", "nowhere/"))
	if _, err := missing.IndexBin(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("IndexBin under a missing prefix: want fs.ErrNotExist, got %v", err)
	}
}

func TestUbuntu_Local(t *testing.T) {
	ubuntu := ocidirtest.UbuntuDir(t)
	client := startVersitygw(t)
	root, err := osfs.NewRoot(ubuntu)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	namer := s3dir.Flat("ubuntu", "")
	upload(t, client, namer, readTree(t, ubuntu))
	ocidirtest.TestSameAs(t, fsdir.New(root), s3dir.New(client, namer))
}
