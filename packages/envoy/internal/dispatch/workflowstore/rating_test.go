package workflowstore

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The rating of a retro: the agent that does the retro rates the plan the issue ran, 1 to 5 with
// its reason, and the daemon writes both onto the retro's record.

// retroOf is the identity every write of the retro of LEG-1's first run carries: task default,
// step 0, kind retro, the role that did it and generation 1.
func retroOf(id string) StepWrite {
	return StepWrite{
		ID:         id,
		Issue:      "LEG-1",
		Task:       new("default"),
		Place:      new(0),
		Kind:       new(StepKindRetro),
		Role:       new("implementer"),
		Generation: new(int64(1)),
	}
}

// rated is write with a rating and its reason.
func rated(write StepWrite, rating int, reason string) StepWrite {
	write.Rating = &StepRating{Rating: rating, Reason: reason}
	return write
}

// A retro's record takes its rating and the reason, each once: a later write of another rating
// leaves the first, appends nothing, and the record the read lists carries the first.
func TestARatingIsWrittenOnceOnTheRetrosRecord(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	put(t, pool, startOf(retroOf(id), minute(0)), minute(0))
	written := put(t, pool, rated(endOf(retroOf(id), ResultPassed, minute(3)), 4, "The tester found the bug."), minute(3))
	if written.Rating == nil || *written.Rating != (StepRating{Rating: 4, Reason: "The tester found the bug."}) {
		t.Fatalf("the rating write answers rating %+v, want 4 and its reason", written.Rating)
	}
	ratingTuples := 0
	for _, tuple := range wholeLog(t, pool) {
		if tuple.kind == AttributeRating || tuple.kind == AttributeRatingReason {
			ratingTuples++
		}
	}
	if ratingTuples != 2 {
		t.Fatalf("the log holds %d tuples of the rating, want one for the rating and one for its reason", ratingTuples)
	}

	before := wholeLog(t, pool)
	again := put(t, pool, rated(retroOf(id), 2, "Another reason."), minute(9))
	if again.Rating == nil || *again.Rating != (StepRating{Rating: 4, Reason: "The tester found the bug."}) {
		t.Fatalf("a second rating answers rating %+v, want the first, 4", again.Rating)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("a second rating appended %d tuples, want none", len(after)-len(before))
	}
	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 1 || steps[0].Rating == nil || *steps[0].Rating != (StepRating{Rating: 4, Reason: "The tester found the bug."}) {
		t.Fatalf("the read lists %+v, want the one retro rated 4 with its reason", steps)
	}
}

// A rating carries its reason, the rating 1 to 5 and the reason stated, not blank and at most 2,000
// characters counted as Unicode code points, as the daemon counts them; a rating is a retro's
// alone. Each refusal names the field and writes nothing. A reason of exactly 2,000 code points of
// two bytes each is taken, which is the control of the bound. A reason without its rating is no
// StepWrite at all: the reason is a field of the rating.
func TestARatingWriteRefusesAValueOutsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, change := range map[string]func(*StepWrite){
		"rating 0":               func(write *StepWrite) { write.Rating.Rating = 0 },
		"rating 6":               func(write *StepWrite) { write.Rating.Rating = 6 },
		"rating_reason left out": func(write *StepWrite) { write.Rating.Reason = "" },
		"rating_reason blank":    func(write *StepWrite) { write.Rating.Reason = "  \n\t " },
		"rating_reason of 2001":  func(write *StepWrite) { write.Rating.Reason = strings.Repeat("é", 2001) },
		"rating on a step":       func(write *StepWrite) { *write = rated(implementer(write.ID), 3, "A reason.") },
		"rating on a merge":      func(write *StepWrite) { write.Kind = new(StepKindMerge) },
	} {
		t.Run(name, func(t *testing.T) {
			write := rated(retroOf(uuid.NewString()), 3, "A reason.")
			change(&write)
			_, err := tryPut(pool, write, minute(0))
			if !errors.Is(err, ErrStepInput) {
				t.Fatalf("the write answered %v, want %v", err, ErrStepInput)
			}
			if field := strings.Fields(name)[0]; !strings.Contains(err.Error(), field) {
				t.Errorf("the refusal %q does not name %s", err, field)
			}
		})
	}
	if log := wholeLog(t, pool); len(log) != 0 {
		t.Fatalf("the refused writes left %d tuples, want none", len(log))
	}

	longest := put(t, pool, rated(retroOf(uuid.NewString()), 5, strings.Repeat("é", 2000)), minute(0))
	if longest.Rating == nil || longest.Rating.Rating != 5 || utf8.RuneCountInString(longest.Rating.Reason) != 2000 {
		t.Fatalf("a reason of 2,000 code points answers rating %+v, want 5 and 2,000 code points", longest.Rating)
	}
}

// A rating is judged against the kind the record holds as well as the kind the write names: a
// record made as a step refuses a write that calls it a retro and rates it, and keeps no rating.
func TestARatingOnARecordHeldAsAStepIsRefused(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	before := wholeLog(t, pool)

	_, err := tryPut(pool, rated(retroOf(id), 3, "A reason."), minute(1))
	if !errors.Is(err, ErrStepInput) || !strings.Contains(err.Error(), "rating") {
		t.Fatalf("a rating of a record held as a step answered %v, want %v naming rating", err, ErrStepInput)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("the refused rating appended %d tuples, want none", len(after)-len(before))
	}
	if steps := issueSteps(t, pool, "LEG-1"); len(steps) != 1 || steps[0].Rating != nil || steps[0].Kind != StepKindStep {
		t.Fatalf("the read lists %+v, want the one step, unrated", steps)
	}
}

// A rating that arrives before the retro's end makes the record, and the end that arrives after it
// fills the rest and leaves the rating: the daemon's two rows land in either order.
func TestARatingBeforeItsEndMakesTheRecord(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	put(t, pool, rated(retroOf(id), 2, "The plan skipped the tests."), minute(4))
	ended := put(t, pool, endOf(retroOf(id), ResultPassed, minute(3)), minute(5))
	if ended.Rating == nil || *ended.Rating != (StepRating{Rating: 2, Reason: "The plan skipped the tests."}) || ended.Result != ResultPassed || ended.EndedAt == nil {
		t.Fatalf("the end after the rating answers %+v, want the rating beside the end", ended)
	}
	if nodes := countNodes(t, pool, id); nodes != 1 {
		t.Fatalf("the two writes left %d nodes, want one", nodes)
	}
}
