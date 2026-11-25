package ghbranch

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// Outcome is what Reset did with an issue's branch.
type Outcome string

const (
	// Moved is a branch that stood at the merged head and now stands at the base's head.
	Moved Outcome = "moved"
	// Made is a branch GitHub did not have, now created at the base's head.
	Made Outcome = "made"
	// Stands is a branch that already stood at the base's head, or at a commit that holds it:
	// nothing was written.
	Stands Outcome = "stands"
	// Left is a branch whose tip is neither the merged head nor a commit that holds the base's
	// head: it holds commits after the merge, or someone moved it. Nothing was written, and the
	// caller tells whoever must merge the base forward.
	Left Outcome = "left"
)

// Result is what Reset found and did: its Outcome, the base it read (Base: the base asked for, or
// the fallback where GitHub no longer has that one), the base's head commit (Head), and the
// branch's tip as Reset read it (Tip, empty where GitHub had no such branch).
type Result struct {
	Outcome Outcome
	Base    string
	Head    string
	Tip     string
}

// Reset moves branch on repo, whose REST API github calls, to the head of base, the branch its
// issue's last pull request merged into, for the next change of that issue; mergedHead is the head
// that pull request merged. It reads the branch's ref, and moves it only where its tip is
// mergedHead, with force, since a squash merge leaves the base on a commit that does not descend
// from the merged head. A branch GitHub does not have is created at the base's head (Made). A
// branch that already stands at the base's head, or at a commit that holds it (GitHub's compare of
// the two answers `ahead`), is left as it stands (Stands), so a second call for the same change
// writes nothing. Any other tip is left too (Left): it holds commits after the merge, which are
// someone's work. A base GitHub no longer has is replaced by fallback, the repository's recorded
// default branch.
//
// The move is GitHub's GraphQL mutation updateRefs through graphql, a client of GitHub's GraphQL
// endpoint with the same token, whose beforeOid makes it conditional on the ref still pointing at
// mergedHead (moveFrom): REST's ref update takes no expected old commit, so a push that landed
// between the read and the write would be lost to the force. A move GitHub refuses because the
// branch moved meanwhile reads the branch again and judges its new tip as any other (Left or
// Stands), so a push in that window stays on the branch. A create is no such risk: GitHub refuses
// the create of a ref that exists.
//
// The ref name passes this repository's private-name rules first, as Create's does, and nothing is
// read or written for a name they refuse.
func Reset(ctx context.Context, github, graphql githubrest.Client, repo ghrepo.Repository, branch, base, fallback, mergedHead string) (Result, error) {
	ref := "refs/heads/" + branch
	if err := leakguard.New(leakguard.Options{}).Rules(ctx, leakguard.RefRecord(ref)); err != nil {
		return Result{}, fmt.Errorf("reset a branch of %s: %w", repo, err)
	}
	result := Result{Base: base}
	head, found, err := refCommit(ctx, github, repo, base)
	if err != nil {
		return Result{}, err
	}
	if !found && fallback != "" && fallback != base {
		result.Base = fallback
		if head, found, err = refCommit(ctx, github, repo, fallback); err != nil {
			return Result{}, err
		}
	}
	if !found {
		return Result{}, fmt.Errorf("read refs/heads/%s of %s: GitHub has no such branch", result.Base, repo)
	}
	result.Head = head

	tip, found, err := refCommit(ctx, github, repo, branch)
	if err != nil {
		return Result{}, err
	}
	if !found {
		if err := github.Post(ctx, "/git/refs", map[string]string{"ref": ref, "sha": head}, nil); err != nil {
			return Result{}, fmt.Errorf("create %s on %s at %s: %w", ref, repo, head, err)
		}
		result.Outcome = Made
		return result, nil
	}
	if tip == mergedHead && tip != head {
		moved, now, err := moveFrom(ctx, github, graphql, repo, branch, mergedHead, head)
		if err != nil {
			return Result{}, err
		}
		if moved {
			result.Tip, result.Outcome = tip, Moved
			return result, nil
		}
		// The branch moved after the read: its new tip is judged below, as any other.
		tip = now
	}
	result.Tip = tip
	if tip == head {
		result.Outcome = Stands
		return result, nil
	}
	held, err := holds(ctx, github, repo, tip, head)
	if err != nil {
		return Result{}, err
	}
	result.Outcome = Left
	if held {
		result.Outcome = Stands
	}
	return result, nil
}

