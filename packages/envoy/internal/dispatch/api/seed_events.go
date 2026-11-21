package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The eval of the command bar's agent seeds a Dispatch (going/eval, WriteSeed) with the rows a
// Dispatch that lived through the seed's history would hold, and with the events each write of
// that history would have appended. It builds each event's payload through the functions of this
// file, which call the ones the routes build theirs with, so this package stays the one owner of
// each payload's shape. The seed holds only each item's end state, so a caller hands each
// function the change that takes the row tx holds back to the moment of the event.

// SeedIssueCreatedPayload is the payload of the issue.created event of the issue key, as the
// create route builds it: the issue as it was created, which is the row tx holds with created
// applied to it, its blocked_by edges counted from an issue with none, and every field it holds
// a value in as changed.
func SeedIssueCreatedPayload(ctx context.Context, tx pgx.Tx, key string, created func(*model.Issue)) (model.IssueEventPayload, error) {
	issue, err := seededIssue(ctx, tx, key)
	if err != nil {
		return model.IssueEventPayload{}, err
	}
	created(&issue)
	changes, err := issueEdgeChanges(ctx, tx, unsetIssue, issue, false)
	if err != nil {
		return model.IssueEventPayload{}, err
	}
	return newIssueEventPayload(ctx, tx, issue, changes, issueChangedFields(unsetIssue, issue))
}

// SeedIssueUpdatedPayload is the payload of the issue.updated or issue.closed event of a write
// that took the issue key from one state to the next, as the patch route builds it: the issue
// after the write, the edges the write moved and the fields it changed. Both states are the row
// tx holds, with before and after applied to it.
func SeedIssueUpdatedPayload(ctx context.Context, tx pgx.Tx, key string, before, after func(*model.Issue)) (model.IssueEventPayload, error) {
	from, err := seededIssue(ctx, tx, key)
	if err != nil {
		return model.IssueEventPayload{}, err
	}
	to := from
	to.Labels, to.BlockedBy = slices.Clone(from.Labels), slices.Clone(from.BlockedBy)
	before(&from)
	after(&to)
	changed := issueChangedFields(from, to)
	changes, err := issueEdgeChanges(ctx, tx, from, to, slices.Contains(changed, "title"))
	if err != nil {
		return model.IssueEventPayload{}, err
	}
	return newIssueEventPayload(ctx, tx, to, changes, changed)
}

// SeedIssueClaimedPayload is the payload of the issue.claimed event of a first claim of the issue
// key, as the claim route builds it (claimEvent): the issue as the claim left it, which is the row
// tx holds with claimed applied to it, holding the claim.
func SeedIssueClaimedPayload(ctx context.Context, tx pgx.Tx, key string, claimed func(*model.Issue)) (any, error) {
	issue, err := seededIssue(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	claimed(&issue)
	if issue.Claim == nil {
		return nil, fmt.Errorf("issue %s holds no claim", key)
	}
	return claimEvent("issue.claimed", key, issue.Claim.Actor, issue, nil, "claimed").Payload, nil
}

// SeedArtifactCreatedPayload is the payload of the artifact.created event of the artifact id, as
// the artifact routes build it (artifactCreatedEventPayload), from the artifact tx holds and the
// reference changes its text moved, which the caller counted.
func SeedArtifactCreatedPayload(ctx context.Context, tx pgx.Tx, id string, changes model.ReferenceChanges) (any, error) {
	artifact, err := loadArtifactRowByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := loadArtifactDetails(ctx, tx, []*model.Artifact{&artifact}); err != nil {
		return nil, err
	}
	return artifactCreatedEventPayload(artifact, changes), nil
}

// SeedAskAnsweredPayload is the payload of the ask.answered event of a first answer of ask, as
// the answer route builds it (answerTransition): the ask as the answer left it, with the reference
// changes its parts moved, and no answer it replaced.
func SeedAskAnsweredPayload(ask model.Ask, changes model.ReferenceChanges) any {
	return answerTransition(model.Actor{}, nil, nil, answerOptions{}).Payload(model.NewAskEventPayload(ask, changes))
}

// seededIssue is the issue key as tx holds it, read as every route reads an issue (loadIssue,
// which reads only through the queryer it is given, so no server is needed for it).
func seededIssue(ctx context.Context, tx pgx.Tx, key string) (model.Issue, error) {
	return (*server)(nil).loadIssue(ctx, tx, key)
}
