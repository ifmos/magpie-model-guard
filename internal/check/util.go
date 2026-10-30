package check

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHex returns n cryptographically random bytes as hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable; fall back is worse.
		panic(err)
	}
	return hex.EncodeToString(b)
}
