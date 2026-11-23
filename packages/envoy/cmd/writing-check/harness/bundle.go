package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// bundlePart is one part of the writing check's bundle the digest reads: a package directory's Go
// files without tests, or, where files is set, those files alone.
type bundlePart struct {
	dir   string
	files []string
	extra []string
}

// bundleParts is the bundle's files, in the order the digest reads them, as paths relative to the
// module root (packages/envoy): the prose reader, the rules, the code stage, the model reader and
// its prompt, the orchestrator's bundle, checker and actor files, and the baseline.
var bundleParts = []bundlePart{
	{dir: "internal/dispatch/writingcheck/prose"},
	{dir: "internal/dispatch/writingcheck/rules"},
	{dir: "internal/dispatch/writingcheck/candidates"},
	{dir: "internal/dispatch/writingcheck/reader", extra: []string{"prompt.md"}},
	{dir: "internal/dispatch/writingcheck", files: []string{"bundle.go", "checker.go", "actor.go"}},
	{dir: "internal/dispatch/writingcheck/baseline"},
}

// BundleFile is one file of the bundle and its SHA-256.
type BundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// bundleFiles is the bundle's files under root, in the digest's order: each part's Go files
// without tests, sorted by name (or the files the part names), then its extra files.
func bundleFiles(root string) ([]BundleFile, error) {
	var out []BundleFile
	for _, part := range bundleParts {
		names := part.files
		if names == nil {
			entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(part.dir)))
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
					names = append(names, e.Name())
				}
			}
			slices.Sort(names)
			if len(names) == 0 {
				return nil, fmt.Errorf("%s holds no Go file", part.dir)
			}
		}
		for _, name := range append(slices.Clone(names), part.extra...) {
			rel := path.Join(part.dir, name)
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				return nil, err
			}
			out = append(out, BundleFile{Path: rel, SHA256: sha256Hex(data)})
		}
	}
	return out, nil
}

// BundleDigest is the bundle's digest: the SHA-256 of the lines "<sha256>  <path>\n" of its files
// in order (filesDigest), which is what `sha256sum <files> | sha256sum` prints run from the module
// root.
func BundleDigest(root string) (string, []BundleFile, error) {
	files, err := bundleFiles(root)
	if err != nil {
		return "", nil, fmt.Errorf("reading the bundle under %s (give --root, the module root packages/envoy): %w", root, err)
	}
	return filesDigest(files), files, nil
}

// filesDigest is the SHA-256 of the lines "<sha256>  <path>\n" of files in order, which is what
// `sha256sum <files> | sha256sum` prints for them.
func filesDigest(files []BundleFile) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s  %s\n", f.SHA256, f.Path)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// moduleRoot is the nearest directory at or above the working directory that holds the bundle
// (packages/envoy), where `harness score` reads the bundle change 7a runs in process.
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return dirAbove(wd, "internal/dispatch/writingcheck/bundle.go", "run from packages/envoy")
}

// dirAbove is the nearest directory at or above dir that holds marker, a slash path; hint is what
// the refusal tells the caller to do where none holds it.
func dirAbove(dir, marker, hint string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, filepath.FromSlash(marker))); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no directory at or above %s holds %s: %s", dir, marker, hint)
		}
	}
}
