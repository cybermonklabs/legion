package claude

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every text below is written for these tests.

// writeExport writes a session export of the rows of two sessions and the places of three cases.
func writeExport(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	enc := json.NewEncoder(gz)
	type row struct {
		session, host, role, kind, text string
	}
	index := map[string]int{}
	for _, r := range []row{
		{"s1", "a", "user", "user", "Build the gadget."},
		{"s1", "b", "user", "user", "Build the gadget."},
		{"s2", "a", "user", "user", "Another session's row."},
		{"s1", "a", "assistant", "thinking", "THINKING"},
		{"s1", "a", "custom:skill-prompt", "system", "A NOTICE"},
		{"s1", "a", "toolResult", "tool_result", "FACT: the gadget ships at noon. " + strings.Repeat("x", 3000)},
		{"s1", "a", "assistant", "assistant", "Done."},
		{"s1", "a", "assistant", "assistant", "AFTER THE PLACE"},
	} {
		line := map[string]any{"session_id": r.session, "index": index[r.session], "row": map[string]any{
			"session_id": r.session, "host": r.host, "ts": "2026-11-01 00:00:00", "role": r.role, "kind": r.kind, "text": r.text}}
		index[r.session]++
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rowsFile), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	places := `{"case_id":"c-early","session_id":"s1","status":"found","rows":2}
{"case_id":"c-late","session_id":"s1","status":"found","rows":6}
{"case_id":"c-missing","session_id":"s3","status":"session_missing","rows":0}
`
	if err := os.WriteFile(filepath.Join(dir, placesFile), []byte(places), 0o600); err != nil {
		t.Fatal(err)
	}
	counts := `{"sha256":{"session-rows.jsonl.gz":"r1","places.jsonl":"p1"}}`
	if err := os.WriteFile(filepath.Join(dir, countsFile), []byte(counts), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestExcerptsCutEachCaseAtItsPlace(t *testing.T) {
	dir := writeExport(t)
	got, err := Excerpts(dir, []string{"c-early", "c-late", "c-missing"}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["c-missing"]; ok {
		t.Error("a case whose session is missing has a session part")
	}
	early, late := got["c-early"].text, got["c-late"].text
	if strings.Count(early, "Build the gadget.") != 1 || strings.Contains(early, "FACT") {
		t.Errorf("the early case's part: %q", early)
	}
	if strings.Count(late, "Build the gadget.") != 1 {
		t.Errorf("a row the export holds once for each host is read once: %q", late)
	}
	for _, absent := range []string{"THINKING", "A NOTICE", "AFTER THE PLACE", "Another session's row."} {
		if strings.Contains(late, absent) {
			t.Errorf("the late case's part holds %q", absent)
		}
	}
	if !strings.Contains(late, "FACT: the gadget ships at noon.") || !strings.Contains(late, cutMark) || !strings.Contains(late, "Done.") {
		t.Errorf("the late case's part: the fact, its row cut at %d bytes, and the row before its place: %.300q", rowLimit, late)
	}
	// A limit keeps the newest bytes.
	cut, err := Excerpts(dir, []string{"c-late"}, 40)
	if err != nil {
		t.Fatal(err)
	}
	if text := cut["c-late"].text; !strings.HasPrefix(text, cutMark) || !strings.HasSuffix(text, "Done.\n") || len(text) > 40+len(cutMark) {
		t.Errorf("a part cut at 40 bytes: %q", text)
	}
}

func TestExcerptsRefuseAnExportShorterThanAPlace(t *testing.T) {
	dir := writeExport(t)
	places := `{"case_id":"c-beyond","session_id":"s1","status":"found","rows":99}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, placesFile), []byte(places), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Excerpts(dir, []string{"c-beyond"}, 1000); err == nil {
		t.Error("a place past the export's rows was let through")
	}
}

func TestCachedExcerptsReadTheCache(t *testing.T) {
	dir := writeExport(t)
	cache := filepath.Join(t.TempDir(), "excerpts.jsonl.gz")
	first, err := CachedExcerpts(dir, []string{"c-late", "c-missing"}, 1000, cache)
	if err != nil {
		t.Fatal(err)
	}
	// With the rows gone, the cache alone answers a read it covers.
	if err := os.Remove(filepath.Join(dir, rowsFile)); err != nil {
		t.Fatal(err)
	}
	again, err := CachedExcerpts(dir, []string{"c-late"}, 1000, cache)
	if err != nil {
		t.Fatalf("a read the cache covers: %v", err)
	}
	if again["c-late"].Digest() != first["c-late"].Digest() {
		t.Error("the cache answered another part")
	}
	// A case the cache was not cut for, or another limit, reads the export again.
	if _, err := CachedExcerpts(dir, []string{"c-early"}, 1000, cache); err == nil {
		t.Error("a read of a case the cache does not cover was answered from it")
	}
	if _, err := CachedExcerpts(dir, []string{"c-late"}, 999, cache); err == nil {
		t.Error("a read at another limit was answered from the cache")
	}
}
