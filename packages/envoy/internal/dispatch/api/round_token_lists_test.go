// round_token_lists_test.go holds the rows of what a credential's list decides
// (access.CredentialCovers, R3): a full member's list decides
// only which private projects its credential reads and writes, a restricted member's list narrows
// every project, and with no private project a full member's list decides nothing (R9).
package api

import "testing"

// hideFromBrokerPerson makes project key, which the broker rig's person made and so owns, private
// and owned by carol instead, with no access list: a project the rig's tokens' person, alice, does
// not see.
func hideFromBrokerPerson(t *testing.T, rig *brokerTokenRig, key string) {
	t.Helper()
	for _, statement := range []string{
		`update projects set owner = 'carol@example.com' where key = $1`,
		`delete from project_access where project_key = $1`,
	} {
		if _, err := rig.database.Pool.Exec(t.Context(), statement, key); err != nil {
			t.Fatalf("give %s to carol: %v", key, err)
		}
	}
	makePrivate(t, rig.database, key)
}
