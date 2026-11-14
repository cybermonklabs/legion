// Package testtypes tells a test that type-checks one package's source where the compiler left
// the types of everything that package imports, so the test reads those from the build cache
// instead of type-checking the whole dependency tree from source as well. admit's held-reason
// tests read the record package's words that way, and supervise's ProcessEnds guard reads its
// own package's.
//
// It is an ordinary package rather than a _test.go file so every test package can import it; Go
// cannot share test-only code across packages.
package testtypes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/importer"
	"io"
	"os"
	"os/exec"
	"sync"
)

// lists is the go command's answer for each pattern asked of it in this process, each asked on
// first use.
var lists struct {
	mu        sync.Mutex
	byPattern map[string]func() (importer.Lookup, error)
}

// CompiledDependencies is the lookup of the types the compiler left for pattern and for each
// package it imports: what importer.ForCompiler(fileset, "gc", lookup) opens when a test
// type-checks pattern's own source. The go command reports the file of each from its build
// cache, building any the test binary's own build did not. A test reads those rather than
// type-checking the dependency tree from its source, which supervise's dependencies make tens of
// seconds of a test binary's run (the source read supervise's sealed makes, in its table_test.go,
// is the longest test in that package by itself); the record package imports supervise, so its
// tree holds all of that.
//
// pattern is an import path, which the go command resolves from the process's working
// directory: a test's is its package's own, inside this module. The go command is asked once per
// pattern for the life of the process however many tests ask, and every caller gets that one
// answer, or that one failure. It takes no *testing.T, so the caller decides how to fail.
func CompiledDependencies(pattern string) (importer.Lookup, error) {
	lists.mu.Lock()
	list, found := lists.byPattern[pattern]
	if !found {
		list = sync.OnceValues(func() (importer.Lookup, error) { return listExports(pattern) })
		if lists.byPattern == nil {
			lists.byPattern = make(map[string]func() (importer.Lookup, error))
		}
		lists.byPattern[pattern] = list
	}
	lists.mu.Unlock()
	return list()
}

// listExports asks the go command where the compiler left the types of pattern and of each
// package it imports, and answers the lookup that opens them by import path.
func listExports(pattern string) (importer.Lookup, error) {
	listed, err := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Export", pattern).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("ask the go command where the dependencies of %s were compiled: %w: %s", pattern, err, exit.Stderr)
		}
		return nil, fmt.Errorf("ask the go command where the dependencies of %s were compiled: %w", pattern, err)
	}
	exports := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(listed))
	for decoder.More() {
		var dependency struct{ ImportPath, Export string }
		if err := decoder.Decode(&dependency); err != nil {
			return nil, fmt.Errorf("read the go command's answer for %s: %w", pattern, err)
		}
		if dependency.Export != "" {
			exports[dependency.ImportPath] = dependency.Export
		}
	}
	return func(path string) (io.ReadCloser, error) {
		export, found := exports[path]
		if !found {
			return nil, fmt.Errorf("the go command reported no compiled types for %s", path)
		}
		return os.Open(export)
	}, nil
}
