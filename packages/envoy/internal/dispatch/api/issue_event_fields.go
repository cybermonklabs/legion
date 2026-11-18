package api

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// unsetIssue is the issue an issue.created's changed list is measured from: no field holds a
// value, and its components inherit from nobody (componentsScan.resolve's unassigned attachment).
var unsetIssue = model.Issue{Components: model.IssueComponents{Mode: "inherit"}}

// issueChangedFields is an issue event's changed list: the name of every field whose value differs
// between before and after, in one fixed order. repo moves with the issue's own repository or the
// one it resolves to (a reparent can move the second alone), and components with the effective
// attachment, so an inherited change counts too. closed_at moves only with a close or a reopen
// (whether the issue is closed, not the time), which a status alone cannot say: a reader learns
// from it that the issue's open asks entered or left the Inbox and the projects' open-ask counts.
func issueChangedFields(before, after model.Issue) []string {
	changed := []string{}
	add := func(field string, moved bool) {
		if moved {
			changed = append(changed, field)
		}
	}
	add("title", before.Title != after.Title)
	add("status", before.Status != after.Status)
	add("closed_at", (before.ClosedAt == nil) != (after.ClosedAt == nil))
	add("rank", before.Rank != after.Rank)
	add("priority", !intPointersEqual(before.Priority, after.Priority))
	add("assignee", !stringPointersEqual(before.Assignee, after.Assignee))
	add("parent", !stringPointersEqual(before.Parent, after.Parent))
	add("labels", !slices.Equal(before.Labels, after.Labels))
	add("route", !stringPointersEqual(before.Route, after.Route))
	add("repo", !stringPointersEqual(before.Repo, after.Repo) || !stringPointersEqual(before.ResolvedRepo, after.ResolvedRepo))
	add("components", !issueComponentsEqual(before.Components, after.Components))
	add("advances", !slices.Equal(before.Advances, after.Advances))
	// The issue's waits under both spellings, one relation answered by both (model.Issue.Needs).
	add("blocked_by", !slices.Equal(before.BlockedBy, after.BlockedBy))
	add("needs", !slices.Equal(before.Needs, after.Needs))
	add("duplicate_of", !stringPointersEqual(before.DuplicateOf, after.DuplicateOf))
	add("filed_from", !stringPointersEqual(before.FiledFrom, after.FiledFrom))
	add("external_links", !slices.Equal(before.ExternalLinks, after.ExternalLinks))
	return changed
}

func intPointersEqual(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func issueComponentsEqual(left, right model.IssueComponents) bool {
	return left.Mode == right.Mode &&
		slices.Equal(left.IDs, right.IDs) &&
		slices.Equal(left.Unknown, right.Unknown) &&
		stringPointersEqual(left.Reason, right.Reason) &&
		stringPointersEqual(left.InheritedFrom, right.InheritedFrom)
}

// issueAncestors is an issue event's ancestors: issue's parent chain, parent first up to the
// root, [] for a root, which it answers without a query. The walk is refs.IssueAncestors, the
// server's one recursive ancestor query, bounded by parentDepthCap as every recursive issue walk
// is. issue is as loadIssue read it in q, so a nil Parent is a null parent_key.
func issueAncestors(ctx context.Context, q queryer, issue model.Issue) ([]string, error) {
	if issue.Parent == nil {
		return []string{}, nil
	}
	return refs.IssueAncestors(ctx, q, issue.Key)
}

// newIssueEventPayload is the payload of an issue.created, issue.updated or issue.closed: the
// issue after the write, what it moved in the reference graph, the fields it changed, and the
// issue's ancestors read in tx. The caller adds thread_logins when the status or the assignee
// moved (issueWriteThreadLogins).
func newIssueEventPayload(ctx context.Context, tx pgx.Tx, issue model.Issue, changes model.ReferenceChanges, changed []string) (model.IssueEventPayload, error) {
	ancestors, err := issueAncestors(ctx, tx, issue)
	if err != nil {
		return model.IssueEventPayload{}, err
	}
	payload := model.NewIssueEventPayload(issue, changes)
	payload.Changed = changed
	payload.Ancestors = ancestors
	return payload, nil
}

// issueEdgeChanges names the nodes whose reads an issue write moves through the issue's own
// child_of, needs, duplicate_of and filed_from edges. A retitle (retitled) names its parent and the
// issues it waits on: the graph keeps those edges, but the excerpt they show (refs.Edges cuts it
// from an issue's title) moves. Every issue the write added to or removed from its waits is named
// too, since its referenced_by_count counts the edge, and so are the issue a duplicate_of moved
// off or onto and the item a filed_from moved off or onto. A create passes unsetIssue as before,
// which names every target the new issue holds. CountedChanges names each target once, so a
// target several reasons name appears once.
func issueEdgeChanges(ctx context.Context, tx pgx.Tx, before, after model.Issue, retitled bool) (model.ReferenceChanges, error) {
	targets := make([]refs.Target, 0, len(before.Needs)+2*len(after.Needs)+5)
	name := func(key string) {
		targets = append(targets, refs.Target{Kind: "issue", ID: key})
	}
	if retitled {
		if after.Parent != nil {
			name(*after.Parent)
		}
		for _, target := range after.Needs {
			name(target)
		}
	}
	for _, target := range before.Needs {
		if !slices.Contains(after.Needs, target) {
			name(target)
		}
	}
	for _, target := range after.Needs {
		if !slices.Contains(before.Needs, target) {
			name(target)
		}
	}
	if !stringPointersEqual(before.DuplicateOf, after.DuplicateOf) {
		for _, key := range []*string{before.DuplicateOf, after.DuplicateOf} {
			if key != nil {
				name(*key)
			}
		}
	}
	if !stringPointersEqual(before.FiledFrom, after.FiledFrom) {
		for _, reference := range []*string{before.FiledFrom, after.FiledFrom} {
			if reference == nil {
				continue
			}
			if item, ok := text.ParseRef(*reference, ""); ok {
				targets = append(targets, refs.Target{Kind: item.Kind, ID: item.ID})
			}
		}
	}
	return refs.CountedChanges(ctx, tx, targets)
}
