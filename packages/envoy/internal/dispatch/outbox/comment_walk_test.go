package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// walkDeadline is how long each case below waits for the delivery walk to answer. A walk that
// held neither limit would never answer the cycle case, and the case fails on this deadline
// rather than holding the run; every case states it, so a build that lost one limit fails on it
// with the shape it walked printed.
const walkDeadline = 30 * time.Second

// cutWalkLog is what loadCommentRecipients logs, once for each comment whose walk the depth bound
// cut.
const cutWalkLog = "the comment thread walk stopped at its depth bound"

// The depth bound, the delivery walk's limit on a chain of replies that never repeats a row
// (spec R3), proved on its own: no chain below holds a cycle, so the visited guard never acts.
// A chain of maxCommentThreadDepth replies is answered whole from either end - the reply under
// its newest comment reaches the author of every comment up to the root, the reply under its
// root every author down to the newest - and says it cut nothing. A chain one reply longer is cut
// at the bound: the walk up from its newest comment stops one comment short of the root, the walk
// down from its root one short of the newest, each reaches every author inside the bound and
// none past it, says it cut that comment, and loadCommentRecipients logs one line naming the
// comment and the bound.
func TestTheCommentWalkAnswersAChainAtTheBoundAndCutsOneLonger(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	for _, tc := range []struct {
		name    string
		replies int
		cut     bool
	}{
		{name: "a chain at the bound", replies: maxCommentThreadDepth},
		{name: "a chain one reply longer", replies: maxCommentThreadDepth + 1, cut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := seedReplyChain(t, database, tc.replies)
			root, newest := chain[0], chain[len(chain)-1]
			// The walk takes maxCommentThreadDepth steps from the comment it starts at, so a cut
			// walk reaches every comment of the chain but the one at its far end.
			upFrom, downTo := 0, len(chain)
			var wantCut []string
			if tc.cut {
				upFrom, downTo = 1, len(chain)-1
				wantCut = []string{root.id, newest.id}
				if newest.id < root.id {
					wantCut = []string{newest.id, root.id}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), walkDeadline)
			defer cancel()
			recipients, cut, err := walkCommentThreads(ctx, database.Pool, []string{root.id, newest.id})
			if err != nil {
				t.Fatalf("the walk over a chain of %d replies (bound %d) did not answer inside %s: %v",
					tc.replies, maxCommentThreadDepth, walkDeadline, err)
			}
			if got, want := recipients[newest.id], threadMembers(chain[upFrom:]); !reflect.DeepEqual(got, want) {
				t.Errorf("a reply under the newest of %d replies reaches %v, want %v", tc.replies, got, want)
			}
			if got, want := recipients[root.id], threadMembers(chain[:downTo]); !reflect.DeepEqual(got, want) {
				t.Errorf("a reply under the root of %d replies reaches %v, want %v", tc.replies, got, want)
			}
			if !reflect.DeepEqual(cut, wantCut) {
				t.Errorf("the walk over %d replies (bound %d) says it cut %v, want %v", tc.replies, maxCommentThreadDepth, cut, wantCut)
			}

			var logged logtest.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			if _, err := loadCommentRecipients(ctx, database.Pool, []string{root.id, newest.id}); err != nil {
				t.Fatalf("load the recipients of a chain of %d replies: %v", tc.replies, err)
			}
			lines := strings.Split(strings.TrimSpace(logged.Lines(cutWalkLog)), "\n")
			if len(wantCut) == 0 {
				if strings.TrimSpace(logged.Lines(cutWalkLog)) != "" {
					t.Errorf("the walk over a chain at the bound logged a cut:\n%s", logged.String())
				}
				return
			}
			if len(lines) != len(wantCut) {
				t.Fatalf("the cut walks logged %d lines, want one for each of the %d cut comments:\n%s", len(lines), len(wantCut), logged.String())
			}
			for i, comment := range wantCut {
				if !strings.Contains(lines[i], "comment="+comment) || !strings.Contains(lines[i], fmt.Sprintf("depth=%d", maxCommentThreadDepth)) {
					t.Errorf("the cut walk logged %q, want it to name comment %s and depth %d", lines[i], comment, maxCommentThreadDepth)
				}
			}
		})
	}
}

