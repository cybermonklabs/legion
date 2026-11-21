package supervise

// A death the runtime read a memory or disk cause of is told in plain words, with its numbers, in
// the role's next task and in the claim's record of it, before the runtime's own detail
// (dispatch://INFRA-1117 R9). The words are the daemon's: what each kind of end did, and the number
// it passed.

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A role whose process dies in a turn of its task gets that task again behind the sentence of its
// interruption, which says why the process ended. Where the runtime read a cause the daemon tells
// in plain words, that sentence opens with it and its numbers: the memory limit of a `roles`
// container killed for memory, the use and the request of the container a node-pressure eviction
// named over its memory or its ephemeral-storage request, and the disk limit of an eviction for
// the pod's own disk. The task of either disk eviction also says to keep large files under
// $TMPDIR, and that of the disk limit first says what the limit counts. The runtime's own detail
// follows, quoted. The memory limit's sentence is the task's one statement that the pod ran out
// of memory: the out-of-memory note behind it keeps its guidance and drops its own statement,
// which an out-of-memory end the daemon names no limit of still carries whole. The claim's record
// of the death, which the operator's listing and a failed claim's notices read, opens with the
// same sentence. A death the daemon names no cause of reads as it always did.
func TestPlainCauseADeathForMemoryOrDiskIsToldInTheRolesNextTaskBeforeTheRuntimesDetail(t *testing.T) {
	const detail = "pod legion-legion-legion-208 (uid uid-pod-1) Failed: container roles terminated (Error, exit code 137)"
	quoted := strconv.Quote("gone: " + detail)
	for _, tc := range []struct {
		name        string
		cause       runtime.DeathCause
		outOfMemory bool
		// told is what the task says of the death, after the sentence of its interruption; note
		// is what stands between that sentence and the task itself; and recorded is the claim's
		// record of the death.
		told, note, recorded string
	}{
		{
			name:  "the roles container killed for memory",
			cause: runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 19 << 30}, outOfMemory: true,
			told: "Your process ended because the pod's roles container, which runs every role of this issue, was killed because it used " +
				"more than its 19 GiB memory limit. The runtime's detail: " + quoted + ".",
			note: outOfMemoryGuidance,
			recorded: "the pod's roles container, which runs every role of this issue, was killed because it used more than its 19 GiB " +
				"memory limit. The runtime's detail: gone: " + detail,
		},
		{
			name: "a node-pressure eviction of the pod's own memory use",
			cause: runtime.DeathCause{Kind: runtime.DeathMemoryRequest, Container: runtime.IssuePodContainer,
				Use: 9437188 << 10, Request: 8 << 30},
			told: "Your process ended because the kubelet evicted the pod for node memory pressure because its roles container was using 9 GiB, " +
				"more than its 8 GiB memory request. The runtime's detail: " + quoted + ".",
			recorded: "the kubelet evicted the pod for node memory pressure because its roles container was using 9 GiB, more than its 8 GiB " +
				"memory request. The runtime's detail: gone: " + detail,
		},
		{
			name: "a node-pressure eviction of the pod's own disk use over its request",
			cause: runtime.DeathCause{Kind: runtime.DeathDiskRequest, Container: runtime.IssuePodContainer,
				Use: 7 << 30, Request: 6 << 30},
			told: "Your process ended because the kubelet evicted the pod for node disk pressure because its roles container was using 7 GiB " +
				"of its own disk, outside $TMPDIR, more than its 6 GiB ephemeral storage request. " +
				"Keep large files under $TMPDIR, on the issue's volume. The runtime's detail: " + quoted + ".",
			recorded: "the kubelet evicted the pod for node disk pressure because its roles container was using 7 GiB of its own disk, " +
				"outside $TMPDIR, more than its 6 GiB ephemeral storage request. The runtime's detail: gone: " + detail,
		},
		{
			name:  "an eviction for the pod's own disk",
			cause: runtime.DeathCause{Kind: runtime.DeathDiskLimit, Limit: 10 << 30},
			told: "Your process ended because the kubelet evicted the pod because its container's own disk, outside $TMPDIR, passed its 10 GiB limit. " +
				"That limit counts each role's home, /tmp and /var/tmp, the container's logs and what is written in the container's own layer. " +
				"Keep large files under $TMPDIR, on the issue's volume. The runtime's detail: " + quoted + ".",
			recorded: "the kubelet evicted the pod because its container's own disk, outside $TMPDIR, passed its 10 GiB limit. " +
				"The runtime's detail: gone: " + detail,
		},
		{
			name:     "an out-of-memory end the daemon names no limit of",
			told:     "Your process ended because the process died: " + quoted + ".",
			note:     outOfMemoryTask,
			recorded: "gone: " + detail, outOfMemory: true,
		},
		{
			name:     "a death the daemon names no cause of",
			told:     "Your process ended because the process died: " + quoted + ".",
			recorded: "gone: " + detail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
			h.must(StreamTurnStart{Claim: testToken})

			h.must(RuntimeObservation{Observation: runtime.Observation{
				Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), Detail: detail, Cause: tc.cause, OutOfMemory: tc.outOfMemory,
			}})
			h.relaunched()

			task := h.wantPrompts(2)[1].Message
			if opens := "Your previous turn on this task was interrupted. " + tc.told; !strings.HasPrefix(task, opens) {
				t.Fatalf("the relaunched role's task is %q, want it to open with %q", task, opens)
			}
			if !strings.HasSuffix(task, "\n\n"+tc.note+"implement the plan") {
				t.Errorf("the relaunched role's task is %q, want the note %q and then the task itself at its end", task, tc.note)
			}
			if says := strings.Count(task, "ran out of memory") + strings.Count(task, "memory limit"); says > 1 {
				t.Errorf("the relaunched role's task says %d times that its pod ran out of memory, want once at most: %q", says, task)
			}
			if got := h.claim().LastDeathCause; got != tc.recorded {
				t.Errorf("the claim records its death as %q, want %q", got, tc.recorded)
			}
			if stored := h.store.load(testToken).LastDeathCause; stored != tc.recorded {
				t.Errorf("the stored claim records its death as %q, want %q", stored, tc.recorded)
			}
			if deaths := h.budgets().Deaths; deaths != 1 {
				t.Errorf("the death charged %d deaths, want 1: a death the daemon tells in plain words is still charged", deaths)
			}
		})
	}
}

