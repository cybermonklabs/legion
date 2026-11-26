package codesurvival

import (
	"bytes"
	"context"
	"fmt"
	"runtime/metrics"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// The run writes no day row after the newest decided day (k6, R7): the first run measures days 0
// to 27 of the 60-day branch and leaves the 30 days after them with fingerprints alone; after the
// eval day refresher has run through today, the code evals hold rows of days 0 to 27 and of no
// later day. A window of measured days reads a number with no day waiting; one that holds 14
// measured days and 14 after them reads the measured days' number and names 14 waiting days; and
// a 7-day window with no measured day reads "no data", never 0, with its 7 days waiting.
func TestTheRunWritesNoRowAfterTheNewestDecidedDay(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	if err := r.registry.RefreshThrough(r.ctx, evals.RefresherDeps{Horizon: events.NewHorizon(r.database.Pool), Build: testBuild}, firstPass); err != nil {
		t.Fatalf("run the eval day refresher through %s: %v", firstPass.Format(time.RFC3339), err)
	}
	days := r.evalDays(widgetsProject)
	if len(days) != 28 || !days[0].Equal(day(0)) || !days[len(days)-1].Equal(day(27)) {
		t.Fatalf("the code evals hold rows of %d days, %v to %v, want of days 0 to 27 alone", len(days), days[0], days[len(days)-1])
	}
	measured := r.read(widgetsProject, "agents", day(0), day(28))
	waiting := r.read(widgetsProject, "agents", day(14), day(42))
	none := r.read(widgetsProject, "agents", day(40), day(47))
	partial := r.read(widgetsProject, "agents", day(14), day(28))
	switch {
	case measured.survival.Share == nil || measured.status.WaitingDays != 0:
		t.Errorf("the window of measured days reads %s with %d days waiting, want a number and none", value(measured.survival.Share), measured.status.WaitingDays)
	case waiting.survival.Share == nil || value(waiting.survival.Share) != value(partial.survival.Share) || waiting.status.WaitingDays != 14 || waiting.status.MeasuredDays != 14:
		t.Errorf("the window that holds waiting days reads %s over %d measured days with %d waiting, want the %s of its 14 measured days and 14 waiting",
			value(waiting.survival.Share), waiting.status.MeasuredDays, waiting.status.WaitingDays, value(partial.survival.Share))
	case none.survival.Share != nil || none.survival.Added != nil || none.rework.Share != nil || none.status.WaitingDays != 7:
		t.Errorf("the window with no measured day reads survival %s over %s lines and rework %s with %d days waiting, want no data and 7 waiting",
			value(none.survival.Share), value(none.survival.Added), value(none.rework.Share), none.status.WaitingDays)
	}
	t.Logf("3 windows and %d days of rows read: %s, %s over 14 days with 14 waiting, and %s", len(days), value(measured.survival.Share), value(waiting.survival.Share), value(none.survival.Share))
}

// bigPatch is a patch of exactly size bytes that adds the file path, as git writes a new file:
// lines of 64 bytes and a last one of the rest.
func bigPatch(path string, size int) []byte {
	header := func(lines int) string {
		return "diff --git a/" + path + " b/" + path + "\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/" + path +
			"\n@@ -0,0 +1," + strconv.Itoa(lines) + " @@\n"
	}
	full := 0
	for lines := (size - len(header(10000))) / 64; ; lines-- {
		if rest := size - len(header(lines+1)) - lines*64; rest >= 2 && rest <= 65 {
			full = lines
			break
		}
	}
	var patch bytes.Buffer
	patch.WriteString(header(full + 1))
	for i := range full {
		line := fmt.Sprintf("+%s line %07d", path, i)
		patch.WriteString(line + string(bytes.Repeat([]byte{'.'}, 63-len(line))) + "\n")
	}
	patch.WriteString("+" + string(bytes.Repeat([]byte{'x'}, size-patch.Len()-2)) + "\n")
	return patch.Bytes()
}

// The patch bytes the run holds stay under R4's bound, eight times the largest patch read plus 64
// MiB, and the process's heap under 1 GiB: a branch of 40 commits of one day, each a patch of 3
// MiB, 120 MiB in all, read at eight workers into a 64 MiB buffer, the first patch held back by the
// double until 28 others were answered. The other workers fill the buffer, 21 patches, then wait,
// each holding one; the first then arrives, and the stream holds 87 MiB at its peak, of the 88 the
// bound allows, and reads each patch once.
func TestThePatchBytesHeldStayUnderTheBound(t *testing.T) {
	t.Parallel()
	const (
		commits = 40
		size    = 3 << 20
	)
	var chain []codesurvivaltest.Commit
	patches := map[string][]byte{}
	for i := range commits {
		commit := codesurvivaltest.Commit{SHA: fmt.Sprintf("%040x", 0xb19000+i), CommittedAt: day(0).Add(10*time.Hour + time.Duration(i)*time.Minute)}
		if i > 0 {
			commit.Parents = []string{chain[i-1].SHA}
		}
		chain = append(chain, commit)
		patches[commit.SHA] = bigPatch(fmt.Sprintf("big/file%02d.txt", i), size)
	}
	r := newRig(t)
	r.github.Serve(widgets, codesurvivaltest.NewBranch(chain, patches, nil))
	r.project(widgetsProject, widgets, testGroups)
	r.github.Hold(chain[0].SHA, 28)

	ctx, cancel := context.WithCancel(r.ctx)
	var (
		mu   sync.Mutex
		peak uint64
		wg   sync.WaitGroup
	)
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	wg.Go(func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			metrics.Read(sample)
			mu.Lock()
			peak = max(peak, sample[0].Value.Uint64())
			mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	requests := r.passAt(day(40).Add(time.Hour))
	cancel()
	wg.Wait()

	for sha, reads := range patchReads(requests) {
		if reads != 1 {
			t.Errorf("the patch of %s was read %d times, want once", sha, reads)
		}
	}
	if reads := len(patchReads(requests)); reads != commits {
		t.Fatalf("the run read %d patches, want %d", reads, commits)
	}
	lines := r.logs.lines(t, "codesurvival: run")
	line := lines[len(lines)-1]
	held, bound := int64(line["peak_patch_bytes"].(float64)), int64(8*size+streamBuffer)
	if held > bound || held < streamBuffer || int64(line["bytes"].(float64)) < commits*size || line["stopped"] != stoppedNone {
		t.Errorf("the run held %d patch bytes at its peak of %v read, want at most the bound of %d and at least the buffer's %d: %v", held, line["bytes"], bound, streamBuffer, line)
	}
	if peak >= 1<<30 {
		t.Errorf("the heap's live objects peaked at %d bytes during the run, want under 1 GiB", peak)
	}
	t.Logf("%d patches of %d bytes read; the stream held %d bytes at its peak (bound %d); the heap's objects peaked at %d bytes", commits, size, held, bound, peak)
}
