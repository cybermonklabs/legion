// packages/envoy/internal/cipher/requests/narrowing_project_test.go
// Create's narrowing check before approval (narrowing_check.go) reads the same project facts
// Create decides with (project_access.go): a narrowing of a private project's GitHub App secret
// is checked like any other, and a request the project rule denies is denied, not checked.
package requests_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// privateAppProject is the private project PRIV_APP_KEY belongs to; its access list names
// checkOperator, the fixture's person.
const privateAppProject = "PRIV"

// newPrivateNarrowingFixture is newNarrowingFixture's broker serving PRIV_APP_KEY, a human-tier
// GitHub App secret tagged dispatch-project=PRIV, on a broker that checks projects, with the
// fixture's session naming the projects named.
func newPrivateNarrowingFixture(t *testing.T, named ...string) *narrowingFixture {
	t.Helper()
	st := storetest.Open(t)
	app := policytest.TypeSecret("PRIV_APP_KEY", checkOperator, policy.TierHuman, policy.TypeGitHubApp, standInAppValue(t))
	app.Tags[policy.TagProject] = privateAppProject
	local := secrets.NewLocal(app)
	loader := policytest.Loader(local)
	loader.ProjectChecks = true
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	if _, refused := current.Get().Refused[policytest.ID("PRIV_APP_KEY")]; refused {
		t.Fatalf("the loader refused PRIV_APP_KEY: %+v", current.Get().Refused)
	}
	enrollmentID, key := newEnrollment(t, st)
	if named == nil {
		named = []string{}
	}
	if _, err := st.Pool.Exec(t.Context(), `update enrollments set projects=$2 where id=$1`, enrollmentID, named); err != nil {
		t.Fatalf("name the session's projects: %v", err)
	}
	dispatch := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		privateAppProject: {Visibility: "private", Owner: checkOperator},
	}, nil)
	stand := newGitHubStand(t, map[string]int64{standInOwner: 7})
	machine := &requests.Machine{
		Store:      st,
		Policy:     current,
		Secrets:    policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   checkAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
		Minters:    map[string]requests.Minter{policy.TypeGitHubApp: secrets.GitHubApp{Base: stand.service.URL()}.Mint},
		Projects:   projectaccess.New(dispatch.URL, dispatch.Account, nil),
	}
	machine.Chain = requests.NewChainVerifier(st, checkAudience, time.Minute, machine.Airlock)
	return &narrowingFixture{Machine: machine, enrollmentID: enrollmentID, key: key, stand: stand}
}

// TestNarrowingBeforeApprovalReadsTheProjectFacts: the narrowing check evaluates the request with
// the project facts Create resolved before it, so a session naming PRIV that narrows PRIV_APP_KEY
// to an account the App is not installed on is refused before anybody is asked, as an untagged
// secret's narrowing is; and a session naming no project is denied project-not-named, not checked
// and not sent to a person. Without the facts the check could not evaluate a tagged secret, would
// skip, and the first request would spend an approval on a narrowing nobody can serve.
func TestNarrowingBeforeApprovalReadsTheProjectFacts(t *testing.T) {
	unservable := &secrets.GitHubNarrowing{Owner: otherOwner, ReadOnly: true}

	named := newPrivateNarrowingFixture(t, privateAppProject)
	request, err := named.create(t, unservable, "PRIV_APP_KEY")
	if !errors.Is(err, secrets.ErrMintNoInstallation) {
		t.Fatalf("Create(PRIV_APP_KEY) from a session naming PRIV, narrowed to %s, which the App is not installed on = %+v, %v; want ErrMintNoInstallation before approval", otherOwner, request, err)
	}
	if rows, _, records := heldRows(t, named.Machine); rows != 0 || records != 0 {
		t.Fatalf("the refused narrowing left %d request rows and %d credential-request records; want none", rows, records)
	}

	unnamed := newPrivateNarrowingFixture(t)
	request, err = unnamed.create(t, unservable, "PRIV_APP_KEY")
	if err != nil || request.State != "denied" || request.Detail == nil || !strings.Contains(*request.Detail, policy.ReasonProjectNotNamed) {
		t.Fatalf("Create(PRIV_APP_KEY) from a session naming no project = %+v, %v; want denied %s", request, err, policy.ReasonProjectNotNamed)
	}
	if asked := unnamed.stand.tokenRequests(); len(asked) != 0 {
		t.Fatalf("a request the project rule denies asked GitHub for %d tokens; want none", len(asked))
	}
}
