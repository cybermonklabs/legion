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

// MergeResult is GitHub's answer to a merge of one branch into another that is no error.
type MergeResult int

const (
	// MergeJoined is a merge commit on the base, whose parents are the base's head and the head's.
	MergeJoined MergeResult = iota + 1
	// MergeNothing is a head the base holds already: no commit, and no ref moved.
	MergeNothing
	// MergeConflict is a head whose changes conflict with the base's: the base is left where it was.
	MergeConflict
)

// String is the result's name, as a test or a log line reads it.
func (m MergeResult) String() string {
	switch m {
	case MergeJoined:
		return "joined"
	case MergeNothing:
		return "nothing to merge"
	case MergeConflict:
		return "conflict"
	default:
		return fmt.Sprintf("MergeResult(%d)", int(m))
	}
}

// Merge merges head into base on repo, whose REST API github calls, with message as the merge
// commit's message (POST /repos/{owner}/{repo}/merges). GitHub's 201 is MergeJoined, its 204
// MergeNothing and its 409 MergeConflict, none of them an error. Every other answer is an error
// naming the repository and both branches and wrapping GitHub's *githubrest.Answer, a 404 for a base
// or head GitHub does not have among them.
//
// The message is published on GitHub as a created ref's name is, so it passes the deployment's
// private-name rules before anything is sent, as Create's ref name does: rules, the rule set the
// daemon compiled from the deployment's rule file at boot, run alone (leakguard.RuleSet.Check). A
// message they match, and a rule pass that could not be made to run, are each an error, and
// nothing is sent. Nil rules are a deployment that names no rule file, and no private-name rule
// runs. Either way the merge reads no environment: the rules are what its caller hands it, never a
// rule file its process's environment may point at.
func Merge(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, base, head, message string, rules *leakguard.RuleSet) (MergeResult, error) {
	failed := func(err error) error {
		return fmt.Errorf("merge %s into %s on %s: %w", head, base, repo, err)
	}
	if rules != nil {
		if err := rules.Check(ctx, "the write", leakguard.Record{Label: "the merge's commit message", Text: message}); err != nil {
			return 0, failed(err)
		}
	}
	status, err := github.PostStatus(ctx, "/merges", map[string]string{"base": base, "head": head, "commit_message": message}, nil)
	var answer *githubrest.Answer
	switch {
	case err == nil && status == http.StatusCreated:
		return MergeJoined, nil
	case err == nil && status == http.StatusNoContent:
		return MergeNothing, nil
	case err == nil:
		return 0, failed(fmt.Errorf("GitHub answered %d, which is neither a merge commit (201) nor nothing to merge (204)", status))
	case errors.As(err, &answer) && answer.Status == http.StatusConflict:
		return MergeConflict, nil
	default:
		return 0, failed(err)
	}
}
