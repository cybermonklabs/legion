package daemon

// A project release's start, which gives up at its own deadline when the store does not answer.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// unansweredStart is a release's store whose start answers only once its context ends, with that
// context's error. The store's own start also runs until its context ends while its write does not
// commit, and then answers the last run's error (store.StartProjectRelease).
type unansweredStart struct{ *releaseRecord }

func (unansweredStart) StartProjectRelease(ctx context.Context, _ string, _ func() time.Time, _ time.Duration) (store.ProjectRelease, []store.TakenOverIdleRelease, []store.ProjectReleaseClaim, error) {
	<-ctx.Done()
	return store.ProjectRelease{}, nil, nil, ctx.Err()
}

// dispatch://INFRA-3583 R7: a start whose record the store does not commit answers the store's
// error at a deadline of its own, a tenth of the bound and at least projectReleaseStartFloor
// (projectReleaseStartBound), and not at the end of the request, which the command holds open long
// past the bound. The mark is clear again, so the daemon starts the project's work as before and
// the operator's next run gets a whole bound. The negative control is the store called on the
// request's context, on which the start answers only once the request ends.
func TestAProjectReleaseStartTheStoreDoesNotAnswerGivesUpAtItsOwnDeadline(t *testing.T) {
	t.Parallel()
	mark := &projectRelease{}
	r := &projectReleaser{ctx: context.Background(), project: "legion", mark: mark, store: unansweredStart{&releaseRecord{}},
		pods: &releasePods{}, defaultBound: time.Minute, now: time.Now, log: quietLogger()}
	request, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	began := time.Now()
	_, started, err := r.Start(request, 10*time.Second, false)
	took := time.Since(began)
	if !errors.Is(err, context.DeadlineExceeded) || started {
		t.Fatalf("Start whose store never answered = started %v, %v; want the deadline's error", started, err)
	}
	if took < projectReleaseStartFloor || took > 30*time.Second || request.Err() != nil {
		t.Fatalf("Start whose store never answered answered after %s, the request %v; want at least %s, the start's deadline for a bound of 10s, and well before the request's minute ended",
			took, request.Err(), projectReleaseStartFloor)
	}
	if mark.held() {
		t.Fatal("the start that answered an error left the mark held, which holds every start of the project until the next boot")
	}
	if round := r.AskRound(); round != "" {
		t.Fatalf("the start that answered an error asks under %q, want no round", round)
	}
}
