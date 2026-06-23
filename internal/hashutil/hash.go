// Package hashutil provides small SHA-256 helpers shared by both tools.
package hashutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// File returns the lowercase hex SHA-256 of a file's contents.
func File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Bytes returns the lowercase hex SHA-256 of an in-memory buffer.
func Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
