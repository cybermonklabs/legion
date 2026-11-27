package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// fill is pattern with each wildcard filled for the caller: the hidden object of each kind hidden
// from it, or one nobody holds of that kind when hidden is false, and each other kind's value beside
// it. It answers too what the second fill wrote in place of the first, so an answer that names what
// the caller sent can be compared.
func (w hiddenWriteSeed) fill(t *testing.T, pattern string, probe writeProbe, sight walkSight, hidden bool) (string, map[string]string) {
	t.Helper()
	written := map[string]string{}
	path := patternWildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe.Probe[patternWildcard.FindStringSubmatch(match)[1]]
		value, ok := w.fills[object]
		if !ok {
			t.Fatalf("%s: the walk holds no value of %q", pattern, object)
		}
		switch {
		case !hiddenFrom(object, sight):
			return value.besideValue()
		case hidden:
			return value.hiddenValue(sight.open)
		}
		written[value.missing] = value.hiddenValue(sight.open)
		return value.missing
	})
	return path, written
}

// maskTimes is body with every RFC 3339 time written as one placeholder.
func maskTimes(body string) string { return timeStamp.ReplaceAllString(body, "<time>") }

// callerAlicePubToken is alice's token that lists PUB: a full member's, so it sees PUB and is open,
// and its list holds its writes from the deployment's rows; a token of carol's is someone else's
// to it.
const callerAlicePubToken privateCaller = "alice's token that lists PUB"

// maskAges is body with every age in seconds written as one placeholder.
func maskAges(body string) string { return ageSeconds.ReplaceAllString(body, `"age_seconds":<age>`) }

// hiddenPerson is a full member no list of PRIV names, so a caller that sees PRIV alone does not
// see them (access.PersonClause), and an open viewer does.
const hiddenPerson = "dana@example.com"

// mintListingToken mints login's personal token named caller, writes projects as its list, and
// keeps it among the seed's tokens, so seed.request presents it.
func mintListingToken(t *testing.T, seed privateSeed, caller privateCaller, login string, projects []string) {
	t.Helper()
	minted := decodeBody[agentTokenResponse](t, dispatchRequest(t, seed.handler, http.MethodPost, "/api/v1/me/agent-tokens",
		map[string]string{"name": string(caller)}, login))
	if minted.Token == "" {
		t.Fatalf("mint %s: no token in the answer", caller)
	}
	if _, err := seed.database.Pool.Exec(context.Background(), `update agent_tokens set projects = $2 where id = $1::uuid`,
		minted.ID, projects); err != nil {
		t.Fatalf("write the list of %s: %v", caller, err)
	}
	seed.tokens[caller] = minted.Token
}

// hiddenFrom reports whether the walk names a hidden object of kind in place of one nobody holds,
// for a caller of sight: an object of a project is hidden from every caller of the walk, a
// personal token is someone else's, and a row of the deployment is hidden from a caller that is not
// open or whose writes a list holds. A session is hidden from every caller: privateSession, which
// has a session_projects pair of PRIV, a private project, from an open caller, which does not see
// PRIV, and s-pub, which has acted in no project, from a caller that is not open
// (access.SessionClause; probeFill.hiddenValue). Every other kind is part of an object the path
// already names - a document's slug, a version, a block, an attempt, a phase, a person or group on a
// list, a repository, a pause point - and is PRIV's own in both calls.
func hiddenFrom(object probeObject, sight walkSight) bool {
	switch object {
	case probeProject, probeIssue, probeDocument, probeAsk, probeComment, probeMessage, probeThread,
		probeResult, probeAgentToken, probeSession:
		return true
	case probePriority, probeSessionGroup, probeCredentialRequest, probeCredentialGrant,
		probeMachineLogin, probeSignin:
		return !sight.open || sight.held
	}
	return false
}

// hiddenWriteSeed is the seed the walk calls, with the value of every probe kind, the trace of its
// store and the record of its listener's calls. digest is the one statement that digests every
// table (tableDigests), built once from the tables the seed lists, which no call changes.
type hiddenWriteSeed struct {
	privateSeed
	fills  map[probeObject]probeFill
	digest string
	trace  *existenceTrace
	calls  *walkListenerCalls
}

// ageSeconds is an answer's `"age_seconds":N` member, a count of whole seconds since a row was made.
var ageSeconds = regexp.MustCompile(`"age_seconds":[0-9]+`)

// walkSight is what one caller of the walk reads and writes. open is a caller that reads the rows of
// no project and the sessions an open viewer reads; held one whose writes a list holds, so a write of
// the deployment is refused it before its handler (writesHeldTo), though it may read the
// deployment's rows; and named a full member's broker token scoped to one project, whose write of
// any other project's object its scope refuses before the handler (namedProjectOf).
type walkSight struct{ open, held, named bool }

// timeStamp matches an RFC 3339 time, which two runs asked a moment apart each stamp their own.
var timeStamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// probeFill is one kind's values: the hidden object, one of that kind nobody holds, the one a call
// names beside another kind's hidden object ("" for the hidden one), the one hidden from an open
// caller where that is another ("" for the hidden one), and PUB's own ("" for a kind with none): a
// session subscribed to PRIV's issue is the one a subscriber row names beside that issue, a session
// of PUB alone is the one hidden from a caller that sees PRIV alone, and a session of PRIV the one
// hidden from an open caller, which does not see PRIV.
type probeFill struct{ hidden, missing, beside, hiddenFromOpen, public string }

// volatileStatement reports whether a statement is the stamp of volatileColumns' column, which a
// token's first use in a minute writes and its next does not, whatever either call names.
func volatileStatement(statement string) bool {
	return strings.HasPrefix(statement, "update agent_tokens set last_used_at = now()")
}

// besideValue is the value a call names for a kind that is not the object it hides.
func (f probeFill) besideValue() string {
	if f.beside != "" {
		return f.beside
	}
	return f.hidden
}

// hiddenValue is the object of the kind hidden from an open caller or one that is not.
func (f probeFill) hiddenValue(open bool) string {
	if open && f.hiddenFromOpen != "" {
		return f.hiddenFromOpen
	}
	return f.hidden
}
