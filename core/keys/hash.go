package keys

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashToken returns the hex-encoded SHA-256 digest of a raw bearer token.
// All secondary datastores (Postgres key column, Redis rate-limit and cache
// namespaces) store and key by this hash so that a Postgres dump or a Redis
// SCAN/MONITOR never exposes a live bearer credential.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
