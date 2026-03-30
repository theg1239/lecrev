package apikey

import (
	"crypto/sha256"
	"encoding/hex"
)

func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
