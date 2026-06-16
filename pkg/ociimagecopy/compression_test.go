package ociimagecopy

import (
	"context"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
)

func TestNewSkopeoWithCompression_Defaults(t *testing.T) {
	t.Parallel()

	sk := newSkopeoWithCompression(cli.NewLocalInvoker(), CompressionConfig{})

	if sk.CompressionFormat != DefaultCompressionFormat {
		t.Errorf("CompressionFormat = %q, want %q", sk.CompressionFormat, DefaultCompressionFormat)
	}
	if sk.CompressionLevel != DefaultCompressionLevel {
		t.Errorf("CompressionLevel = %d, want %d", sk.CompressionLevel, DefaultCompressionLevel)
	}
	if !sk.ForceCompression {
		t.Error("ForceCompression = false, want true (default must force already-gzip layers to zstd)")
	}
}

func TestNewSkopeoWithCompression_Override(t *testing.T) {
	t.Parallel()

	sk := newSkopeoWithCompression(cli.NewLocalInvoker(), CompressionConfig{
		Format: "gzip",
		Level:  9,
	})

	if sk.CompressionFormat != "gzip" {
		t.Errorf("CompressionFormat = %q, want gzip", sk.CompressionFormat)
	}
	if sk.CompressionLevel != 9 {
		t.Errorf("CompressionLevel = %d, want 9", sk.CompressionLevel)
	}
	if sk.ForceCompression {
		t.Error("ForceCompression = true, want false (explicit override must not silently force recompression)")
	}
}

func TestNewSkopeoWithCompression_FormatOnlyDoesNotDefaultLevel(t *testing.T) {
	t.Parallel()

	sk := newSkopeoWithCompression(cli.NewLocalInvoker(), CompressionConfig{
		Format: "gzip",
	})

	if sk.CompressionFormat != "gzip" {
		t.Errorf("CompressionFormat = %q, want gzip", sk.CompressionFormat)
	}
	if sk.CompressionLevel != 0 {
		t.Errorf("CompressionLevel = %d, want 0", sk.CompressionLevel)
	}
}

func TestNewLocal_DefaultCompression(t *testing.T) {
	t.Parallel()

	local, err := NewLocal(context.Background(), LocalConfig{
		BaseDir:   t.TempDir(),
		Transport: skopeo.TransportOci,
		OCIPath:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	sk, ok := local.Skopeo().(*skopeo.Skopeo)
	if !ok {
		t.Fatalf("Local.Skopeo() = %T, want *skopeo.Skopeo", local.Skopeo())
	}
	if sk.CompressionFormat != DefaultCompressionFormat {
		t.Errorf("CompressionFormat = %q, want %q", sk.CompressionFormat, DefaultCompressionFormat)
	}
	if sk.CompressionLevel != DefaultCompressionLevel {
		t.Errorf("CompressionLevel = %d, want %d", sk.CompressionLevel, DefaultCompressionLevel)
	}
	if !sk.ForceCompression {
		t.Error("ForceCompression = false, want true for a default NewLocal")
	}
}