// The visited guard, the delivery walk's limit on a cycle (spec R3), proved on its own:
// comments.reply_to carries no acyclicity constraint, and two comments that reply to each other
// make a cycle far shorter than the depth bound. The walk keyed at one of them, and the walk keyed
// at a reply into the cycle, each answer inside the deadline with every author of the comments
// they visited, and neither says it was cut. That answer alone cannot tell the guard from the
// bound: a walk without the guard goes round the two comments until the bound stops it, the
// union of the members drops the repeats, and the row at the bound replies to a row the walk
// visited, which the cut rule reads as no cut (commentRecipientsQuery). So the same statement runs
// again with the bound lifted to the largest depth it binds: only the guard can end that walk, and
// it answers the same inside the deadline.
func TestTheCommentWalkEndsACycleOnTheRowsItVisited(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	first := model.Actor{Kind: "session", ID: "session-first"}
	second := model.Actor{Kind: "session", ID: "session-second"}
	entering := model.Actor{Kind: "session", ID: "session-entering"}
	a := seedThreadComment(t, database, "T-1", "", first, nil)
	b := seedThreadComment(t, database, "T-1", "", second, &a)
	if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $1 where id = $2`, b, a); err != nil {
		t.Fatalf("close the reply_to cycle: %v", err)
	}
	into := seedThreadComment(t, database, "T-1", "", entering, &a)

	ctx, cancel := context.WithTimeout(context.Background(), walkDeadline)
	defer cancel()
	recipients, cut, err := walkCommentThreads(ctx, database.Pool, []string{a, into})
	if err != nil {
		t.Fatalf("the walk over a two-comment reply_to cycle did not answer inside %s: %v", walkDeadline, err)
	}
	want := map[string][]threadRecipient{
		a: {{sessionID: first.ID, fromThread: true}, {sessionID: second.ID, fromThread: true}},
		into: {
			{sessionID: first.ID, fromThread: true}, {sessionID: second.ID, fromThread: true},
			{sessionID: entering.ID, fromThread: true},
		},
	}
	if !reflect.DeepEqual(recipients, want) {
		t.Errorf("the walk over the cycle reaches %v, want %v", recipients, want)
	}
	if len(cut) != 0 {
		t.Errorf("the walk over a two-comment cycle says it cut %v, want no cut", cut)
	}

	rows, err := database.Pool.Query(ctx, commentRecipientsQuery, []string{a, into}, math.MaxInt32)
	if err != nil {
		t.Fatalf("the unbounded walk over a two-comment reply_to cycle did not start: %v", err)
	}
	unbounded := map[string][]threadRecipient{}
	var key string
	var sessionID *string
	var fromThread, stopped bool
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID, &fromThread, &stopped}, func() error {
		if stopped {
			return fmt.Errorf("the unbounded walk says it cut comment %s", key)
		}
		unbounded[key] = append(unbounded[key], threadRecipient{sessionID: *sessionID, fromThread: fromThread})
		return nil
	}); err != nil {
		t.Fatalf("the walk over a two-comment reply_to cycle with no depth bound did not answer inside %s: %v: only the visited guard ends it", walkDeadline, err)
	}
	if !reflect.DeepEqual(unbounded, want) {
		t.Errorf("the walk over the cycle with no depth bound reaches %v, want %v", unbounded, want)
	}
}

// The two limits meeting: a cycle of maxCommentThreadDepth+1 comments, keyed at either end of the
// chain it closes, has the walk up reach its last new row at exactly the bound's depth, still
// replying to another - the row it set out from, which it has visited. The depth bound stops the
// walk there, and the cut rule reads that row as no cut, since the row it replies to is one the
// walk visited (commentRecipientsQuery's cuts): the walk answers every author of the cycle, in the
// order they wrote, says it cut nothing, and loadCommentRecipients logs no line.
func TestTheCommentWalkEndsACycleAtTheBoundWithoutACut(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	chain := seedReplyChain(t, database, maxCommentThreadDepth)
	root, newest := chain[0], chain[len(chain)-1]
	if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $1 where id = $2`, newest.id, root.id); err != nil {
		t.Fatalf("close the reply_to cycle: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), walkDeadline)
	defer cancel()
	recipients, cut, err := walkCommentThreads(ctx, database.Pool, []string{root.id, newest.id})
	if err != nil {
		t.Fatalf("the walk over a cycle of %d comments did not answer inside %s: %v", len(chain), walkDeadline, err)
	}
	want := map[string][]threadRecipient{root.id: threadMembers(chain), newest.id: threadMembers(chain)}
	if !reflect.DeepEqual(recipients, want) {
		t.Errorf("the walk over a cycle of %d comments reaches %v, want %v", len(chain), recipients, want)
	}
	if len(cut) != 0 {
		t.Errorf("the walk over a cycle of %d comments (bound %d) says it cut %v, want no cut: the row it stopped at replies to one it visited",
			len(chain), maxCommentThreadDepth, cut)
	}
	var logged logtest.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	if _, err := loadCommentRecipients(ctx, database.Pool, []string{root.id, newest.id}); err != nil {
		t.Fatalf("load the recipients of the cycle: %v", err)
	}
	if lines := strings.TrimSpace(logged.Lines(cutWalkLog)); lines != "" {
		t.Errorf("the walk over the cycle logged a cut:\n%s", lines)
	}
}

// chainLink is one comment of a seeded chain of replies, and the session that wrote it.
type chainLink struct {
	id      string
	session string
}

// seedReplyChain seeds a root comment on T-1 and replies comments under it, each replying to the
// one before it and each written by a session of its own, and answers the chain root first. Each
// comment is written after the one before it, so the chain is in the order the walk ranks a
// thread's members (commentRecipientsQuery).
func seedReplyChain(t *testing.T, database *store.Store, replies int) []chainLink {
	t.Helper()
	chain := make([]chainLink, 0, replies+1)
	var parent *string
	for link := range replies + 1 {
		author := model.Actor{Kind: "session", ID: fmt.Sprintf("session-link-%02d", link)}
		id := seedThreadComment(t, database, "T-1", "", author, parent)
		chain = append(chain, chainLink{id: id, session: author.ID})
		parent = &id
	}
	return chain
}

// threadMembers is the recipients the walk answers for links: each one's session, as a member of
// the thread, in the order of links.
func threadMembers(links []chainLink) []threadRecipient {
	members := make([]threadRecipient, 0, len(links))
	for _, link := range links {
		members = append(members, threadRecipient{sessionID: link.session, fromThread: true})
	}
	return members
}
