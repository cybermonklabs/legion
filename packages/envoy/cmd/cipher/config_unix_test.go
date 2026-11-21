//go:build unix

// packages/envoy/cmd/cipher/config_unix_test.go
//
// The settings file is read only when it is a regular file: a directory is not settings, and a
// FIFO someone left at the path would hold every command that opens it until somebody writes to
// it.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Serial: each row sets a process-wide variable, HOME, and clientConfig, the package-level settings
// loadClientConfig fills; t.Setenv panics under t.Parallel.
func TestAConfigPathThatIsNotARegularFileIsRefused(t *testing.T) {
	for name, create := range map[string]func(path string) error{
		"a directory": func(path string) error { return os.Mkdir(path, 0o700) },
		"a FIFO":      func(path string) error { return syscall.Mkfifo(path, 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Cleanup(func() { clientConfig = nil })
			path := filepath.Join(home, ".config", "agent-secrets", "config")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := create(path); err != nil {
				t.Fatal(err)
			}
			// A FIFO with no writer holds an open for reading until one comes: should the
			// read block, this test's cleanup is that writer, and the wait below fails it first.
			t.Cleanup(func() {
				if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					f.Close()
				}
			})

			done := make(chan error, 1)
			go func() { done <- loadClientConfig() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("loadClientConfig did not return within 5s for %s at %s", name, path)
			}
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "is not a regular file") {
				t.Fatalf("loadClientConfig = %v; want an error naming %s as not a regular file", err, path)
			}
			if clientConfig != nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("settings %v, error %v; want nothing read and the path refused, not missed", clientConfig, err)
			}
		})
	}
}
