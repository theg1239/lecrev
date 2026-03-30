package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func Hash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func NormalizeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return append(json.RawMessage(nil), compact.Bytes()...)
	}
	return append(json.RawMessage(nil), raw...)
}
