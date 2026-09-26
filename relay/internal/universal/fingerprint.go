package universal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"
)

// Fingerprint identifies a payload shape: sha256 over the source name and the
// sorted, de-duplicated paths. Values and types are left out, so a field that
// flips between null and a string, a reordered object and a longer array all
// keep the same fingerprint.
func Fingerprint(source string, fields []Field) string {
	paths := make([]string, 0, len(fields))
	for _, f := range fields {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)

	// Length-prefix every part: a key may legally contain any byte, so no
	// separator character is safe.
	h := sha256.New()
	var n [4]byte
	for _, p := range append([]string{source}, paths...) {
		binary.BigEndian.PutUint32(n[:], uint32(len(p))) // #nosec G115 -- paths are capped at 1 MiB bodies
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
