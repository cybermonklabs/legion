package fleetalert

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// writer is a Writer over a JSON log the test reads, as the daemon's own log is JSON: the alert
// policy reads the line out of `jsonPayload.msg`, so the test reads the same field.
func writer(t *testing.T) (*Writer, *testlog.Buffer) {
	t.Helper()
	buf := &testlog.Buffer{}
	return New(slog.New(slog.NewJSONHandler(buf, nil)), "ACME"), buf
}

// lines is every log entry the buffer holds, read as the policy reads them.
func lines(t *testing.T, buf *testlog.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, line)
		}
		out = append(out, entry)
	}
	return out
}

// The fixed text is the contract between this repository and the deployment repository, whose
// `legion/` program matches it in jsonPayload.msg. Neither side changes it alone.
func TestTheFixedTextIsTheContractWithTheAlertPolicy(t *testing.T) {
	if Prefix != "LEGION FLEET ALERT" {
		t.Fatalf("the fixed text is %q; the alert policy of the deployment repository's legion/ program matches %q in jsonPayload.msg, and the two move together", Prefix, "LEGION FLEET ALERT")
	}
}

func TestALineStartsWithTheFixedTextAndNamesItsCaseAndProject(t *testing.T) {
	w, buf := writer(t)

	w.Open(NoLiveSession, "", "sessions", 0)

	got := lines(t, buf)
	if len(got) != 1 {
		t.Fatalf("one open wrote %d lines, want 1: %s", len(got), buf.String())
	}
	msg, _ := got[0]["msg"].(string)
	if !strings.HasPrefix(msg, Prefix+": ") {
		t.Errorf("msg = %q, want it to start with %q", msg, Prefix+": ")
	}
	if !strings.Contains(msg, NoLiveSession.Sentence) {
		t.Errorf("msg = %q, want it to name the case %q", msg, NoLiveSession.Sentence)
	}
	if got[0]["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR: the policy reads the daemon's error lines", got[0]["level"])
	}
	if got[0]["case"] != NoLiveSession.Word {
		t.Errorf("case = %v, want %q", got[0]["case"], NoLiveSession.Word)
	}
	if got[0]["project"] != "ACME" {
		t.Errorf("project = %v, want ACME", got[0]["project"])
	}
	if _, named := got[0]["claim"]; named {
		t.Errorf("a case with no claim named one: %v", got[0]["claim"])
	}
	if got[0]["sessions"] != float64(0) {
		t.Errorf("the attributes the caller gave are not on the line: %v", got[0])
	}
}

func TestALineIsWrittenOnceUntilItsCaseEnds(t *testing.T) {
	w, buf := writer(t)

	if wrote := w.Open(ControllerUnreachable, "claim-1"); !wrote {
		t.Fatal("the first open wrote nothing")
	}
	if wrote := w.Open(ControllerUnreachable, "claim-1"); wrote {
		t.Error("the second open of an open case wrote a line")
	}
	if len(lines(t, buf)) != 1 {
		t.Fatalf("an open case wrote more than once: %s", buf.String())
	}

	w.Close(ControllerUnreachable, "claim-1")
	if wrote := w.Open(ControllerUnreachable, "claim-1"); !wrote {
		t.Error("the case came back and wrote nothing")
	}
	if len(lines(t, buf)) != 2 {
		t.Fatalf("the case that came back wrote %d lines, want 2: %s", len(lines(t, buf)), buf.String())
	}
}

func TestEachClaimOfOneCaseHasItsOwnLine(t *testing.T) {
	w, buf := writer(t)

	w.Open(NewerProcessRefused, "claim-1", "generation", 3)
	w.Open(NewerProcessRefused, "claim-2", "generation", 7)
	w.Open(NewerProcessRefused, "claim-1", "generation", 3)

	got := lines(t, buf)
	if len(got) != 2 {
		t.Fatalf("two claims of one case wrote %d lines, want 2: %s", len(got), buf.String())
	}
	if got[0]["claim"] != "claim-1" || got[1]["claim"] != "claim-2" {
		t.Errorf("the lines name %v and %v, want claim-1 and claim-2", got[0]["claim"], got[1]["claim"])
	}
}

// A closed case the writer never opened changes nothing: a caller that ends a case it never
// raised (a daemon that booted with the case already over) writes no line and clears nothing else.
func TestClosingACaseThatIsNotOpenChangesNothing(t *testing.T) {
	w, buf := writer(t)

	w.Open(SessionListUnreadable, "")
	w.Close(NoLiveSession, "")
	w.Open(SessionListUnreadable, "")

	if got := lines(t, buf); len(got) != 1 {
		t.Fatalf("closing another case let the open one write again: %s", buf.String())
	}
}

// Every case this package names has a word and a sentence, and no two share a word, so none
// reaches the policy unnamed or under another's word. The list is the test's own: a case the
// package adds goes here too.
func TestEveryCaseHasItsOwnWordAndASentence(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []Case{ControllerUnreachable, ControllerRelaunchRefused, NoLiveSession, SessionListUnreadable, NewerProcessRefused} {
		if c.Word == "" || c.Sentence == "" {
			t.Errorf("case %+v has no word or no sentence", c)
		}
		if seen[c.Word] {
			t.Errorf("two cases share the word %q", c.Word)
		}
		seen[c.Word] = true
	}
}

// heldLines is a store of open lines that outlives the writers it is loaded into, as the
// daemon's database outlives a daemon.
type heldLines struct {
	open       map[Line]bool
	refuseNext error
}

func (h *heldLines) OpenLines(context.Context) ([]Line, error) {
	var lines []Line
	for l := range h.open {
		lines = append(lines, l)
	}
	return lines, nil
}

