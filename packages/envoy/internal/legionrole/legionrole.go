// Package legionrole is the one statement, on Envoy's side, of what a role of a Legion issue is and
// of the token of its claim: the grammar the daemon's claim.IsRole and isStepRole in
// packages/contracts take, which Dispatch's step write takes too (internal/dispatch/workflowstore).
// Every reader of an issue's role asks it here: the Legion agent registry (legionagents), Dispatch's
// holders of an issue's roles and its reading of a session's title, and the secrets broker's
// reading of a pod's claim. It imports nothing of theirs, so a reader of a role gains no other
// package by asking it.
package legionrole

import (
	"regexp"
	"strings"
)

// Grammar is the grammar of an issue's role, one of Legion's six or a role the deployment defines,
// as a part of a pattern. The controller's name is of the grammar and is no role of an issue
// (IsRole).
const Grammar = `[a-z][a-z0-9-]{0,31}`

// Pattern is the grammar as a pattern of a whole name, `^[a-z][a-z0-9-]{0,31}$`: the spelling a
// JSON schema's `pattern` and a check of the database give it.
const Pattern = `^` + Grammar + `$`

var (
	pattern = regexp.MustCompile(Pattern)
	// tokenPattern is an issue role's token: legion-<project>-<issue project>-<number>-<role>, the
	// issue's project lowercased. Neither a project token nor an issue key's parts hold a "-", so
	// the issue's project, its number and the role are the three groups.
	tokenPattern = regexp.MustCompile(`^legion-[a-z0-9]+-([a-z][a-z0-9]*)-([0-9]+)-(` + Grammar + `)$`)
)

// IsRole reports whether role is a role of an issue: one of Legion's six, or a role the deployment
// defines in its settings, a name of Pattern other than the controller's, which is the project's
// and no issue's.
func IsRole(role string) bool {
	return role != "controller" && pattern.MatchString(role)
}

// ParseToken is the Dispatch issue key and the role an issue role's token names: ACME-7 and planner
// for legion-acme-acme-7-planner, and a role the deployment defines as one of Legion's six
// (IsRole). It is false for a token that is no issue role's: the controller's, which names a
// project and no issue, a claim on the controller's name, and a role outside the grammar.
func ParseToken(token string) (issue, role string, ok bool) {
	match := tokenPattern.FindStringSubmatch(token)
	if match == nil || !IsRole(match[3]) {
		return "", "", false
	}
	return strings.ToUpper(match[1]) + "-" + match[2], match[3], true
}
