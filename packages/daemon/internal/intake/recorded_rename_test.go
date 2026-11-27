package intake

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A review or a checks settlement of a pull request already open when its repository was
// transferred is one the daemon records: the row keeps sjawhar/legion, the name the pull request
// opened under, while the event names cybermonklabs/legion, so the read asks under both names of
// the configured pair (ghrepo.Under), as the workflow reads the row. Without the pair
// the fact names a pull request the daemon does not record, which is acknowledged with no
// transaction and no GitHub read; so does a number neither name records. A fact that is no
// review and no settlement is recorded whatever it names.
func TestAReviewOfAPullRequestRecordedUnderTheOtherNameOfARenameIsRecorded(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	recordPullRequest(t, pool, "LEGION-208", 42)
	renamed := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	for _, tc := range []struct {
		name    string
		renames ghrepo.Renames
		fact    Fact
		want    bool
	}{
		{name: "a review naming the repository it moved to, the pair configured", renames: renamed,
			fact: PullRequestReview{Repo: "cybermonklabs/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}, want: true},
		{name: "a checks settlement naming the repository it moved to, the pair configured", renames: renamed,
			fact: PullRequestChecks{Repo: "cybermonklabs/legion", Number: 42, HeadSHA: "head-captured"}, want: true},
		{name: "a review naming the repository the row keeps, the pair configured", renames: renamed,
			fact: PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}, want: true},
		{name: "a review naming the repository the row keeps, no pair configured",
			fact: PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}, want: true},
		{name: "a review naming the repository it moved to, no pair configured",
			fact: PullRequestReview{Repo: "cybermonklabs/legion", Number: 42, ID: 7, State: "approved", Author: "reviewer"}},
		{name: "a checks settlement naming the repository it moved to, no pair configured",
			fact: PullRequestChecks{Repo: "cybermonklabs/legion", Number: 42, HeadSHA: "head-captured"}},
		{name: "a review of a pull request neither name records", renames: renamed,
			fact: PullRequestReview{Repo: "cybermonklabs/legion", Number: 43, ID: 7, State: "approved", Author: "reviewer"}},
		{name: "a push, which is no pull request read at all", renames: renamed,
			fact: Push{Repo: "cybermonklabs/legion", Branch: "legion/LEGION-208", Before: "head-captured", After: "pushed"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A hang guard, not the subject: AckWait bounds the one store read recordedPullRequest
			// makes, and any fixed window lets the machine decide whether a slow read comes back as
			// the row's answer or as context deadline exceeded: at 5 s a loaded runner's read did,
			// and at one minute a read of 61 s did. The window is what is left of the test
			// binary's deadline less 30 s (testwait.Remaining), so a read however slow is
			// waited out and only one that never answers fails the row, by name, before the
			// binary's own timeout.
			spec := ConsumerSpec{Project: "LEGION", AckWait: testwait.Remaining(t), Renames: tc.renames}
			got, err := recordedPullRequest(context.Background(), spec, pool, tc.fact)
			if err != nil || got != tc.want {
				t.Fatalf("recordedPullRequest(%#v) = %v, %v; want %v and no error", tc.fact, got, err, tc.want)
			}
		})
	}
}
