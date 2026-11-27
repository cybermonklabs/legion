package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheIssueOfAConversationItemAnswersAReadThatFails holds conversationItemIssue, which a PATCH's
// strandedWords reads for each conversation word a child hangs from, to its error: a read the store
// refuses - an id that is no uuid, which the statement casts - is that error, and no item of no
// issue, which the PATCH would retract as stranded before the aborted transaction failed it with
// another error. An item no row holds is still no issue, and no error.
func TestTheIssueOfAConversationItemAnswersAReadThatFails(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	for _, kind := range []string{graphstore.KindAsk, graphstore.KindComment, graphstore.KindMessage} {
		if issue, err := conversationItemIssue(ctx, database.Pool, kind, "not-a-uuid"); err == nil {
			t.Errorf("the issue of the %s not-a-uuid read %q and no error, want the store's refusal of the id", kind, issue)
		}
		if issue, err := conversationItemIssue(ctx, database.Pool, kind, "00000000-0000-4000-8000-000000000000"); err != nil || issue != "" {
			t.Errorf("the issue of a %s no row holds read %q, %v, want none and no error", kind, issue, err)
		}
	}
}
