// person_pod_machine_logins_test.go drives GET /v1/launcher-credentials, the list that Dispatch's
// page of machine logins relays, and GET /v1/operator/machines, for a person's pod login on the
// real broker (ciphertest.NewRig with ciphertest.WithPersonPod). The row of the pod's login names
// its person in the person field. Every other row writes the field as null. So Dispatch shows the
// pod's login as the person's own and not as a service's login.
package api_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
)

// daemonTokenLogin logs the Legion daemon in with its own pod-bound token over
// POST /v1/launcher-credentials/token: a service's token login, which no person approves and which
// every signed-in person's list holds. It answers the credential's id.
func daemonTokenLogin(t *testing.T, rig *ciphertest.Rig) string {
	t.Helper()
	key := newSigningKey(t)
	status, body := rig.Req(t, http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{
		"token":   rig.MintAccountPodToken(t, ciphertest.DaemonAccount, "pod-uid-daemon", 20*time.Minute),
		"request": signMachineLoginRequest(t, key, rig.URL, "", "legion-daemon-rig", "legion-daemon"),
	})
	if status != http.StatusOK {
		t.Fatalf("the daemon's token login = %d %s; want 200", status, body)
	}
	return decode[wireTokenLogin](t, body).CredentialID
}

// machineLoginPersons reads body, a list of machine logins, and answers the person field of each
// row, keyed by the row's credential id. Each value is the field's JSON as the broker wrote it:
// null, or the person as a JSON string. what names the call that answered body, for the failure
// messages. It fails t when a row has no person field.
func machineLoginPersons(t *testing.T, what string, body []byte) map[string]string {
	t.Helper()
	rows := decode[struct {
		Credentials []map[string]json.RawMessage `json:"credentials"`
	}](t, body).Credentials
	persons := make(map[string]string, len(rows))
	for _, row := range rows {
		var id string
		if err := json.Unmarshal(row["credential_id"], &id); err != nil {
			t.Fatalf("%s: a row with no credential id: %s", what, body)
		}
		person, ok := row["person"]
		if !ok {
			t.Fatalf("%s lists login %s with no person field: %s", what, id, body)
		}
		persons[id] = string(person)
	}
	return persons
}

// pagePersons reads approver's list of machine logins as Dispatch's page reads it
// (GET /v1/launcher-credentials?approver=), through machineLoginPersons.
func pagePersons(t *testing.T, rig *ciphertest.Rig, approver string) map[string]string {
	t.Helper()
	what := "GET /v1/launcher-credentials?approver=" + approver
	status, body := rig.UI(t, http.MethodGet, "/v1/launcher-credentials?approver="+url.QueryEscape(approver), nil)
	if status != http.StatusOK {
		t.Fatalf("%s = %d %s; want 200", what, status, body)
	}
	return machineLoginPersons(t, what, body)
}

// TestAPersonPodLoginsRowNamesItsPersonAndEveryOtherRowNamesNone: ada's list holds her box's
// login, which she approved, the daemon's token login and her pod's login. Only the pod's row
// names a person, ada. Grace's list holds the daemon's login alone, with no person. The box's
// own list (GET /v1/operator/machines) marks the pod's login in the same way. Then the setting
// moves the account to grace while the login's row still names ada as its operator. The person
// field follows the setting: grace's list holds the pod's login and names grace, and ada's list
// no longer holds it.
func TestAPersonPodLoginsRowNamesItsPersonAndEveryOtherRowNamesNone(t *testing.T) {
	rig := newPersonPodRig(t)
	boxMachine, _, box := approvedLogin(t, rig)
	daemon := daemonTokenLogin(t, rig)
	pod, _ := personPodTokenLogin(t, rig, personPodUID)
	ada, grace := strconv.Quote(personPodPerson), strconv.Quote(personPodOther)

	if got, want := pagePersons(t, rig, personPodPerson), map[string]string{box: "null", daemon: "null", pod: ada}; !maps.Equal(got, want) {
		t.Fatalf("%s's machine logins name the persons %v; want %v: the pod's login %s names %s, and her box's login %s and the daemon's login %s name no one",
			personPodPerson, got, want, pod, personPodPerson, box, daemon)
	}
	if got, want := pagePersons(t, rig, personPodOther), map[string]string{daemon: "null"}; !maps.Equal(got, want) {
		t.Fatalf("%s's machine logins name the persons %v; want %v: the daemon's login %s alone, naming no one, and not %s's pod's login %s",
			personPodOther, got, want, daemon, personPodPerson, pod)
	}

	what := "GET /v1/operator/machines from " + personPodPerson + "'s box"
	status, body := launcherCall(t, rig, boxMachine.key, box, http.MethodGet, "/v1/operator/machines", nil)
	if status != http.StatusOK {
		t.Fatalf("%s = %d %s; want 200", what, status, body)
	}
	if got, want := machineLoginPersons(t, what, body), map[string]string{box: "null", pod: ada}; !maps.Equal(got, want) {
		t.Fatalf("%s names the persons %v; want %v: the pod's login %s names %s, and the box's login %s names no one",
			what, got, want, pod, personPodPerson, box)
	}

	rig.SetPodOperator(personPodAccount, personPodOther)
	if got := rigColumn(t, rig, `select coalesce(operator, '<none>') from launcher_credentials where id=$1 and revoked_at is null`, pod); !slices.Equal(got, []string{personPodPerson}) {
		t.Fatalf("after the move, the pod's live login %s holds the operator %v; want %s until the pod logs in again", pod, got, personPodPerson)
	}
	if got, want := pagePersons(t, rig, personPodOther), map[string]string{daemon: "null", pod: grace}; !maps.Equal(got, want) {
		t.Fatalf("after the move, %s's machine logins name the persons %v; want %v: the pod's login %s names %s, as the setting maps its account now, and not %s, whom its row names",
			personPodOther, got, want, pod, personPodOther, personPodPerson)
	}
	if got, want := pagePersons(t, rig, personPodPerson), map[string]string{box: "null", daemon: "null"}; !maps.Equal(got, want) {
		t.Fatalf("after the move, %s's machine logins name the persons %v; want %v, without the pod's login %s",
			personPodPerson, got, want, pod)
	}
}