// updateRefsMutation is GitHub's GraphQL mutation updateRefs over one ref: its update to afterOid
// is made only while the ref points at beforeOid, and force allows an update that is no
// fast-forward (GitHub's GraphQL reference, the Git category: updateRefs and RefUpdate).
const updateRefsMutation = `mutation($repositoryId: ID!, $name: GitRefname!, $beforeOid: GitObjectID!, $afterOid: GitObjectID!, $force: Boolean!) {
  updateRefs(input: {repositoryId: $repositoryId, refUpdates: [{name: $name, beforeOid: $beforeOid, afterOid: $afterOid, force: $force}]}) { clientMutationId }
}`

// moveFrom moves branch of repo from the commit from to the commit to, with force, only while its
// ref points at from (updateRefs's beforeOid), and answers whether it moved. The mutation names the
// repository by its node id, which the REST API answers beside its name (`node_id`). A refusal
// GitHub answers in the mutation's errors reads the branch again: GitHub answers a stale beforeOid
// with an error of no type and a generic message (a 200 with no data, seen when it was tried on a
// scratch branch), so the refusal is judged by the branch rather than by its words. Where the
// branch no longer points at from, it moved after the caller's read, and moveFrom answers false
// and the branch's tip now. Where it still does, or GitHub no longer has it, the refusal is an
// error naming GitHub's words. A rate limit or any other failure is an error.
func moveFrom(ctx context.Context, github, graphql githubrest.Client, repo ghrepo.Repository, branch, from, to string) (bool, string, error) {
	ref := "refs/heads/" + branch
	var repository struct {
		NodeID string `json:"node_id"`
	}
	if err := github.Get(ctx, "", &repository); err != nil {
		return false, "", fmt.Errorf("read the node id of %s: %w", repo, err)
	}
	if repository.NodeID == "" {
		return false, "", fmt.Errorf("read the node id of %s: GitHub named none", repo)
	}
	var moved struct {
		UpdateRefs *struct{} `json:"updateRefs"`
	}
	err := graphql.GraphQL(ctx, updateRefsMutation, map[string]any{
		"repositoryId": repository.NodeID, "name": ref, "beforeOid": from, "afterOid": to, "force": true,
	}, &moved)
	var refused githubrest.GraphQLErrors
	if !errors.As(err, &refused) {
		if err != nil {
			return false, "", fmt.Errorf("move %s on %s from %s to %s: %w", ref, repo, from, to, err)
		}
		return true, "", nil
	}
	now, found, readErr := refCommit(ctx, github, repo, branch)
	if readErr != nil {
		return false, "", readErr
	}
	if !found || now == from {
		return false, "", fmt.Errorf("move %s on %s from %s to %s: %w", ref, repo, from, to, err)
	}
	return false, now, nil
}

// refCommit is the commit refs/heads/<branch> of repo names, and whether GitHub has that branch:
// GitHub's 404 for the ref is no such branch, and every other answer other than 2xx is an error
// naming the ref and wrapping GitHub's *githubrest.Answer.
func refCommit(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, branch string) (string, bool, error) {
	var read struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := github.Get(ctx, "/git/ref/heads/"+RefPath(branch), &read)
	var answer *githubrest.Answer
	if errors.As(err, &answer) && answer.Status == http.StatusNotFound && !answer.RateLimited {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read refs/heads/%s of %s: %w", branch, repo, err)
	}
	if read.Object.SHA == "" {
		return "", false, fmt.Errorf("read refs/heads/%s of %s: GitHub named no commit", branch, repo)
	}
	return read.Object.SHA, true, nil
}

// holds says whether tip holds head: GitHub's compare of head with tip answers `ahead` (tip
// descends from head) or `identical`. One commit to a page keeps the answer small, and its status
// still reads the whole comparison.
func holds(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, tip, head string) (bool, error) {
	var compared struct {
		Status string `json:"status"`
	}
	if err := github.Get(ctx, "/compare/"+head+"..."+tip+"?per_page=1", &compared); err != nil {
		return false, fmt.Errorf("compare %s with %s on %s: %w", tip, head, repo, err)
	}
	switch compared.Status {
	case "ahead", "identical":
		return true, nil
	case "behind", "diverged":
		return false, nil
	}
	return false, fmt.Errorf("compare %s with %s on %s: GitHub answered the status %q", tip, head, repo, compared.Status)
}
