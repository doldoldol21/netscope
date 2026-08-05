// SHA-256 verification of downloaded release assets against the release's
// checksums.txt (produced by CI at package time, shasum -a 256 format).
package update

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// FindChecksum extracts the SHA-256 hex digest for the named file from a
// checksums.txt body. Lines look like "<64-hex>  <name>" (shasum also emits a
// "*<name>" binary-mode marker, which is accepted). Returns ok=false when the
// file has no entry.
func FindChecksum(sums, name string) (digest string, ok bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == name || fields[1] == "*"+name {
			d := strings.ToLower(fields[0])
			if len(d) == sha256.Size*2 {
				return d, true
			}
		}
	}
	return "", false
}

// VerifyFileSHA256 hashes the file at path and compares it to the expected hex
// digest, returning a descriptive error on mismatch.
func VerifyFileSHA256(path, wantHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(wantHex))) != 1 {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantHex)
	}
	return nil
}
