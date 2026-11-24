package githubrest

import (
	"context"
	"net/url"
	"strings"
)

// Comparison is GitHub's compare of two commits (GET /compare/{base}...{head}): its status, one of
// ahead, behind, identical or diverged, and AheadBy, how many commits head holds that base lacks.
// A head that holds such a commit has AheadBy above 0 whatever its status: ahead, or diverged once
// base moved on past where head left it.
type Comparison struct {
	Status  string `json:"status"`
	AheadBy int    `json:"ahead_by"`
}

// Compare is GitHub's compare of base and head of the client's repository, each a branch or a
// commit, a branch's slashes kept as GitHub reads a branch name on its REST routes. It asks for one
// commit of the list, since a reader of the counts reads no commit of it. An answer other than 2xx is
// an *Answer, a 404 for a base or head GitHub lacks among them.
func (c Client) Compare(ctx context.Context, base, head string) (Comparison, error) {
	var comparison Comparison
	err := c.Get(ctx, "/compare/"+comparedName(base)+"..."+comparedName(head)+"?per_page=1", &comparison)
	return comparison, err
}

// comparedName is name, a branch or a commit, as a part of a compare's path: every character a URL
// path would read as its own syntax escaped, and each `/` kept.
func comparedName(name string) string {
	return strings.ReplaceAll(url.PathEscape(name), "%2F", "/")
}
