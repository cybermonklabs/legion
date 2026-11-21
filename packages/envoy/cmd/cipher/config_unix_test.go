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
)

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
			// read block, this test's cleanup is that writer, and the shared wait below
			// (returns, through testwait) fails it first.
			t.Cleanup(func() {
				if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					f.Close()
				}
			})

			err := returns(t, "loadClientConfig's answer for "+name+" at "+path, loadClientConfig)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "is not a regular file") {
				t.Fatalf("loadClientConfig = %v; want an error naming %s as not a regular file", err, path)
			}
			if clientConfig != nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("settings %v, error %v; want nothing read and the path refused, not missed", clientConfig, err)
			}
		})
	}
}
