package api

import (
	"net/http"
	"testing"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// seedSessionWork has the roster's sessions act in the seed's projects, each under a credential
// that proves the session (asSession): what a proven session writes in a project is what makes it
// one of the project's agents (session_projects).
func seedSessionWork(t *testing.T, seed privateSeed) {
	t.Helper()
	act := func(project, route, session string, body map[string]any) {
		t.Helper()
		seed.asSession(t, session, project, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/"+route, body, http.StatusCreated)
	}
	act(seedPublic, "comments", sessionOfPublic, map[string]any{"body": "Done on PUB."})
	act(seedPrivate, "comments", sessionOfPrivate, map[string]any{"body": "Done on PRIV."})
	act(seedPublic, "asks", sessionOfBoth, map[string]any{"question": "Ship PUB?"})
	act(seedPrivate, "asks", sessionOfBoth, map[string]any{"question": "Ship PRIV?"})
}

// privateRosterListed is privateRoster with the listener listing the projects of session-pub's
// and session-priv's credentials, as a listener that knows them does: each is carol's, its broker
// token scoped to one project (asSession). A session's conversations and files are its content
// (access.SessionContentClause), and carol sees PRIV, so with no list a caller that does not see
// PRIV reads no conversation of either; the list bounds what session-pub's credential reads to PUB.
// A read stores the list where the content rule reads it only once Dispatch has read the
// listener's sessions, as a list of the agents does.
const privateRosterListed = `[
	{"session_id":"session-both","title":"Both","capabilities":["btw","steer"],"last_seen":400},
	{"session_id":"session-priv","title":"Private","capabilities":["btw","steer"],"projects":["PRIV"],"last_seen":300},
	{"session_id":"session-pub","title":"Public","capabilities":["btw","steer"],"projects":["PUB"],"last_seen":200},
	{"session_id":"session-idle","title":"Idle","capabilities":["btw","steer"],"projects":[],"last_seen":100}
]`

// The sessions the stand-in listener lists, by what each does in the seed: session-pub comments on
// PUB's issue, session-priv on PRIV's, session-both opens an ask on each, and session-idle does
// nothing, so it has no history in any project.
const (
	sessionOfPublic  = "session-pub"
	sessionOfPrivate = "session-priv"
	sessionOfBoth    = "session-both"
	sessionIdle      = "session-idle"
)
