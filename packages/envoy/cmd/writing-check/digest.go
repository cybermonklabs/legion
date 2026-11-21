package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// bundlePart is one part of the bundle the digest reads: a package directory's Go files without
// tests, or, where files is set, those files alone.
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

// bundleFile is one file of the bundle and its SHA-256.
type bundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// bundleFiles is the bundle's files under root, in the digest's order: each part's Go files
// without tests, sorted by name (or the files the part names), then its extra files.
func bundleFiles(root string) ([]bundleFile, error) {
	var out []bundleFile
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
			out = append(out, bundleFile{Path: rel, SHA256: sha256Hex(data)})
		}
	}
	return out, nil
}

// bundleDigest is the bundle's digest: the SHA-256 of the lines "<sha256>  <path>\n" of its files
// in order, which is what `sha256sum <files> | sha256sum` prints run from the module root.
func bundleDigest(root string) (string, []bundleFile, error) {
	files, err := bundleFiles(root)
	if err != nil {
		return "", nil, fmt.Errorf("reading the bundle under %s (give --root, the module root packages/envoy): %w", root, err)
	}
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s  %s\n", f.SHA256, f.Path)
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

func runDigest(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check digest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "the module root, packages/envoy")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check digest: --root is the only flag, and nothing else")
		return 2
	}
	digest, files, err := bundleDigest(*root)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check digest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "bundle digest: %s\nfiles (%d, in digest order):\n", digest, len(files))
	for _, f := range files {
		fmt.Fprintf(stdout, "%s  %s\n", f.SHA256, f.Path)
	}
	return 0
}

// errNoDigest is the refusal of a final-test run whose bundle digest cannot be read.
var errNoDigest = errors.New("a final-test run names the bundle digest: run from packages/envoy or give --root")
