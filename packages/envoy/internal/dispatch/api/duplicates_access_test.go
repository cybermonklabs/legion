package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestTheNearCopyStatementsNameNoIssueOfAProjectTheirViewerDoesNotSee runs each of the gate's two
// statements over the private project of verification v16's seed for alice, who does not see it,
// and for carol, who does, with the same arguments: carol is answered the project's issue, which
// is what makes the row able to fail, and alice is answered nothing, no score and no name.
//
// The create route reaches the gate before it reads whether its creator sees the project, so
// alice's create on PRIV runs the gate as alice (TestAnIssueOfAPrivateProjectAnswersAsMissing
// holds what that create answers). Her term pick reads no statistics of a project she does not
// see, so that create only ever runs the equal-lexemes statement; this row gives the scoring
// statement the terms carol's pick finds, so its own project clause is the only thing between
// alice and the issue.
func TestTheNearCopyStatementsNameNoIssueOfAProjectTheirViewerDoesNotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	ctx := context.Background()
	pool := seed.database.Pool
	const title = "Astrolabe calibration drifts after a reseat"
	issue := decodeBody[model.Issue](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": seedPrivate, "title": title, "spec": duplicateSpecAstrolabe, "force": true,
	}, http.StatusCreated)).Key
	if _, err := refreshSearchTermStats(ctx, pool, seedPrivate); err != nil {
		t.Fatalf("refresh %s's term statistics: %v", seedPrivate, err)
	}
	viewerOf := func(login string) model.Actor {
		t.Helper()
		viewer, err := access.Resolve(ctx, pool, model.BrowserActor(login))
		if err != nil {
			t.Fatalf("resolve what %s sees: %v", login, err)
		}
		return model.Actor{Viewer: &viewer}
	}
	alice, carol := viewerOf(seedAlice), viewerOf(seedCarol)

	picked, err := seed.server.pickRelatedTerms(ctx, pool, carol, seedPrivate, duplicateGateText(title, duplicateSpecAstrolabe), title, relatedExclusion{}, priceUnknownTerms)
	if err != nil {
		t.Fatalf("pick the terms as carol: %v", err)
	}
	keyLexemes, err := duplicateKeyLexemes(ctx, pool, seedPrivate)
	if err != nil {
		t.Fatalf("read %s's key lexemes: %v", seedPrivate, err)
	}
	lexemes, weights := nearCopyScoreTerms(picked, keyLexemes)
	if len(lexemes) < duplicateMinTerms {
		t.Fatalf("carol's pick found %d terms, fewer than the %d the scoring statement is run for", len(lexemes), duplicateMinTerms)
	}

	for _, read := range []struct {
		name      string
		statement string
		arguments func(who model.Actor) []any
		names     string
	}{{
		name: "the scoring statement", statement: duplicateScoreQuery, names: issue,
		arguments: func(who model.Actor) []any {
			return duplicateScoreArguments(seedPrivate, lexemes, weights, []string{}, 0, title, who)
		},
	}, {
		name: "the equal-lexemes statement", statement: duplicateEqualLexemesQuery, names: seed.issue[seedPrivate],
		arguments: func(who model.Actor) []any {
			return []any{seedPrivate, "The issue of " + seedPrivate, "", []string{}, duplicateHeadlineOptions, refs.VisibilityArg(who)}
		},
	}} {
		t.Run(read.name, func(t *testing.T) {
			answer := func(who model.Actor) []scoredDuplicate {
				t.Helper()
				rows, err := pool.Query(ctx, read.statement, read.arguments(who)...)
				if err != nil {
					t.Fatalf("run %s: %v", read.name, err)
				}
				candidates, err := scanDuplicateCandidates(rows)
				if err != nil {
					t.Fatalf("read %s's answer: %v", read.name, err)
				}
				return candidates
			}
			if seen := answer(carol); len(seen) != 1 || seen[0].Key != read.names {
				t.Fatalf("%s answers carol, who sees %s, %+v; want %s alone", read.name, seedPrivate, seen, read.names)
			}
			if unseen := answer(alice); len(unseen) != 0 {
				t.Errorf("%s answers alice, who does not see %s, %+v; want nothing", read.name, seedPrivate, unseen)
			}
		})
	}
}