// A death the daemon tells in plain words is told in the role's next task even where it cut off
// no turn: the claim was ready, its task sent and not yet confirmed by a turn, so the death took
// nothing back and found no turn running, which leaves no record of a let-go by the rule every
// other end keeps (recordEnd). The death is charged all the same, since the claim holds a task,
// so the record is kept (recordEnd, EndedWork.Death), and the task the relaunched agent is sent
// next opens with it: that no turn ran, why the process ended, and, for a disk eviction, where
// large files go.
func TestPlainCauseADeathWithATaskNoTurnConfirmedIsToldInTheNextTask(t *testing.T) {
	const detail = "no pod of sandbox legion-legion-legion-208 (operatingMode Running)"
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
	if p := h.pending(); p.ID == "" || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v, want the task sent and unconfirmed: this case is about a task no turn confirmed", p)
	}

	h.must(RuntimeObservation{Observation: runtime.Observation{
		Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), Detail: detail,
		Cause: runtime.DeathCause{Kind: runtime.DeathDiskLimit, Limit: 10 << 30},
	}})
	h.relaunched()

	if deaths := h.budgets().Deaths; deaths != 1 {
		t.Errorf("the death charged %d deaths, want 1: the claim held a task", deaths)
	}
	prompts := h.wantPrompts(2)
	next := prompts[len(prompts)-1].Message
	for _, says := range []string{
		"Your previous process ended because the kubelet evicted the pod because its container's own disk, outside $TMPDIR, passed its 10 GiB limit. " +
			"That limit counts each role's home, /tmp and /var/tmp, the container's logs and what is written in the container's own layer. " +
			"Keep large files under $TMPDIR, on the issue's volume. The runtime's detail: " + strconv.Quote("gone: "+detail) + ".",
		"You were not in a turn then.",
	} {
		if !strings.Contains(next, says) {
			t.Errorf("the relaunched role is next sent %q, want it to say %q", next, says)
		}
	}
	if !strings.HasSuffix(next, "implement the plan") {
		t.Errorf("the relaunched role is next sent %q, want its task at the end of it", next)
	}
}

