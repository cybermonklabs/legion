package omp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const blobPrefix = "blob:sha256:"

var imageBlobHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// The loader restores blobs before the context is built, including blobs in
// native payloads and entry patches. The request later omits unreadable inline
// image blocks, including blocks whose references could not be resolved.
type sessionBlobs struct {
	read  func(string) ([]byte, error)
	cache map[blobSite]string
}

type blobSite struct {
	ref string
	url bool
}

// Never send a noncanonical reference to the caller's store, where the hash
// may become a file name.
func (b *sessionBlobs) resolve(data string, asURL bool) (string, error) {
	hash, ok := strings.CutPrefix(data, blobPrefix)
	if !ok || !imageBlobHash.MatchString(hash) {
		return data, nil
	}
	site := blobSite{data, asURL}
	if cached, ok := b.cache[site]; ok {
		return cached, nil
	}
	body, err := b.read(hash)
	if err != nil {
		return "", fmt.Errorf("resolve session image %s: %w", hash, err)
	}
	if body != nil {
		if asURL {
			data = strings.ToValidUTF8(string(body), "\uFFFD")
		} else {
			data = base64.StdEncoding.EncodeToString(body)
		}
	}
	if b.cache == nil {
		b.cache = make(map[blobSite]string)
	}
	b.cache[site] = data
	return data, nil
}

func (b *sessionBlobs) restoreEntry(line []byte) ([]byte, error) {
	if b.read == nil || (!bytes.Contains(line, []byte(blobPrefix)) && !bytes.Contains(line, []byte(`\u00`))) {
		return line, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var entry any
	if err := decoder.Decode(&entry); err != nil {
		return nil, err
	}
	restored, changed, err := b.restore(entry, "")
	if err != nil || !changed {
		return line, err
	}
	return json.Marshal(restored)
}

// restore follows resolvedImageSiteOf and blobRefSitesIn. Array elements
// inherit their holder's key. Snapcompact frames are not resolved by this pass.
func (b *sessionBlobs) restore(value any, key string) (any, bool, error) {
	changed := false
	switch node := value.(type) {
	case []any:
		for i, child := range node {
			restored, replace, err := b.restore(child, key)
			if err != nil {
				return nil, false, err
			}
			if replace {
				node[i], changed = restored, true
			}
		}
	case map[string]any:
		kind, _ := node["type"].(string)
		mime, _ := node["mimeType"].(string)
		_, hasData := node["data"].(string)
		field := ""
		switch {
		case hasData && (key == "content" && kind == "image" ||
			key == "images" && (kind == "image" || strings.HasPrefix(strings.ToLower(mime), "image/"))):
			field = "data"
		case kind == "image_generation_call":
			field = "result"
		}
		if ref, ok := node[field].(string); field != "" && ok && strings.HasPrefix(ref, blobPrefix) {
			restored, err := b.resolve(ref, false)
			if err != nil {
				return nil, false, err
			}
			if restored != ref {
				node[field], changed = restored, true
			}
			if field == "data" {
				return node, changed, nil
			}
		}
		for childKey, child := range node {
			restored, replace, err := b.restore(child, childKey)
			if err != nil {
				return nil, false, err
			}
			if replace {
				node[childKey], changed = restored, true
			}
		}
	case string:
		if key == "image_url" {
			restored, err := b.resolve(node, true)
			return restored, restored != node, err
		}
	}
	return value, changed, nil
}
