package ociimagecopy

import (
	"context"
	"testing"

	"github.com/ngicks/oci-image-copy/pkg/cli"
	"github.com/ngicks/oci-image-copy/pkg/cli/skopeo"
)

func TestNewSkopeoWithCompression_ZeroConfigPassesThrough(t *testing.T) {
	t.Parallel()

	// The zero value injects NO default — the zstd default lives in
	// DefaultConfig, not in this constructor — so a zero config yields no
	// skopeo compression flags at all.
	sk := NewSkopeoWithCompression(cli.NewLocalInvoker(), CompressionConfig{})

	if sk.CompressionFormat != "" {
		t.Errorf("CompressionFormat = %q, want empty (no implicit default)", sk.CompressionFormat)
	}
	if sk.CompressionLevel != 0 {
		t.Errorf("CompressionLevel = %d, want 0 (no implicit default)", sk.CompressionLevel)
	}
	if sk.ForceCompression {
		t.Error("ForceCompression = true, want false (no implicit default)")
	}
}

func TestNewSkopeoWithCompression_Override(t *testing.T) {
	t.Parallel()

	sk := NewSkopeoWithCompression(cli.NewLocalInvoker(), CompressionConfig{
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
		t.Error(
			"ForceCompression = true, want false " +
				"(explicit override must not silently force recompression)",
		)
	}
}

func TestNewSkopeoWithCompression_FormatOnlyDoesNotDefaultLevel(t *testing.T) {
	t.Parallel()

	sk := NewSkopeoWithCompression(cli.NewLocalInvoker(), CompressionConfig{
		Format: "gzip",
	})

	if sk.CompressionFormat != "gzip" {
		t.Errorf("CompressionFormat = %q, want gzip", sk.CompressionFormat)
	}
	if sk.CompressionLevel != 0 {
		t.Errorf("CompressionLevel = %d, want 0", sk.CompressionLevel)
	}
}

func TestNewLocal_ThreadsCompression(t *testing.T) {
	t.Parallel()

	// NewLocal must thread the supplied compression config to its skopeo
	// verbatim (no implicit default injected here).
	local, err := NewLocal(context.Background(), LocalConfig{
		BaseDir:     t.TempDir(),
		Transport:   skopeo.TransportOci,
		OCIPath:     t.TempDir(),
		Compression: CompressionConfig{Format: "zstd", Level: 19, Force: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	sk, ok := local.Skopeo().(*skopeo.Skopeo)
	if !ok {
		t.Fatalf("Local.Skopeo() = %T, want *skopeo.Skopeo", local.Skopeo())
	}
	if sk.CompressionFormat != "zstd" || sk.CompressionLevel != 19 || !sk.ForceCompression {
		t.Errorf(
			"threaded compression = {%q %d %v}, want {zstd 19 true}",
			sk.CompressionFormat, sk.CompressionLevel, sk.ForceCompression,
		)
	}
}
