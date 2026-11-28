package omp

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

const blobPrefix = "blob:sha256:"

var imageBlobHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// The pin leaves unresolved references in place. Never send a noncanonical
// reference to the caller's store, where the hash may become a file name.
func resolveImageBlob(data string, resolve func(string) ([]byte, error)) (string, error) {
	hash, ok := strings.CutPrefix(data, blobPrefix)
	if resolve == nil || !ok || !imageBlobHash.MatchString(hash) {
		return data, nil
	}
	body, err := resolve(hash)
	if err != nil {
		return "", fmt.Errorf("resolve session image %s: %w", hash, err)
	}
	if body == nil {
		return data, nil
	}
	return base64.StdEncoding.EncodeToString(body), nil
}
