package update

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestFindChecksum(t *testing.T) {
	sums := "" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  netscope-v1.0.0-app.zip\n" +
		"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB *binary-mode.zip\n" +
		"short  bad-entry.zip\n" +
		"malformed line with too many fields here\n"

	if d, ok := FindChecksum(sums, "netscope-v1.0.0-app.zip"); !ok || d[0] != 'a' {
		t.Errorf("plain entry: got %q ok=%v", d, ok)
	}
	if d, ok := FindChecksum(sums, "binary-mode.zip"); !ok || d[0] != 'b' {
		t.Errorf("binary-mode entry not found or not lowercased: got %q ok=%v", d, ok)
	}
	if _, ok := FindChecksum(sums, "bad-entry.zip"); ok {
		t.Error("entry with a non-sha256 digest must not match")
	}
	if _, ok := FindChecksum(sums, "missing.zip"); ok {
		t.Error("missing entry must not match")
	}
}

func TestVerifyFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asset.zip")
	body := []byte("release bytes")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])

	if err := VerifyFileSHA256(path, good); err != nil {
		t.Errorf("matching digest: %v", err)
	}
	if err := VerifyFileSHA256(path, "deadbeef"+good[8:]); err == nil {
		t.Error("mismatching digest must fail")
	}
	if err := VerifyFileSHA256(filepath.Join(t.TempDir(), "gone"), good); err == nil {
		t.Error("missing file must fail")
	}
}
