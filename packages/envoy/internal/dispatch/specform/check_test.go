package specform

import (
	"context"
	"errors"
	"testing"
)

// check is Check with a context that never ends, which the tests of the rules call: such a
// context gives Check no error to answer.
func check(c Context, in Input) []Finding {
	findings, err := Check(context.Background(), c, in)
	if err != nil {
		panic(err)
	}
	return findings
}

// Check judges no text once its context has ended, and answers the context's error. Control: the
// same texts under a context that has not ended give their findings.
func TestCheckJudgesNoTextOnceItsContextEnds(t *testing.T) {
	in := Input{Texts: []Text{statement("Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment.")}}
	if found, err := Check(context.Background(), planted(), in); err != nil || len(found) == 0 {
		t.Fatalf("the control: Check = %#v, %v, want a finding and no error", found, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if found, err := Check(ctx, planted(), in); !errors.Is(err, context.Canceled) || found != nil {
		t.Errorf("Check under an ended context = %#v, %v, want no finding and context.Canceled", found, err)
	}
}
