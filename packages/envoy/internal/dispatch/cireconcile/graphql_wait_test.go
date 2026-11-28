package cireconcile

import (
	"testing"
	"time"
)

func TestGraphQLWaitDoesNotOverflow(t *testing.T) {
	t.Parallel()
	const processing = int64(GraphQLTimePerMinute / time.Second)
	const waiting = int64((time.Minute - GraphQLTimePerMinute) / time.Second)
	if processing%waiting != 0 {
		t.Fatalf("the processing-to-wait ratio %d:%d must be whole before graphQLWait can divide first", processing, waiting)
	}
	const maxDuration = time.Duration(1<<63 - 1)
	const firstOverflow = time.Duration(int64(maxDuration)/int64(time.Minute-GraphQLTimePerMinute) + 1)
	for _, took := range []time.Duration{150 * time.Millisecond, firstOverflow - 1, firstOverflow, time.Second, maxDuration} {
		t.Run(took.String(), func(t *testing.T) {
			if got, want := graphQLWait(took), took/3; got != want {
				t.Errorf("graphQLWait(%s) = %s, want %s under the 45s-per-minute budget", took, got, want)
			}
		})
	}
}

func TestGraphQLWaitMatchesItsBudgetAcrossAMinute(t *testing.T) {
	t.Parallel()
	const processing = int64(GraphQLTimePerMinute / time.Second)
	const waiting = int64((time.Minute - GraphQLTimePerMinute) / time.Second)
	mismatches := 0
	var firstTook, firstGot, firstWant time.Duration
	for took := time.Millisecond; took <= time.Minute; took += time.Millisecond {
		// Integer seconds make the independent multiplication safe throughout this sweep.
		want := time.Duration(int64(took) * waiting / processing)
		if got := graphQLWait(took); got != want {
			if mismatches == 0 {
				firstTook, firstGot, firstWant = took, got, want
			}
			mismatches++
		}
	}
	if mismatches != 0 {
		t.Fatalf("%d of 60000 waits differ from the budget; first graphQLWait(%s) = %s, want %s", mismatches, firstTook, firstGot, firstWant)
	}
}
