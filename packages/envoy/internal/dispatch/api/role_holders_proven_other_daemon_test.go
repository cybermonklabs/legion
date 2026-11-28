package api

import (
	"net/http"
	"testing"
)

// TestAnotherProjectsDaemonPublishesRoleHoldersOfAProvenSessionsProject: with a project's
// proven-sessions setting on, a role-holder publication passes on a credential that proves the
// session legion-daemon:<P> of a Legion daemon, which includes the daemon of another project P whose
// broker token names P (R8, isIssueDaemon), and not only on a credential that proves the session of
// the issue's own project's daemon. So the 403 ROLE_HOLDER_NOT_PROVEN refusal is for a credential
// that proves no daemon's session, and its text may not say that the credential must prove the
// own project's daemon's session alone.
func TestAnotherProjectsDaemonPublishesRoleHoldersOfAProvenSessionsProject(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRigOn(t, testServerOptions{legionStatusScope: true})
	caller := newBrokerCaller(t)
	for _, project := range []string{brokerTestProject, "OTH"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	requireTaken(t, "turn proven sessions on for OTH", rig.asPerson(t, http.MethodPut, "/api/v1/projects/OTH/proven-sessions", map[string]any{}), http.StatusOK)
	issue := createdIssue(t, "file an issue of the proven-sessions project", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OTH", "title": "An issue of a proven-sessions project", "force": true,
	})).Key
	const daemonToken = "a-daemon-token"
	rig.broker.Says(daemonToken, liveDaemonToken(caller.jkt))

	set := []issueRoleHolder{{Role: "tester", Session: "ses-a"}}
	published := rig.serve(caller.presenting(t, http.MethodPut, "/api/v1/issues/"+issue+"/role-holders", daemonToken, map[string]any{"holders": set}))
	requireTaken(t, "the publication of another project's daemon under its own broker token", published, http.StatusOK)
	requireStoredRoleHolders(t, readStoredRoleHolders(t, rig.database, issue), set, true)
}
