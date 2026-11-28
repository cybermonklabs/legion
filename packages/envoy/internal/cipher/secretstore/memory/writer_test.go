package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// openWriter writes an empty memory secrets file of t's own and opens the memory store's writer
// over it, as the CLI opens it from the broker's settings.
func openWriter(t *testing.T) (*memory.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory-secrets.json")
	writeFile(t, path, `{"secrets": []}`)
	w, err := memory.Backend{}.OpenWriter(context.Background(), secretstore.Settings{
		Store: memory.Name, Prefix: prefix, Fields: map[string]string{"memory_secrets_file": path},
	}, secretstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w.(*memory.Writer), path
}

// TestTheAppliedHookRunsOnceEachWriteChangedTheFile: a writer opened under Hooks.Applied runs it
// once for each write that changed the file, naming its kind, with the file already holding the
// write; a write the store refuses runs it not at all.
func TestTheAppliedHookRunsOnceEachWriteChangedTheFile(t *testing.T) {
	var applied []string
	var path string
	t.Cleanup(memory.SwapHooks(memory.Hooks{Applied: func(call string) {
		applied = append(applied, call)
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			t.Errorf("Applied(%s) ran with the file unread: %v", call, err)
		}
	}}))
	w, opened := openWriter(t)
	path = opened
	ctx := context.Background()
	id := prefix + "deel-api-key"
	c := w.PrepareCreate(id, map[string]string{"owner": "ada@example.com", "tier": "agent"})
	if _, err := c.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if err := w.Set(ctx, id, "v2"); err != nil {
		t.Fatal(err)
	}
	held, err := w.CurrentTags(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetTags(ctx, id, held, map[string]string{"judge": "600"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := w.Restore(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := w.Set(ctx, prefix+"missing", "v"); err == nil {
		t.Fatal("a set of a secret the store does not hold answered nil")
	}
	if want := []string{"create", "set", "tags", "delete", "restore"}; !slices.Equal(applied, want) {
		t.Fatalf("Applied ran for %v; want %v", applied, want)
	}
}

// TestAWriteNeverExposesAPartialFile: a reader of the file beside 50 writes sees only whole files:
// every read parses, and holds every secret written before it began.
func TestAWriteNeverExposesAPartialFile(t *testing.T) {
	w, path := openWriter(t)
	ctx := context.Background()
	var written sync.Map
	done := make(chan struct{})
	var problems []string
	var mu sync.Mutex
	go func() {
		defer close(done)
		for i := range 50 {
			id := fmt.Sprintf("%skey-%02d", prefix, i)
			c := w.PrepareCreate(id, map[string]string{"owner": "shared", "tier": "agent"})
			if _, err := c.Begin(ctx); err != nil {
				mu.Lock()
				problems = append(problems, err.Error())
				mu.Unlock()
				return
			}
			if err := c.Store(ctx, "v"); err != nil {
				mu.Lock()
				problems = append(problems, err.Error())
				mu.Unlock()
				return
			}
			written.Store(id, true)
		}
	}()
	reads := 0
	for {
		select {
		case <-done:
			mu.Lock()
			defer mu.Unlock()
			if len(problems) > 0 {
				t.Fatalf("a write failed: %v", problems)
			}
			if reads == 0 {
				t.Fatal("UNKNOWN: the reader read nothing beside the writes")
			}
			return
		default:
		}
		var before []string
		written.Range(func(k, _ any) bool { before = append(before, k.(string)); return true })
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Secrets []memory.Secret `json:"secrets"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("a read beside the writes found a partial file (%d bytes): %v", len(data), err)
		}
		names := map[string]bool{}
		for _, s := range f.Secrets {
			names[s.Name] = true
		}
		for _, id := range before {
			if !names[id] {
				t.Fatalf("a read lost %s, which a write finished before it began", id)
			}
		}
		reads++
	}
}

// TestAHungCallAnswersItsContextsEndAndWritesNothing: a call HangNext names answers nothing until
// its context ends, then the context's error, and the file holds no write of it; the call after it
// runs as any other.
func TestAHungCallAnswersItsContextsEndAndWritesNothing(t *testing.T) {
	w, _ := openWriter(t)
	id := prefix + "deel-api-key"
	c := w.PrepareCreate(id, map[string]string{"owner": "ada@example.com", "tier": "agent"})
	if _, err := c.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.HangNext("create")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Store(ctx, "v1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a hung create answered %v; want its context's deadline", err)
	}
	if _, err := w.Describe(context.Background(), id, secretstore.ReadOptions{}); !errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("after a hung create Describe = %v; want ErrNotFound: the hung call wrote nothing", err)
	}
	if err := c.Store(context.Background(), "v1"); err != nil {
		t.Fatalf("the create after the hung one: %v", err)
	}
}
