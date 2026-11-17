package workflowstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	write.Rating = new(rating)
	write.RatingReason = new(reason)
	return write
}

// A retro's record takes its rating and the reason, each once: a later write of another rating
// leaves the first, appends nothing, and the record the read lists carries the first.
func TestARatingIsWrittenOnceOnTheRetrosRecord(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()

	put(t, pool, startOf(retroOf(id), minute(0)), minute(0))
	written := put(t, pool, rated(endOf(retroOf(id), ResultPassed, minute(3)), 4, "The tester found the bug."), minute(3))
	if written.Rating != 4 || written.RatingReason != "The tester found the bug." {
		t.Fatalf("the rating write answers rating %d %q, want 4 and its reason", written.Rating, written.RatingReason)
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
	if again.Rating != 4 || again.RatingReason != "The tester found the bug." {
		t.Fatalf("a second rating answers rating %d %q, want the first, 4", again.Rating, again.RatingReason)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("a second rating appended %d tuples, want none", len(after)-len(before))
	}
	steps := issueSteps(t, pool, "LEG-1")
	if len(steps) != 1 || steps[0].Rating != 4 || steps[0].RatingReason != "The tester found the bug." {
		t.Fatalf("the read lists %+v, want the one retro rated 4 with its reason", steps)
	}
}

// A rating and its reason come together, the rating 1 to 5 and the reason not blank and at most
// 2,000 characters counted as Unicode code points, as the daemon counts them; a rating is a
// retro's alone. Each refusal names the field and writes nothing. A reason of exactly 2,000 code
// points of two bytes each is taken, which is the control of the bound.
func TestARatingWriteRefusesAValueOutsideItsForm(t *testing.T) {
	pool := storetest.Open(t).Pool
	for name, write := range map[string]StepWrite{
		"rating 0":               rated(retroOf(uuid.NewString()), 0, "A reason."),
		"rating 6":               rated(retroOf(uuid.NewString()), 6, "A reason."),
		"rating_reason left out": func() StepWrite { write := retroOf(uuid.NewString()); write.Rating = new(3); return write }(),
		"rating left out":        func() StepWrite { write := retroOf(uuid.NewString()); write.RatingReason = new("Why."); return write }(),
		"rating_reason blank":    rated(retroOf(uuid.NewString()), 3, "  \n\t "),
		"rating_reason of 2001":  rated(retroOf(uuid.NewString()), 3, strings.Repeat("é", 2001)),
		"rating on a step":       rated(implementer(uuid.NewString()), 3, "A reason."),
		"rating on a merge": func() StepWrite {
			write := rated(retroOf(uuid.NewString()), 3, "A reason.")
			write.Kind = new(StepKindMerge)
			return write
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			err := tryWrite(pool, func(tx pgx.Tx) error {
				_, err := PutStep(context.Background(), tx, write, daemon, minute(0))
				return err
			})
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
	if longest.Rating != 5 || len([]rune(longest.RatingReason)) != 2000 {
		t.Fatalf("a reason of 2,000 code points answers rating %d and %d code points, want 5 and 2,000", longest.Rating, len([]rune(longest.RatingReason)))
	}
}

// A rating is judged against the kind the record holds, not the kind the write names: a record
// made as a step refuses a write that calls it a retro and rates it, and keeps no rating.
func TestARatingOnARecordHeldAsAStepIsRefused(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	put(t, pool, startOf(implementer(id), minute(0)), minute(0))
	before := wholeLog(t, pool)

	err := tryWrite(pool, func(tx pgx.Tx) error {
		_, err := PutStep(context.Background(), tx, rated(retroOf(id), 3, "A reason."), daemon, minute(1))
		return err
	})
	if !errors.Is(err, ErrStepInput) || !strings.Contains(err.Error(), "rating") {
		t.Fatalf("a rating of a record held as a step answered %v, want %v naming rating", err, ErrStepInput)
	}
	if after := wholeLog(t, pool); len(after) != len(before) {
		t.Fatalf("the refused rating appended %d tuples, want none", len(after)-len(before))
	}
	if steps := issueSteps(t, pool, "LEG-1"); len(steps) != 1 || steps[0].Rating != 0 || steps[0].Kind != StepKindStep {
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
	if ended.Rating != 2 || ended.RatingReason != "The plan skipped the tests." || ended.Result != ResultPassed || ended.EndedAt == nil {
		t.Fatalf("the end after the rating answers %+v, want the rating beside the end", ended)
	}
	if nodes := countNodes(t, pool, id); nodes != 1 {
		t.Fatalf("the two writes left %d nodes, want one", nodes)
	}
}
