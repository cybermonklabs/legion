// packages/envoy/internal/cipher/ciphertest/exchange_entries.go

package ciphertest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// The one account every Rig's BROKER_EXCHANGE_ACCOUNTS lists and what a pod running as it may
// write: ExchangeSession is the session name the entry fixes, so a token minted for that pod
// carries it and a request naming any other is refused.
const (
	ExchangeAccount = "system:serviceaccount:example-ns:example-worker"
	ExchangeProject = "EXAMPLE"
	ExchangePerson  = "ada@example.com"
	ExchangeSession = "example-worker"
)

// The second account every Rig's BROKER_EXCHANGE_ACCOUNTS lists: its entry fixes no session, as
// a Legion issue pod's does, so each pod of it trades only for a session a registrar registered for
// that pod (POST /v1/oauth/exchange/pod-sessions), with the key the registration names. Every
// Rig's BROKER_POD_REGISTRARS lists DaemonAccount as a registrar of its pods.
const ExchangeRolesAccount = "system:serviceaccount:example-ns:example-roles"

// The third account every Rig's BROKER_EXCHANGE_ACCOUNTS lists: a Legion daemon's own pod, whose
// entry fixes DaemonExchangeSession, the session a daemon of ExchangeProject writes Dispatch
// under, and names the Rig's approver (Operator) as its person, since Dispatch refuses every
// write of a token that names none (SIGN_IN_REQUIRED). It is an account of its own rather than
// DaemonAccount, which a Rig lists for no exchange: the registration route refuses a pod token of
// an account the exchange does not list (POD_ACCOUNT_NOT_LISTED), and a test of that refusal
// presents the daemon's own login token.
const (
	DaemonExchangeAccount = "system:serviceaccount:legion-system:legion-daemon-exchange"
	DaemonExchangeSession = "legion-daemon:" + ExchangeProject
)

// The fourth account every Rig's BROKER_EXCHANGE_ACCOUNTS lists: a pod of a second project,
// OtherProject, whose entry fixes OtherProjectSession and names the Rig's approver as its person,
// so a test holds a token that Dispatch serves for its person and that names a project other
// than ExchangeProject.
const (
	OtherProjectAccount = "system:serviceaccount:other-ns:other-project-worker"
	OtherProject        = "OTHER"
	OtherProjectSession = "other-project-worker"
)

// The one workflow every Rig's BROKER_EXCHANGE_WORKFLOWS lists and what a run of it may write. It
// fixes no session, so a run names its own.
const (
	ExchangeRepository = "example-org/example-repo"
	ExchangeWorkflow   = ".github/workflows/release.yaml"
	ExchangeRef        = "refs/heads/main"
)

// newExchangeSet is the token exchange's entries a rig's broker trades on, the one set cmd/cipherd
// builds from its exchange settings: the four accounts and the one workflow the Rig's constants
// name.
func newExchangeSet(t *testing.T) *exchange.Set {
	t.Helper()
	set, err := exchange.NewSet(
		// Each entry names the rig's one cluster issuer, which, written as a bare URL, has the
		// empty name.
		[]exchange.AccountEntry{
			{Account: ExchangeAccount, Project: ExchangeProject, Person: ExchangePerson, Session: ExchangeSession, Issuers: []string{""}},
			{Account: ExchangeRolesAccount, Project: ExchangeProject, Person: ExchangePerson, Issuers: []string{""}},
			{Account: DaemonExchangeAccount, Project: ExchangeProject, Person: ExchangePerson, Session: DaemonExchangeSession, Issuers: []string{""}},
			{Account: OtherProjectAccount, Project: OtherProject, Person: ExchangePerson, Session: OtherProjectSession, Issuers: []string{""}},
		},
		nil,
		[]exchange.WorkflowEntry{{Repository: ExchangeRepository, Workflow: ExchangeWorkflow, Ref: ExchangeRef, Project: ExchangeProject, Person: ExchangePerson}},
		nil,
		nil,
		// The rig lists no machine, so there is no box a person could have signed in on and no
		// domain to hold one to.
		"",
	)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return set
}
