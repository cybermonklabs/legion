package exchange_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// TestNewSetRefusesAnEntryWhoseProjectIsNoProjectKey: an entry's project is what every token minted
// under it names, and the exchange stores and reads that list back through oauthwire.ParseProjects,
// which takes only contracts.ProjectKeyPattern keys. An entry naming anything else — lowercase, one
// letter, too long, a hyphen — could never mint: every trade of it fails server_error at
// projectAccess's ParseProjects, with no log line naming the entry (seen against the real binary at
// b10331eb0a with BROKER_EXCHANGE_ACCOUNTS=system:serviceaccount:agent-sessions:agent-session=priv=erin@example.com,
// which booted and answered every pod trade 500 server_error, where the merge base minted). So the
// set refuses it at boot, naming the entry, for each of the three settings that name a project.
//
// A job entry's projects never reach the set as text: each of the three job settings reads its
// projects field through ParseJobProjects, and a JobProjects is built by nothing else, so such a
// key is refused there, naming the key, whether it is a named list's one key or one of public's,
// and the setting's own refusal wraps that with the entry.
func TestNewSetRefusesAnEntryWhoseProjectIsNoProjectKey(t *testing.T) {
	for _, bad := range []string{"priv", "Acme", "A", "TOOLONGPROJ", "AC-ME", "ACME PRIV"} {
		t.Run(bad, func(t *testing.T) {
			for _, c := range []struct {
				setting   string
				accounts  []exchange.AccountEntry
				workflows []exchange.WorkflowEntry
				machines  []exchange.GoogleEntry
				names     string
			}{
				{setting: "BROKER_EXCHANGE_ACCOUNTS",
					accounts: []exchange.AccountEntry{{Account: workerAccount, Project: bad, Person: person, Issuers: []string{firstIssuer}}}, names: workerAccount},
				{setting: "BROKER_EXCHANGE_WORKFLOWS",
					workflows: []exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Project: bad, Person: person}}, names: workflow},
				{setting: "BROKER_VM_EXCHANGE_ACCOUNTS",
					machines: []exchange.GoogleEntry{{Account: pinnedAccount, Service: pinnedService, Project: bad, Person: person}}, names: pinnedAccount},
			} {
				set, err := exchange.NewSet(c.accounts, nil, c.workflows, c.machines, signInDomain)
				if err == nil {
					t.Errorf("NewSet with a %s entry naming the project %q = %v, want a refusal: every token it minted would name a project no list holds, and every trade would fail", c.setting, bad, set)
					continue
				}
				if !strings.Contains(err.Error(), c.names) {
					t.Errorf("NewSet refused the %s entry naming %q with %q, which does not name the entry %s", c.setting, bad, err, c.names)
				}
			}
			for _, text := range []string{bad, "public," + bad, "public,ACME," + bad} {
				job, err := exchange.ParseJobProjects(text)
				if err == nil {
					t.Errorf("ParseJobProjects(%q) = %v, want a refusal: every token a job entry of it minted would name a project no list holds", text, job)
					continue
				}
				if !strings.Contains(err.Error(), `"`+bad+`"`) {
					t.Errorf("ParseJobProjects(%q) refused with %q, which does not name the key %q", text, err, bad)
				}
			}
		})
	}
}
