package store

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// RepoRename is one rename pair: From is the `owner/name` a GitHub repository was renamed away
// from, and To the `owner/name` it has now, both in lower case. `sjawhar/legion` to
// `cybermonklabs/legion` is the one pair production holds.
//
// A pair is a repo_projects row with a renamed_to. No route of this release writes one, so the
// rows are frozen, and the boot reads them once (ReadRepoRenames) and gives them to every reader
// (refs.Renames). One step is the whole rule: the route that wrote the rows refused a pair whose
// target was itself renamed, and refused a rename of a repository another pair already pointed
// at, so no chain and no cycle exists.
type RepoRename struct {
	From, To string
}

// ReadRepoRenames reads the rename pairs. It reads nothing, and answers no pairs and no error,
// when repo_projects does not exist: dispatch://INFRA-4411 drops the table, and a process of this
// build must then boot and serve with no pairs. The table's presence is a statement of its own,
// because Postgres resolves a table name when it parses a statement: a read that named a dropped
// table would fail whatever guard it held.
//
// It lowercases both names of each row, as every comparison of a repository here is without
// case, and drops a row whose two names are equal or empty, which renames nothing. Two rows can
// name one old name in different cases, because the table's key is the repository's own
// spelling. Of those, the pair with the greatest new name is kept, so each old name has one new
// name and every reader takes the same one.
func ReadRepoRenames(ctx context.Context, pool *Pool) ([]RepoRename, error) {
	var present bool
	if err := pool.QueryRow(ctx, `select to_regclass('repo_projects') is not null`).Scan(&present); err != nil {
		return nil, fmt.Errorf("ask whether repo_projects exists: %w", err)
	}
	if !present {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `select repo, renamed_to from repo_projects where renamed_to is not null`)
	if err != nil {
		return nil, fmt.Errorf("read the repository rename pairs: %w", err)
	}
	defer rows.Close()
	var pairs []RepoRename
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, fmt.Errorf("read a repository rename pair: %w", err)
		}
		pair := RepoRename{From: strings.ToLower(from), To: strings.ToLower(to)}
		if pair.From == "" || pair.To == "" || pair.From == pair.To {
			continue
		}
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the repository rename pairs: %w", err)
	}
	slices.SortFunc(pairs, func(a, b RepoRename) int {
		return cmp.Or(strings.Compare(a.From, b.From), strings.Compare(b.To, a.To))
	})
	return slices.CompactFunc(pairs, func(a, b RepoRename) bool { return a.From == b.From }), nil
}

// RepoURLRule rewrites a GitHub URL that names one repository so that it names another. A URL
// names the repository `owner/name` when all of these are true:
//
//   - its scheme is http or https, and its host is github.com or www.github.com, in any case;
//   - its first two path segments are owner and name, in any case, and name can have `.git`
//     after it;
//   - `/`, `?`, `#` or the end of the URL comes after them.
//
// The rewrite puts the other `owner/name` in place of those two segments and keeps every other
// character. So `https://github.com/SJawhar/Legion/pull/7` names sjawhar/legion and becomes
// `https://github.com/cybermonklabs/legion/pull/7`, and
// `https://github.com/sjawhar/legion-smoke/issues/599` does not name sjawhar/legion.
//
// The rule is one regular expression, and Go and Postgres both read it (Rewrite, RewriteSQL), so
// a URL rewritten in Go and a URL rewritten in a statement follow the same rule. The expression
// uses only syntax that both engines read the same way: `(?is)` at its start makes the match
// case-insensitive and lets `.` match a newline in both.
type RepoURLRule struct {
	to      string
	pattern string
	re      *regexp.Regexp
}

// NewRepoURLRule is the rule that rewrites a URL naming from so that it names to. Both are
// `owner/name`.
func NewRepoURLRule(from, to string) RepoURLRule {
	pattern := `(?is)^(https?://(?:www\.)?github\.com/)` + regexp.QuoteMeta(from) + `((?:\.git)?(?:[/?#].*)?)$`
	return RepoURLRule{to: to, pattern: pattern, re: regexp.MustCompile(pattern)}
}

// Rewrite answers url under the rule's new name, and true, when url names the rule's old name.
// Otherwise it answers url unchanged, and false.
func (r RepoURLRule) Rewrite(url string) (string, bool) {
	match := r.re.FindStringSubmatchIndex(url)
	if match == nil {
		return url, false
	}
	return url[match[2]:match[3]] + r.to + url[match[4]:match[5]], true
}

// RewriteSQL is Rewrite as a SQL expression over url, an expression that names a URL. It answers
// url unchanged where url does not name the rule's old name, and null where url is null.
func (r RepoURLRule) RewriteSQL(url string) string {
	return `regexp_replace(` + url + `, ` + QuoteLiteral(r.pattern) + `, ` +
		QuoteLiteral(`\1`+strings.ReplaceAll(r.to, `\`, `\\`)+`\2`) + `)`
}

// QuoteLiteral is s as a SQL string literal: in single quotes, with each single quote in s
// doubled. It is for text a statement is built from once, such as a rename pair, where a bound
// parameter cannot go. A backslash stays as it is, because standard_conforming_strings is on,
// which is Postgres's default.
func QuoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// QuoteLiterals is values as the list of SQL string literals an `in` list takes: each value
// through QuoteLiteral, in the order of values, joined with a comma and a space. Like
// QuoteLiteral, it is for text a statement is built from once, such as a list of kinds or
// statuses, where a bound parameter does not go.
func QuoteLiterals(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = QuoteLiteral(value)
	}
	return strings.Join(quoted, ", ")
}
