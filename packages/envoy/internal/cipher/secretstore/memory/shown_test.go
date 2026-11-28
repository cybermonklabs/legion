package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// TestTheWriterNamesItsFileOnlyAsTheSettingsLetIt: the writer's file is a field of the broker's
// settings, so its identity and every error of its reads and writes - a file gone, one that is no
// memory secrets file, a lock it cannot open, a directory it cannot write - name the file as the
// settings' Words print it, never as its path. Control: with Words that print the path, each names
// it, so the check sees the path in each.
func TestTheWriterNamesItsFileOnlyAsTheSettingsLetIt(t *testing.T) {
	const planted = "hunter2pw-memorypath-91d4"
	const prefix = "example/agent-secrets/"
	const held = prefix + "held"
	ctx := context.Background()
	open := func(t *testing.T, words func(string) string) (w *memory.Writer, dir, path string) {
		t.Helper()
		dir = filepath.Join(t.TempDir(), planted+"-dir")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, planted+".json")
		if err := os.WriteFile(path, []byte(`{"secrets": [{"name": "`+held+`", "value": "v1", "tags": {"tier": "agent"}}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		opened, err := memory.Backend{}.OpenWriter(ctx, secretstore.Settings{
			Store: memory.Name, Prefix: prefix, Fields: map[string]string{"memory_secrets_file": path}, Words: words,
		}, secretstore.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return opened.(*memory.Writer), dir, path
	}
	failure := func(t *testing.T, err error) string {
		t.Helper()
		if err == nil {
			t.Fatal("the call answered no error, so the row reads nothing")
		}
		return err.Error()
	}
	rows := []struct {
		name string
		run  func(t *testing.T, words func(string) string) string
	}{
		{"the identity", func(t *testing.T, words func(string) string) string {
			w, _, _ := open(t, words)
			return w.Identity()
		}},
		{"a file gone before a read", func(t *testing.T, words func(string) string) string {
			w, _, path := open(t, words)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			_, err := w.List(ctx, prefix, secretstore.ReadOptions{})
			return failure(t, err)
		}},
		{"a file broken before a write", func(t *testing.T, words func(string) string) string {
			w, _, path := open(t, words)
			if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			return failure(t, w.Set(ctx, held, "v2"))
		}},
		{"a lock it cannot open", func(t *testing.T, words func(string) string) string {
			w, _, path := open(t, words)
			if err := os.Mkdir(path+".lock", 0o700); err != nil {
				t.Fatal(err)
			}
			return failure(t, w.Set(ctx, held, "v2"))
		}},
		{"a directory it cannot write", func(t *testing.T, words func(string) string) string {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a directory of mode 500")
			}
			w, dir, _ := open(t, words)
			if err := w.Set(ctx, held, "v2"); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return failure(t, w.Set(ctx, held, "v3"))
		}},
	}
	for _, row := range rows {
		t.Run(row.name+", beside an address that prints no field", func(t *testing.T) {
			const shown = "(not printed: the broker's address is not clean)"
			text := row.run(t, func(string) string { return shown })
			if strings.Contains(text, planted) || !strings.Contains(text, shown) {
				t.Errorf("%q: want the file named as %q and never as its path", text, shown)
			}
		})
		t.Run("control: "+row.name+", beside an address that prints the field", func(t *testing.T) {
			text := row.run(t, func(field string) string { return field })
			if !strings.Contains(text, planted) {
				t.Errorf("%q names no path, so the row cannot see one", text)
			}
		})
	}
}