// A role whose pod the kubelet evicted for its disk limit is told in its next task, between the
// sentence of that death and the runtime's detail, what the limit counts and where large files go
// (dispatch://INFRA-1117 R11): each role's home, /tmp and /var/tmp, the container's logs and its
// own layer, and $TMPDIR. Where the runtime read that the pod kept its test database on a volume
// the limit counts (runtime.DeathCause.TestDatabaseCounted), the note names the test database
// too, and the databases test runs never drop that fill it. Where the pod kept it on a disk of its
// own, or ran none, the note names no database at all.
func TestPGDiskTheNoteOfADiskLimitEvictionNamesWhatTheLimitCounts(t *testing.T) {
	const detail = "pod legion-legion-legion-208 (uid uid-pod-1) Failed: container roles terminated (Error, exit code 137)"
	const lead = "the kubelet evicted the pod because its container's own disk, outside $TMPDIR, passed its 10 GiB limit."
	counted := []string{"each role's home", "/tmp", "/var/tmp", "the container's logs", "the container's own layer", "$TMPDIR"}
	for _, tc := range []struct {
		name                string
		testDatabaseCounted bool
		names, namesNoneOf  []string
	}{
		{name: "the test database on a disk of its own, or none", names: counted, namesNoneOf: []string{"database"}},
		{name: "the test database on a counted volume", testDatabaseCounted: true,
			names: slices.Concat(counted, []string{"test database", "never drop"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
			h.must(StreamTurnStart{Claim: testToken})

			h.must(RuntimeObservation{Observation: runtime.Observation{
				Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), Detail: detail,
				Cause: runtime.DeathCause{Kind: runtime.DeathDiskLimit, Limit: 10 << 30, TestDatabaseCounted: tc.testDatabaseCounted},
			}})
			h.relaunched()

			task := h.wantPrompts(2)[1].Message
			start, end := strings.Index(task, lead), strings.Index(task, "The runtime's detail:")
			if start < 0 || end < start {
				t.Fatalf("the relaunched role's task is %q, want the death's sentence %q and then the runtime's detail", task, lead)
			}
			note := task[start+len(lead) : end]
			for _, name := range tc.names {
				in := note
				if name == "/tmp" {
					// /var/tmp holds /tmp, so /tmp is looked for with /var/tmp taken out.
					in = strings.ReplaceAll(note, "/var/tmp", "")
				}
				if !strings.Contains(in, name) {
					t.Errorf("the note behind the death's sentence is %q, want it to name %q", note, name)
				}
			}
			for _, name := range tc.namesNoneOf {
				if strings.Contains(note, name) {
					t.Errorf("the note behind the death's sentence is %q, want it to name no %q", note, name)
				}
			}
		})
	}
}

// The sizes in those sentences are the daemon's formatting of the bytes the runtime parsed, in
// GiB or MiB as the amount reads, to two decimals with the zeros that end them dropped, so a use
// just over a request never reads as equal to it.
func TestPlainCauseSizesReadAsAPersonReadsThem(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{19 << 30, "19 GiB"},
		{16082620 << 10, "15.34 GiB"},
		{8<<30 + 43<<20, "8.04 GiB"},
		{768 << 20, "768 MiB"},
		{1536 << 10, "1.5 MiB"},
		{512, "512 bytes"},
	} {
		if got := plainSize(tc.bytes); got != tc.want {
			t.Errorf("plainSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
