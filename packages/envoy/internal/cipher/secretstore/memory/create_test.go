package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// openCreateWriter is a memory writer over an empty secrets file of its own, under prefix test/.
func openCreateWriter(t *testing.T) (*memory.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := memory.Backend{}.OpenWriter(context.Background(), secretstore.Settings{
		Store: memory.Name, Prefix: "test/", Fields: map[string]string{"memory_secrets_file": path},
	}, secretstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return w.(*memory.Writer), path
}

// TestUnsettledNamesTheSecretWhileTheCreateIsInFlight: a second signal ends the form without
// waiting for the create's Store, and prints Unsettled, so while Store is in flight, its file
// written and its answer not yet given, Unsettled names the secret it may have made and the
// command that checks it. Before Store begins and after it answers success it names nothing.
func TestUnsettledNamesTheSecretWhileTheCreateIsInFlight(t *testing.T) {
	applied, release := make(chan struct{}), make(chan struct{})
	restore := memory.SwapHooks(memory.Hooks{Applied: func(call string) {
		if call == "create" {
			close(applied)
			<-release
		}
	}})
	defer restore()
	w, path := openCreateWriter(t)
	c := w.PrepareCreate("test/in-flight", nil)
	if got := c.Unsettled(); got != "" {
		t.Fatalf("before Store, Unsettled = %q, want nothing", got)
	}
	done := make(chan error, 1)
	go func() { done <- c.Store(context.Background(), "in-flight-value") }()
	<-applied
	during := c.Unsettled()
	raw, err := os.ReadFile(path)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "test/in-flight") {
		t.Fatal("control: the create had not written the secret when Unsettled was read, so the row reads nothing")
	}
	for _, want := range []string{"IN_FLIGHT", "`agent-secrets secret show IN_FLIGHT`"} {
		if !strings.Contains(during, want) {
			t.Errorf("Unsettled while Store was in flight = %q, want it to name %q", during, want)
		}
	}
	if got := c.Unsettled(); got != "" {
		t.Errorf("after Store answered success, Unsettled = %q, want nothing", got)
	}
}

// TestUnsettledReadsAsStoreRuns: Unsettled is read from another goroutine while Store runs (the
// second signal's handler), so under -race the two share nothing unguarded. A lost answer leaves
// Unsettled naming the secret.
func TestUnsettledReadsAsStoreRuns(t *testing.T) {
	w, _ := openCreateWriter(t)
	w.LoseNext("create")
	c := w.PrepareCreate("test/race", nil)
	start, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(start)
		for range 10000 {
			_ = c.Unsettled()
			runtime.Gosched()
		}
		close(done)
	}()
	<-start
	err := c.Store(context.Background(), "race-value")
	<-done
	if err == nil {
		t.Fatal("control: the lost answer did not fire, so the row reads nothing")
	}
	if got := c.Unsettled(); !strings.Contains(got, "`agent-secrets secret show RACE`") {
		t.Errorf("after a lost answer, Unsettled = %q, want the command that checks RACE", got)
	}
}
