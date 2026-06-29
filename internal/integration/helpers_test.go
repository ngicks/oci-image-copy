package integration

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// ────────────────────────────────────────────────────────────────────────────
// Cryptographic helpers
// ────────────────────────────────────────────────────────────────────────────

// generateKeyPair creates a fresh ed25519 key pair wrapped in a keyPair.
func generateKeyPair(t *testing.T) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("gossh.NewPublicKey: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("gossh.NewSignerFromKey: %v", err)
	}

	// Marshal private key to OpenSSH PEM format.
	privPEM, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("gossh.MarshalPrivateKey: %v", err)
	}
	var pemBuf bytes.Buffer
	if err := pem.Encode(&pemBuf, privPEM); err != nil {
		t.Fatalf("pem.Encode: %v", err)
	}

	return keyPair{
		signer:    signer,
		publicKey: sshPub,
		privPEM:   pemBuf.Bytes(),
	}
}

// ────────────────────────────────────────────────────────────────────────────
// OCI digest helpers
// ────────────────────────────────────────────────────────────────────────────

func sha256DigestStr(b []byte) string {
	return "sha256:" + sha256HexStr(b)
}

func sha256HexStr(b []byte) string {
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}

// ────────────────────────────────────────────────────────────────────────────
// Minimal gzip tar builder
// ────────────────────────────────────────────────────────────────────────────

// buildMinimalTarGz returns a valid gzip-compressed tar containing one small
// regular file ("hello", content "hi\n"). This produces a real, parseable
// OCI layer that skopeo will accept.
func buildMinimalTarGz(t *testing.T) []byte {
	t.Helper()
	content := []byte("hi\n")

	// Build a minimal POSIX tar header (512 bytes).
	var header [512]byte
	copy(header[0:100], "hello")         // name
	copy(header[100:108], "0000644\x00") // mode
	copy(header[108:116], "0000000\x00") // uid
	copy(header[116:124], "0000000\x00") // gid
	// size in octal, null-terminated
	copy(header[124:136], fmt.Sprintf("%011o\x00", len(content)))
	copy(header[136:148], "00000000000\x00") // mtime
	header[156] = '0'                        // type flag = regular file
	copy(header[257:265], "ustar  \x00")     // magic

	// Compute checksum: sum all bytes with checksum field treated as spaces.
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	var cksum uint32
	for _, b := range header {
		cksum += uint32(b)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", cksum))

	// data block (padded to 512 bytes)
	var dataBlock [512]byte
	copy(dataBlock[:], content)

	// end-of-archive: two 512-byte zero blocks
	var eoa [1024]byte

	tarData := make([]byte, 0, 2048)
	tarData = append(tarData, header[:]...)
	tarData = append(tarData, dataBlock[:]...)
	tarData = append(tarData, eoa[:]...)

	// gzip compress
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(tarData); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
