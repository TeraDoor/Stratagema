package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a short random id like "lock-3f9a2b1c8d4e" — random, not
// sequential, so two processes racing to create rows never collide.
func newID(prefix string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b), nil
}
