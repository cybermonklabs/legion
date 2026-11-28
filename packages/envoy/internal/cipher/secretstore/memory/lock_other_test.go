//go:build !unix

package memory_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// TestTheWriterIsRefusedWithoutAFileLock: on a system without a file lock between processes,
// OpenWriter refuses, naming the lock, before it reads or writes the file.
func TestTheWriterIsRefusedWithoutAFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory-secrets.json")
	_, err := memory.Backend{}.OpenWriter(context.Background(), secretstore.Settings{
		Store: memory.Name, Prefix: "example/agent-secrets/", Fields: map[string]string{"memory_secrets_file": path},
	}, secretstore.WriterOptions{})
	if err == nil || !strings.Contains(err.Error(), "file lock between processes") {
		t.Fatalf("OpenWriter = %v, want the refusal naming the missing file lock", err)
	}
}
