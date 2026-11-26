//go:build linux

package plan

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// readBound is the most CPU time of its own thread that FromBlocks may spend reading each plan of
// TestEachNeedOfAPlanIsReadInLinearTime.
const readBound = time.Second

// numbered is n words of prefix and a five-digit number, from 0: x00000, x00001, ….
func numbered(prefix string, n int) []string {
	words := make([]string, n)
	for i := range words {
		words[i] = fmt.Sprintf("%s%05d", prefix, i)
	}
	return words
}

// FromBlocks reads each `needs` word of a task once and looks it up in a set of what each of the
// task's four lists has kept already (resolved), so the read takes time linear in the words. Each
// plan below is read within readBound of the thread's own CPU time, and its task a needs each task
// its words name, once, and keeps each word that names no task, each word into another issue and
// each word that names an element, once: 50,000 words that name no task, each written twice; a
// task that needs each of 50,000 others; and 50,000 words into another issue, each written twice.
// The fault is a read that scans what the task has kept for each word, quadratic in the words of
// one block; the control is resolved with slices.Contains over a kept list in place of its set,
// under which the plan of that list's words fails its bound.
//
// A count of steps cannot hold this read: the scan's loop runs inside slices.Contains, in the
// standard library, where no count of this package runs. So the read is held to 1 s of the
// thread's CPU time. It catches that scan at these sizes; it proves no bound of time in general.
func TestEachNeedOfAPlanIsReadInLinearTime(t *testing.T) {
	// needing is a plan of a task a whose `needs` writes words, then a task of each id of ids.
	needing := func(words []string, ids []string) []Block {
		blocks := []Block{{ID: "a", Type: TaskBlockType, Attributes: map[string]string{"needs": strings.Join(words, " ")}}}
		for _, id := range ids {
			blocks = append(blocks, Block{ID: id, Type: TaskBlockType})
		}
		return blocks
	}
	outside := numbered("dispatch://ACME-2/artifact/plan-md#b-o", 50_000)
	for _, tc := range []struct {
		name                       string
		blocks                     []Block
		needs, unresolved, outside []string
	}{
		{name: "50,000 words that name no task, each twice", blocks: needing(append(numbered("x", 50_000), numbered("x", 50_000)...), nil), unresolved: numbered("x", 50_000)},
		{name: "one task needing each of 50,000 others", blocks: needing(numbered("t", 50_000), numbered("t", 50_000)), needs: numbered("t", 50_000)},
		{name: "50,000 words into another issue, each twice", blocks: needing(append(outside, outside...), nil), outside: outside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			before := threadCPU(t)
			p, err := FromBlocks("ACME-1", "plan-md", 1, tc.blocks)
			spent := threadCPU(t) - before
			if err != nil {
				t.Fatalf("FromBlocks(%s): %v", tc.name, err)
			}
			got := p.Tasks[0]
			if !reflect.DeepEqual(got.Needs, tc.needs) || !reflect.DeepEqual(got.Unresolved, tc.unresolved) || !reflect.DeepEqual(got.OutsideIssue, tc.outside) {
				t.Fatalf("FromBlocks(%s): a needs %d tasks and keeps %d words and %d outside, want %d, %d and %d", tc.name,
					len(got.Needs), len(got.Unresolved), len(got.OutsideIssue), len(tc.needs), len(tc.unresolved), len(tc.outside))
			}
			if spent > readBound {
				t.Fatalf("FromBlocks(%s) spent %v of its thread's CPU, want at most %v", tc.name, spent, readBound)
			}
			t.Logf("FromBlocks(%s) spent %v of its thread's CPU", tc.name, spent)
		})
	}
}
