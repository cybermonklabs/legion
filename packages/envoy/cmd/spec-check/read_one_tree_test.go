//go:build linux || darwin

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// A reader of the spec files reads one tree: the files as they were before a run of print or as
// the run leaves them, never a mix of the two (R3-N3 of the acceptance), since it opens the
// directory one time and reads each file through it. The test reads a directory of 40 spec files
// again and again while another goroutine exchanges it with a directory of the same names and
// other texts, as print does, and requires each read to hold the texts of one of the two. The
// instrument must have seen the exchange: the reads hold each of the two trees at least once.
func TestAReaderReadsTheTreeBeforeAnExchangeOrAfterIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir, other := filepath.Join(root, "spec"), filepath.Join(root, "other")
	const files = 40
	for _, d := range []struct{ path, text string }{{dir, "old"}, {other, "new"}} {
		if err := os.MkdirAll(d.path, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range files {
			writeFile(t, filepath.Join(d.path, fmt.Sprintf("x-%02d.yaml", i)), []byte(d.text+"\n"))
		}
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var exchanges atomic.Int64
	wg.Go(func() {
		for !stop.Load() {
			if err := (osFileSystem{}).exchange(other, dir); err != nil {
				t.Errorf("exchange: %v", err)
				return
			}
			exchanges.Add(1)
		}
	})
	seen := map[string]int{}
	for range 2000 {
		set, err := readSet(io.Discard, dir)
		if err != nil {
			seen["an error: "+err.Error()]++
			continue
		}
		texts := map[string]int{}
		for _, content := range set.Files {
			texts[string(content)]++
		}
		switch {
		case len(set.Files) != files:
			seen[fmt.Sprintf("%d files", len(set.Files))]++
		case texts["old\n"] == files:
			seen["the tree before"]++
		case texts["new\n"] == files:
			seen["the tree after"]++
		default:
			seen["a mix of the two trees"]++
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("%d exchanges; the reads: %v", exchanges.Load(), seen)
	if seen["the tree before"] == 0 || seen["the tree after"] == 0 {
		t.Fatalf("the reads saw no exchange, so the test cannot see a mix: %v", seen)
	}
	if len(seen) != 2 {
		t.Errorf("a read held a tree that never was: %v", seen)
	}
}