func (h *heldLines) PutLine(_ context.Context, l Line) error {
	h.open[l] = true
	return nil
}

func (h *heldLines) DeleteLine(_ context.Context, l Line) error {
	if err := h.refuseNext; err != nil {
		h.refuseNext = nil
		return err
	}
	delete(h.open, l)
	return nil
}

// boot is a writer of a daemon that has just booted over held.
func boot(t *testing.T, held *heldLines) (*Writer, *testlog.Buffer) {
	t.Helper()
	w, buf := writer(t)
	if err := w.Load(context.Background(), held); err != nil {
		t.Fatalf("load the open lines: %v", err)
	}
	return w, buf
}

// R6's line is written once until its case ends, and a daemon's boot ends no case: a case the
// last daemon opened and nothing ended writes no line from the daemon that boots next, and one
// that ended before the boot writes again when it comes back.
func TestALineOpenBeforeABootIsNotWrittenAgainAfterIt(t *testing.T) {
	held := &heldLines{open: map[Line]bool{}}
	first, firstLog := boot(t, held)
	first.Open(ControllerUnreachable, "claim-1")
	first.Open(NoLiveSession, "")
	if got := len(lines(t, firstLog)); got != 2 {
		t.Fatalf("the first daemon wrote %d lines, want 2", got)
	}
	first.Wait()

	second, secondLog := boot(t, held)
	if wrote := second.Open(ControllerUnreachable, "claim-1"); wrote {
		t.Error("the daemon that booted next wrote the line of a case the last one opened")
	}
	if wrote := second.Open(NoLiveSession, ""); wrote {
		t.Error("the daemon that booted next wrote the line of a case with no claim the last one opened")
	}
	if got := lines(t, secondLog); len(got) != 0 {
		t.Fatalf("the daemon that booted next wrote %d lines of cases still open: %v", len(got), got)
	}

	second.Close(ControllerUnreachable, "claim-1")
	second.Wait()
	third, thirdLog := boot(t, held)
	if wrote := third.Open(ControllerUnreachable, "claim-1"); !wrote {
		t.Error("a case that ended before the boot and came back wrote nothing")
	}
	if wrote := third.Open(NoLiveSession, ""); wrote {
		t.Error("a case open through two boots wrote its line again")
	}
	if got := len(lines(t, thirdLog)); got != 1 {
		t.Fatalf("the third daemon wrote %d lines, want the one of the case that came back", got)
	}
}

// A close the store refused is kept and made again at the next close of the same line, so the
// next daemon does not read a case that ended as open.
func TestAClosedLineTheStoreKeptIsDeletedAtTheNextClose(t *testing.T) {
	held := &heldLines{open: map[Line]bool{}}
	w, _ := boot(t, held)
	w.Open(SessionListUnreadable, "")
	w.Wait()
	held.refuseNext = errors.New("the database is restarting")
	w.Close(SessionListUnreadable, "")
	w.Wait()
	if !held.open[Line{Word: SessionListUnreadable.Word}] {
		t.Fatal("the refused delete took the line out of the store")
	}
	w.Close(SessionListUnreadable, "")
	w.Wait()
	if held.open[Line{Word: SessionListUnreadable.Word}] {
		t.Fatal("the next close of the line did not delete it from the store")
	}
}

// A case whose end only the caller can read — a lock-out the worker stream no longer holds — is
// ended for each claim that reading no longer holds, and kept for each it still holds.
func TestCloseEndedEndsTheLinesWhoseCaseNoLongerHolds(t *testing.T) {
	held := &heldLines{open: map[Line]bool{}}
	w, _ := boot(t, held)
	w.Open(NewerProcessRefused, "claim-1")
	w.Open(NewerProcessRefused, "claim-2")
	w.Open(ControllerUnreachable, "claim-1")

	w.CloseEnded(NewerProcessRefused, func(claim string) bool { return claim == "claim-2" })
	w.Wait()
	want := map[Line]bool{{Word: NewerProcessRefused.Word, Claim: "claim-2"}: true, {Word: ControllerUnreachable.Word, Claim: "claim-1"}: true}
	if !maps.Equal(held.open, want) {
		t.Fatalf("the open lines are %v, want %v", held.open, want)
	}
}

// blockingLines is a store of open lines whose writes wait until release closes, as a store that
// does not answer does; entered receives each line whose write has begun.
type blockingLines struct {
	*heldLines
	entered chan Line
	release chan struct{}
}

func (b *blockingLines) PutLine(ctx context.Context, l Line) error {
	b.entered <- l
	<-b.release
	return b.heldLines.PutLine(ctx, l)
}

// A store that does not answer holds back the record of a line, never another line: the writer
// writes each line, and answers its caller, while the store still waits on an earlier record.
func TestAStoreThatDoesNotAnswerStallsNoOtherLine(t *testing.T) {
	held := &blockingLines{heldLines: &heldLines{open: map[Line]bool{}}, entered: make(chan Line, 4), release: make(chan struct{})}
	w, buf := writer(t)
	if err := w.Load(context.Background(), held); err != nil {
		t.Fatalf("load the open lines: %v", err)
	}
	go w.Open(ControllerUnreachable, "claim-1")
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the store was never asked to keep the first line")
	}

	opened := make(chan bool, 1)
	go func() { opened <- w.Open(NoLiveSession, "") }()
	select {
	case wrote := <-opened:
		if !wrote {
			t.Error("the second case's line was not written")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second case's line waited on the store's write of the first line's record")
	}
	if got := len(lines(t, buf)); got != 2 {
		t.Fatalf("the writer wrote %d lines while the store held the first record back, want 2: %s", got, buf.String())
	}
	close(held.release)
}
