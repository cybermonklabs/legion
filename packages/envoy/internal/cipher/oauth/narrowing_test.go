package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"authelia.com/provider/oauth2/handler/rfc8693"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// tradeKinds is every kind of trade the exchange mints a token for, under the subject token type
// the trade presents. The token type alone does not tell every kind apart: a pod's token is a
// pod entry's that names a person, one's that names none, or a read entry's, and a machine's
// Google identity token is an entry's that names a person or one's that names none, which only
// the entry the broker's configuration lists decides.
// So the kinds are written here by hand, and TestEveryTradeKindNarrowsOnRequest ties the list to
// the code: every subject token type the exchange client accepts must be listed here, and every
// type listed here must be one the client accepts. A new type the client takes fails that test
// until its kinds are written here.
var tradeKinds = map[string][]string{
	oauthwire.SubjectTokenTypeKubernetes:  {"pod-entry", "pod-entry-no-person", "read-entry"},
	oauthwire.SubjectTokenTypeGitHub:      {"workflow-entry"},
	oauthwire.SubjectTokenTypeGoogle:      {"google-identity-no-person", "google-identity-with-a-person"},
	oauthwire.SubjectTokenTypePerson:      {"person-sign-in"},
	oauthwire.SubjectTokenTypeAccessToken: {"machine-login"},
}

// narrowingRow is how the narrowing tests trade one kind of tradeKinds.
type narrowingRow struct {
	// trade is one trade of the row's kind on a narrowing fixture (newNarrowingFixture), asking
	// for no scope. Each call mints the trade's subject token and keys afresh, and makes whatever
	// the trade needs first, so a test calls it once for each trade it sends.
	trade func(t *testing.T, f *signInFixture) narrowingTrade
}

// narrowingTrade is one trade a narrowing test sends (exchangeFixture.send): its form, asking for
// no scope; the key its DPoP proof is signed with; the key its DPoP-Holder proof is signed with
// when it asks for read, nil for a trade that sends none; and the session name its token carries.
type narrowingTrade struct {
	form    url.Values
	key     *ecdsa.PrivateKey
	holder  *ecdsa.PrivateKey
	session string
}

// readKey is the key tr's token is bound to when it asks for read: its holder's, and its DPoP
// proof's for a trade that sends no holder proof.
func (tr narrowingTrade) readKey() *ecdsa.PrivateKey {
	if tr.holder != nil {
		return tr.holder
	}
	return tr.key
}

// The session names the narrowing rows trade under, one for each kind, and the pod uid of the pod
// entry's row.
const (
	narrowedPodUID        = "pod-uid-narrowed"
	narrowedPodSession    = "narrowed-pod"
	narrowedRunSession    = "ci-narrowed"
	narrowedFleetSession  = "fleet-narrowed"
	narrowedPinnedSession = "pinned-narrowed"
	narrowedBoxSession    = "ada-box-narrowed"
	narrowedDevBoxSession = "ada-devbox-narrowed"
	narrowedGoldenCut     = "2026-11-18T09:30:00.123456Z"
	narrowedGoldenCutKey  = "machine-login-with-a-cut"
)

// pinnedMachine is the Google account of a second fleet, whose entry names a person and a
// project, as BROKER_VM_EXCHANGE_ACCOUNTS writes one; pinnedFleet is its service.
const (
	pinnedMachine = "example-pinned@example-project.iam.gserviceaccount.com"
	pinnedFleet   = "example-pinned"
)

// personlessAccount is a pod account whose entry names no person and no project and fixes
// personlessSession (<service account>===<session>): its tokens act as the account itself, which
// they name as their service.
const (
	personlessAccount = "system:serviceaccount:example-ns:example-public"
	personlessSession = "example-public-session"
)

// narrowingRows is the narrowing row of every kind of trade, keyed by the kind's name as
// tradeKinds spells it, which is also the key of the kind's answer in
// internal/ciphertoken/testdata/narrowed-answers.json.
var narrowingRows = map[string]narrowingRow{
	"pod-entry": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		f.registers(t, account, narrowedPodUID, narrowedPodSession)
		return narrowingTrade{form: exchangeForm(f.podToken(t, narrowedPodUID, nil), oauthwire.SubjectTokenTypeKubernetes, narrowedPodSession),
			key: newKey(t), session: narrowedPodSession}
	}},
	"pod-entry-no-person": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		return narrowingTrade{form: exchangeForm(f.podTokenOf(t, personlessAccount, "pod-uid-public"), oauthwire.SubjectTokenTypeKubernetes, ""),
			key: newKey(t), session: personlessSession}
	}},
	"read-entry": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		return narrowingTrade{form: f.readPodForm(t), key: newKey(t), session: readSession}
	}},
	"workflow-entry": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		return narrowingTrade{form: exchangeForm(f.workflowToken(t, uuid.NewString(), nil), oauthwire.SubjectTokenTypeGitHub, narrowedRunSession),
			key: newKey(t), session: narrowedRunSession}
	}},
	"google-identity-no-person": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		return narrowingTrade{form: exchangeForm(f.machineToken(t, signInMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, narrowedFleetSession),
			key: newKey(t), session: narrowedFleetSession}
	}},
	"google-identity-with-a-person": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		return narrowingTrade{form: exchangeForm(f.machineToken(t, pinnedMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, narrowedPinnedSession),
			key: newKey(t), session: narrowedPinnedSession}
	}},
	"person-sign-in": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		// A sign-in's read trade names the credential the sign-in already holds, which an
		// ordinary trade of the box key mints first.
		box := newKey(t)
		f.exchanged(t, f.personTrade(t, narrowedBoxSession), box)
		return narrowingTrade{form: f.personTrade(t, narrowedBoxSession), key: box, holder: newKey(t), session: narrowedBoxSession}
	}},
	"machine-login": {trade: func(t *testing.T, f *signInFixture) narrowingTrade {
		machine := newKey(t)
		return narrowingTrade{form: exchangeForm(f.devBoxToken(t, machine, person), oauthwire.SubjectTokenTypeAccessToken, narrowedDevBoxSession),
			key: machine, holder: newKey(t), session: narrowedDevBoxSession}
	}},
}

// newNarrowingFixture is a sign-in fixture whose exchange lists every kind of trade tradeKinds
// names: the exchange fixture's accounts, personlessAccount's entry, which names no person and no
// project, the read entry and workflows, the sign-in fixture's fleet, whose entry names no person,
// and pinnedMachine's fleet, whose entry names person and project. adjust is applied to the
// configuration last.
func newNarrowingFixture(t *testing.T, adjust func(*oauth.Config)) *signInFixture {
	t.Helper()
	return newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicyReading(t,
			[]exchange.AccountEntry{
				{Account: account, Project: project, Person: person, Issuers: []string{clusterName}},
				{Account: daemonAccount, Project: project, Person: person, Session: daemonSession, Issuers: []string{clusterName}},
				{Account: otherAccount, Project: otherProject, Person: otherPerson, Issuers: []string{clusterName}},
				{Account: personlessAccount, Session: personlessSession, Issuers: []string{clusterName}},
			},
			[]exchange.ReadEntry{{Account: readAccount, Service: readService, Session: readSession, Issuers: []string{clusterName}}},
			[]exchange.WorkflowEntry{
				{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
				{Repository: repository, Workflow: deployWorkflow, Ref: ref, Project: project, Person: person, Session: deploySession},
			},
			exchange.GoogleEntry{Account: signInMachine, Service: signInFleet},
			exchange.GoogleEntry{Account: pinnedMachine, Service: pinnedFleet, Project: project, Person: person})
		adjust(c)
	})
}

// personTrade is a person's sign-in trade on signInMachine, with fresh tokens of both halves.
func (f *signInFixture) personTrade(t *testing.T, session string) url.Values {
	t.Helper()
	return signInForm(f.personToken(t, person, deviceAudience, true, signInDomain), f.machineToken(t, signInMachine, machineAudience, true), session)
}

// send posts tr asking for the scopes scope names ("" for none) and the cut cut ("" for none),
// with a DPoP proof signed by tr.key and, when it asks for read and has a holder, a DPoP-Holder
// proof signed by tr.holder, and answers the status and the JSON body.
func (f *exchangeFixture) send(t *testing.T, tr narrowingTrade, scope, cut string) (int, map[string]any) {
	t.Helper()
	form := maps.Clone(tr.form)
	if scope != "" {
		form.Set("scope", scope)
	}
	if cut != "" {
		form.Set(oauthwire.FormAsOf, cut)
	}
	headers := http.Header{"DPoP": {dpopProof(t, tr.key, f.url+"/token")}}
	if tr.holder != nil && slices.Contains(strings.Fields(scope), oauthwire.ScopeRead) {
		headers.Set(oauthwire.HeaderDPoPHolder, dpopProof(t, tr.holder, f.url+"/token"))
	}
	return f.postWithHeaders(t, form, headers)
}

// postWithHeaders posts form to the token endpoint with headers, and answers the status and the
// JSON body.
func (f *fixture) postWithHeaders(t *testing.T, form url.Values, headers http.Header) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the token answer: %v", err)
	}
	return resp.StatusCode, body
}

// tokenRows is how many access tokens either table holds.
func (f *fixture) tokenRows(t *testing.T) int {
	t.Helper()
	return f.count(t, "oauth_access_tokens") + f.count(t, "oauth_narrowed_access_tokens")
}

// narrowedAnswer is the golden answer of kind (ciphertokentest.NarrowedAnswers): bound to jkt,
// naming session and expiring at exp.
func narrowedAnswer(t *testing.T, kind, jkt, session string, exp int64) oauthwire.Introspection {
	t.Helper()
	answer, ok := ciphertokentest.NarrowedAnswers(t, jkt, session, exp)[kind]
	if !ok {
		t.Fatalf("the narrowed answers hold no answer for %s", kind)
	}
	return answer
}

// withRecordedAccount copies into want what the broker answers beside a token whose credential
// records it, after holding each to what the trade made: for kind person-sign-in, the Google
// account of the sign-in (the issuer of this suite's Google stand-in and the person token's sub),
// and for kind machine-login, when its machine login was made (within a minute of now, the broker's
// clock in this test). A golden answer and a literal hold neither, since the issuer's URL and the
// time change at every run; every other kind records neither, and want is left as it is.
func withRecordedAccount(t *testing.T, f *signInFixture, kind string, answer oauthwire.Introspection, want *oauthwire.Introspection) {
	t.Helper()
	switch kind {
	case "person-sign-in":
		issuer, subject := oidc.CanonicalIssuer(f.google.URL()), "109"+person
		if answer.AccountIssuer != issuer || answer.AccountSubject != subject {
			t.Errorf("the sign-in's token answers the account %q %q, want %q %q, the account its person token named",
				answer.AccountIssuer, answer.AccountSubject, issuer, subject)
		}
		want.AccountIssuer, want.AccountSubject = answer.AccountIssuer, answer.AccountSubject
	case "machine-login":
		made := time.Unix(answer.CredentialCreatedAt, 0)
		if since := time.Since(made); since < -time.Minute || since > time.Minute {
			t.Errorf("the token answers its credential was made at %s, %s from now; want within a minute of now", made, since)
		}
		want.CredentialCreatedAt = answer.CredentialCreatedAt
	}
}

// TestEveryTradeKindNarrowsOnRequest holds narrowingRows to every kind of trade the exchange
// mints a token for. The kinds are those of every subject token type the exchange client accepts,
// read from the client's own registration (oauthstore, GetClient), with the read entry beside the
// pod entry under the Kubernetes type (tradeKinds). It fails for each accepted type tradeKinds
// does not list, each type it lists that the client does not accept, each kind with no row, and
// each row that names no kind. Every problem is collected first and the test fails once, naming
// each of them.
func TestEveryTradeKindNarrowsOnRequest(t *testing.T) {
	// GetClient reads nothing of the store's own state: the two clients are the code's.
	client, err := (&oauthstore.Store{}).GetClient(context.Background(), oauthwire.ExchangeClientID)
	if err != nil {
		t.Fatalf("GetClient(%s): %v", oauthwire.ExchangeClientID, err)
	}
	exchanging, ok := client.(rfc8693.Client)
	if !ok {
		t.Fatalf("the exchange client is a %T, which answers no subject token types", client)
	}
	accepted := exchanging.GetSupportedSubjectTokenTypes()
	if len(accepted) == 0 {
		t.Fatal("the exchange client accepts no subject token type, so the walk read nothing")
	}

	var problems []string
	kindOf := map[string]string{}
	for _, tokenType := range accepted {
		kinds, listed := tradeKinds[tokenType]
		if !listed {
			problems = append(problems, "the subject token type "+tokenType+": the exchange client accepts it, and tradeKinds lists no kind of trade under it")
		}
		for _, kind := range kinds {
			kindOf[kind] = tokenType
		}
	}
	for tokenType := range tradeKinds {
		if !slices.Contains(accepted, tokenType) {
			problems = append(problems, "the subject token type "+tokenType+": tradeKinds lists it, and the exchange client does not accept it")
		}
	}
	for kind := range kindOf {
		if _, rowed := narrowingRows[kind]; !rowed {
			problems = append(problems, "the kind "+kind+": no row in narrowingRows")
		}
	}
	for kind := range narrowingRows {
		if _, known := kindOf[kind]; !known {
			problems = append(problems, "narrowingRows row "+kind+": it names no kind of trade tradeKinds lists under a type the exchange client accepts")
		}
	}
	sort.Strings(problems)
	t.Logf("the exchange client accepts %d subject token types, under which %d kinds of trade mint a token; narrowingRows holds %d rows",
		len(accepted), len(kindOf), len(narrowingRows))
	if len(problems) > 0 {
		t.Errorf("the narrowing inventory (narrowing_test.go) has %d problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// TestATradeThatAsksForReadKeepsItsIdentityAndHoldsRead: every kind of trade, asking for read, is
// answered a token that introspects as its golden answer in
// internal/ciphertoken/testdata/narrowed-answers.json, the file every reader of a narrowed token
// is held to: the narrowed word of its project scope, the scope read, or read person for a token
// that acts for its person, bound to the holder's key where the trade sent a holder proof and to
// its DPoP proof's key where it sent none. It names the same session, person, service, machine
// and projects as the same trade asking for no scope, the narrowed word's base is that trade's
// word, and no answer holds approve. The token answer names the scope it holds and no launcher
// credential. A machine login's trade that names a cut is held to its own golden answer.
func TestATradeThatAsksForReadKeepsItsIdentityAndHoldsRead(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	type kindCase struct{ answer, kind, cut string }
	var cases []kindCase
	for _, kind := range slices.Sorted(maps.Keys(narrowingRows)) {
		cases = append(cases, kindCase{answer: kind, kind: kind})
	}
	cases = append(cases, kindCase{answer: narrowedGoldenCutKey, kind: "machine-login", cut: narrowedGoldenCut})
	for _, c := range cases {
		t.Run(c.answer, func(t *testing.T) {
			row := narrowingRows[c.kind]
			plainStatus, plainBody := f.send(t, row.trade(t, f), "", "")
			plain := f.introspected(t, accessTokenOf(t, plainStatus, plainBody))
			tr := row.trade(t, f)
			status, body := f.send(t, tr, oauthwire.ScopeRead, c.cut)
			answer := f.introspected(t, accessTokenOf(t, status, body))
			want := narrowedAnswer(t, c.answer, thumbprint(t, tr.readKey()), tr.session, answer.ExpiresAt)
			withRecordedAccount(t, f, c.kind, answer, &want)
			if !reflect.DeepEqual(answer, want) {
				t.Errorf("Introspect = %+v, want the golden answer %+v", answer, want)
			}
			for _, field := range []struct {
				name        string
				read, plain any
			}{
				{"session", answer.Session, plain.Session},
				{"person", answer.Person, plain.Person},
				{"service", answer.Service, plain.Service},
				{"machine", answer.Machine, plain.Machine},
				{"projects", strings.Join(answer.Projects, " "), strings.Join(plain.Projects, " ")},
				{"base project scope", oauthwire.BaseProjectScope(answer.ProjectScope), plain.ProjectScope},
			} {
				if field.read != field.plain {
					t.Errorf("the read trade's %s is %v, and the same trade asking for no scope names %v", field.name, field.read, field.plain)
				}
			}
			if answer.ProjectScope == plain.ProjectScope {
				t.Errorf("the read trade carries the project scope %q, the very word the same trade asking for no scope carries", answer.ProjectScope)
			}
			if slices.Contains(strings.Fields(answer.Scope), "approve") {
				t.Errorf("the read token holds the scope %q, which names approve", answer.Scope)
			}
			if body["scope"] != want.Scope {
				t.Errorf("the token answer names the scope %v, want %q", body["scope"], want.Scope)
			}
			for _, named := range []string{oauthwire.ExtraCredentialID, oauthwire.ExtraCredentialExpiresAt} {
				if value, carried := body[named]; carried {
					t.Errorf("the token answer carries %s %v, and a read trade's answer names no launcher credential", named, value)
				}
			}
		})
	}
}

// TestATradeThatAsksForReadWithACutCarriesIt: a read trade's cut is in its introspection answer
// exactly, in UTC to the microsecond whatever offset and digits it was sent with. A cut on a trade
// that does not ask for read, a cut later than now, one with seven fractional digits, one that is
// not RFC 3339, and the zero time, which would read as no cut at all, with or without an offset,
// are each refused 400 invalid_request naming as_of, and no token is stored in either table.
func TestATradeThatAsksForReadWithACutCarriesIt(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	row := narrowingRows["pod-entry"]
	for _, c := range []struct{ name, cut, want string }{
		{"in UTC to the microsecond", narrowedGoldenCut, narrowedGoldenCut},
		{"with an offset and one fractional digit", "2026-11-18T10:30:00.5+01:00", "2026-11-18T09:30:00.500000Z"},
		{"with no fractional digit", "2026-11-18T09:30:00Z", "2026-11-18T09:30:00.000000Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.send(t, row.trade(t, f), oauthwire.ScopeRead, c.cut)
			if answer := f.introspected(t, accessTokenOf(t, status, body)); answer.AsOf != c.want {
				t.Errorf("the token's cut introspects as %q, want %q", answer.AsOf, c.want)
			}
		})
	}
	for _, c := range []struct{ name, scope, cut string }{
		{"a cut without read", "", "2026-11-18T09:30:00Z"},
		{"a cut later than now", oauthwire.ScopeRead, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		{"a cut with seven fractional digits", oauthwire.ScopeRead, "2026-11-18T09:30:00.1234567Z"},
		{"a cut that is not RFC 3339", oauthwire.ScopeRead, "2026-11-18 09:30:00"},
		{"a cut at the zero time", oauthwire.ScopeRead, "0001-01-01T00:00:00Z"},
		{"a cut at the zero time with an offset", oauthwire.ScopeRead, "0001-01-01T01:00:00+01:00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := f.tokenRows(t)
			status, body := f.send(t, row.trade(t, f), c.scope, c.cut)
			refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", oauthwire.FormAsOf)
			if after := f.tokenRows(t); after != before {
				t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
			}
		})
	}
}

// TestTheMachineLoginTradeMayAskForRead: a machine login is granted the scope machine alone, and
// its token as the subject may still ask for read, alone or beside machine, and narrows; asking
// for approve, alone or beside read, is invalid_scope and mints nothing.
func TestTheMachineLoginTradeMayAskForRead(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	row := narrowingRows["machine-login"]
	for _, scope := range []string{oauthwire.ScopeRead, oauthwire.ScopeRead + " " + oauthwire.Scope, oauthwire.Scope + " " + oauthwire.ScopeRead} {
		t.Run(scope, func(t *testing.T) {
			status, body := f.send(t, row.trade(t, f), scope, "")
			answer := f.introspected(t, accessTokenOf(t, status, body))
			if answer.ProjectScope != oauthwire.ProjectScopeReadPerson || answer.Scope != oauthwire.ScopeRead+" "+oauthwire.ScopePerson || answer.Person != person {
				t.Errorf("a machine login's trade asking %q introspects as %+v, want %s's read-person token holding read person", scope, answer, person)
			}
		})
	}
	for _, scope := range []string{"approve", oauthwire.ScopeRead + " approve"} {
		t.Run(scope, func(t *testing.T) {
			tr := row.trade(t, f)
			before := f.tokenRows(t)
			status, body := f.send(t, tr, scope, "")
			if status != http.StatusBadRequest || body["error"] != "invalid_scope" || body["access_token"] != nil {
				t.Fatalf("a machine login's trade asking %q = %d %v, want 400 invalid_scope and no access token", scope, status, body)
			}
			if after := f.tokenRows(t); after != before {
				t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
			}
		})
	}
}

// sameAnswer fails the test unless the read trade was answered as the plain one: the same status,
// and for a refusal the same code and description.
func sameAnswer(t *testing.T, plainStatus int, plain map[string]any, readStatus int, read map[string]any) {
	t.Helper()
	if readStatus != plainStatus || read["error"] != plain["error"] || read["error_description"] != plain["error_description"] {
		t.Fatalf("the read trade = %d %v %v, and the same trade asking for no scope = %d %v %v", readStatus, read["error"], read["error_description"],
			plainStatus, plain["error"], plain["error_description"])
	}
}

// TestANarrowedTradeIsCheckedAsItsBaseWord: the project check reads a narrowed token's base word,
// so under each branch of projectAccess a trade asking for read is answered as the same trade
// asking for none. A pod entry naming a project mints with no check configured and with a check
// that sees it, and is refused naming the project with a check that does not; an entry naming no
// project mints while Dispatch cannot be asked, logging the unchecked mint for each trade; a
// person's trade naming a project with no check configured is refused naming the setting. A
// read-named token stored with no list of projects introspects not live, as a named one does.
func TestANarrowedTradeIsCheckedAsItsBaseWord(t *testing.T) {
	row := narrowingRows["pod-entry"]
	both := func(t *testing.T, f *signInFixture, trade func() narrowingTrade) (map[string]any, int, map[string]any) {
		t.Helper()
		plainStatus, plain := f.send(t, trade(), "", "")
		readStatus, read := f.send(t, trade(), oauthwire.ScopeRead, "")
		sameAnswer(t, plainStatus, plain, readStatus, read)
		return plain, readStatus, read
	}
	t.Run("an entry naming a project mints with no check and with one that sees it", func(t *testing.T) {
		for _, access := range []*fakeAccess{nil, newFakeAccess()} {
			f := newNarrowingFixture(t, func(c *oauth.Config) {
				if access != nil {
					c.Exchange.Projects = access
				}
			})
			_, status, body := both(t, f, func() narrowingTrade { return row.trade(t, f) })
			f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopeReadNamed, []string{project})
		}
	})
	t.Run("an entry naming a project its person cannot see is refused naming it", func(t *testing.T) {
		access := newFakeAccess()
		access.sees[person] = []string{"PRIV"}
		f := newNarrowingFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
		_, status, body := both(t, f, func() narrowingTrade { return row.trade(t, f) })
		refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "its entry names the project "+project+", which its person "+person+" cannot see")
	})
	t.Run("an entry naming no project mints unchecked while Dispatch cannot be asked", func(t *testing.T) {
		logs := capturedLog(t)
		down := newFakeAccess()
		down.kinds[person] = projectaccess.MemberRestricted
		down.down = true
		f := newNarrowingFixture(t, func(c *oauth.Config) {
			c.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Person: person, Issuers: []string{clusterName}}}, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet})
			c.Exchange.Projects = down
		})
		f.registers(t, account, "pod-uid-public", "public-session-1")
		_, status, body := both(t, f, func() narrowingTrade {
			return narrowingTrade{form: exchangeForm(f.podToken(t, "pod-uid-public", nil), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), key: newKey(t)}
		})
		f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopeReadNamed, nil)
		if got := logs.lines("unchecked", account, "public-session-1"); len(got) != 2 {
			t.Fatalf("the two unchecked mints logged %d lines naming %s, want one each:\n%s", len(got), account, strings.Join(logs.lines(), "\n"))
		}
	})
	t.Run("a person's trade naming a project with no check is refused naming the setting", func(t *testing.T) {
		f := newNarrowingFixture(t, func(*oauth.Config) {})
		_, status, body := both(t, f, func() narrowingTrade {
			tr := narrowingRows["machine-login"].trade(t, f)
			tr.form = withProjects(tr.form, "PRIV")
			return tr
		})
		refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "BROKER_PROJECT_ACCESS_URL")
	})
	t.Run("a narrowed named token stored with no list of projects is not live", func(t *testing.T) {
		f := newNarrowingFixture(t, func(*oauth.Config) {})
		plain, _, read := both(t, f, func() narrowingTrade { return row.trade(t, f) })
		for _, table := range []string{"oauth_access_tokens", "oauth_narrowed_access_tokens"} {
			tag, err := f.pool.Exec(context.Background(), `update `+table+` set request = request #- '{session,extra,projects}'
				where request #>> '{session,extra,session}' = $1`, narrowedPodSession)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("drop the projects of the one token of %s: %v, %d rows", table, err, tag.RowsAffected())
			}
		}
		for name, body := range map[string]map[string]any{"named": plain, "read-named": read} {
			if answer := f.introspected(t, accessTokenOf(t, http.StatusOK, body)); !reflect.DeepEqual(answer, oauthwire.Introspection{}) {
				t.Errorf("a %s token stored with no list of projects introspects as %+v, want not live", name, answer)
			}
		}
	})
}

// TestANarrowedTokenIsNoSubjectForAnotherTrade: a token that holds read, a narrowed one with a cut
// or without one and the read entry's own, is never the subject of a trade: presented as an access
// token, asking for no scope, for read with a later cut and for read with no cut, each is refused
// 400, mints nothing and writes no row in either table. The library refuses a client that trades
// its own tokens, and every one of these is the exchange client's.
func TestANarrowedTokenIsNoSubjectForAnotherTrade(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	mint := func(kind, scope, cut string) (string, *ecdsa.PrivateKey) {
		tr := narrowingRows[kind].trade(t, f)
		status, body := f.send(t, tr, scope, cut)
		if scope == "" {
			return accessTokenOf(t, status, body), tr.key
		}
		return accessTokenOf(t, status, body), tr.readKey()
	}
	for _, subject := range []struct{ name, kind, scope, cut string }{
		{"a narrowed token", "pod-entry", oauthwire.ScopeRead, ""},
		{"a narrowed token with a cut", "machine-login", oauthwire.ScopeRead, narrowedGoldenCut},
		{"the read entry's token", "read-entry", "", ""},
	} {
		token, key := mint(subject.kind, subject.scope, subject.cut)
		for _, ask := range []struct{ name, scope, cut string }{
			{"asking for no scope", "", ""},
			{"asking for read with a later cut", oauthwire.ScopeRead, "2026-11-19T12:00:00Z"},
			{"asking for read with no cut", oauthwire.ScopeRead, ""},
		} {
			t.Run(subject.name+" "+ask.name, func(t *testing.T) {
				before := f.tokenRows(t)
				tr := narrowingTrade{form: exchangeForm(token, oauthwire.SubjectTokenTypeAccessToken, "traded-again"), key: key}
				status, body := f.send(t, tr, ask.scope, ask.cut)
				if status != http.StatusBadRequest || body["access_token"] != nil {
					t.Fatalf("a trade of %s %s = %d %v, want 400 and no access token", subject.name, ask.name, status, body)
				}
				if after := f.tokenRows(t); after != before {
					t.Errorf("the refused trade left %d access tokens, want the %d there were", after, before)
				}
			})
		}
	}
}

// TestANarrowedTokenExpiresNoLaterThanItsSubject: a narrowed token never outlives the token it
// was traded from. For a pod's token under an entry that names a person and under one that names
// none, a workflow run's, a machine's and a person's subject token whose exp is a minute away,
// and for a machine login's access token with a minute left, the narrowed token's exp, its row's
// expires_at and its answer's expires_in all end no later than the subject, though the access
// lifetime is fifteen minutes.
func TestANarrowedTokenExpiresNoLaterThanItsSubject(t *testing.T) {
	f := newNarrowingFixture(t, func(*oauth.Config) {})
	expires := func(claims map[string]any, at time.Time) { claims["exp"] = at.Unix() }
	googleToken := func(t *testing.T, account, audience, sub string, at time.Time, extra map[string]any) string {
		claims := map[string]any{
			"iss": f.google.URL(), "sub": sub, "aud": audience, "iat": time.Now().Add(-time.Minute).Unix(), "exp": at.Unix(),
			"email": account, "email_verified": true,
		}
		maps.Copy(claims, extra)
		return f.google.Mint(t, f.googleKey, claims)
	}
	for _, c := range []struct {
		name  string
		trade func(t *testing.T, at time.Time) narrowingTrade
	}{
		{"a pod's token", func(t *testing.T, at time.Time) narrowingTrade {
			f.registers(t, account, narrowedPodUID, narrowedPodSession)
			subject := f.podToken(t, narrowedPodUID, func(claims map[string]any) { expires(claims, at) })
			return narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, narrowedPodSession), key: newKey(t)}
		}},
		{"the pod's token of an entry naming no person", func(t *testing.T, at time.Time) narrowingTrade {
			subject := f.podToken(t, "pod-uid-public", func(claims map[string]any) {
				claims["sub"] = personlessAccount
				expires(claims, at)
			})
			return narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, ""), key: newKey(t)}
		}},
		{"a workflow run's token", func(t *testing.T, at time.Time) narrowingTrade {
			subject := f.workflowToken(t, uuid.NewString(), func(claims map[string]any) { expires(claims, at) })
			return narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeGitHub, narrowedRunSession), key: newKey(t)}
		}},
		{"a machine's Google identity token", func(t *testing.T, at time.Time) narrowingTrade {
			subject := googleToken(t, signInMachine, machineAudience, "110"+signInMachine, at, nil)
			return narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeGoogle, narrowedFleetSession), key: newKey(t)}
		}},
		{"a person's Google identity token", func(t *testing.T, at time.Time) narrowingTrade {
			box := newKey(t)
			f.exchanged(t, f.personTrade(t, narrowedBoxSession), box)
			subject := googleToken(t, person, deviceAudience, "109"+person, at, map[string]any{"hd": signInDomain})
			form := signInForm(subject, f.machineToken(t, signInMachine, machineAudience, true), narrowedBoxSession)
			return narrowingTrade{form: form, key: box, holder: newKey(t)}
		}},
		{"a machine login's access token", func(t *testing.T, at time.Time) narrowingTrade {
			machine, credential := newKey(t), f.newCredential(t)
			subject := f.devBoxTokenOf(t, machine, person, credential)
			// A minute left on the login's token, in its row and in its stored session alike.
			if _, err := f.pool.Exec(context.Background(), `update oauth_access_tokens set expires_at = $2,
				request = jsonb_set(request, '{session,expires_at,access_token}', to_jsonb($3::text))
				where credential_id = $1`, credential, at, at.UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatalf("shorten the machine login's token: %v", err)
			}
			return narrowingTrade{form: exchangeForm(subject, oauthwire.SubjectTokenTypeAccessToken, narrowedDevBoxSession), key: machine, holder: newKey(t)}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			at := time.Now().Add(time.Minute).Truncate(time.Second)
			tr := c.trade(t, at)
			before := time.Now()
			status, body := f.send(t, tr, oauthwire.ScopeRead, "")
			token := accessTokenOf(t, status, body)
			answer := f.introspected(t, token)
			if answer.ExpiresAt > at.Unix() {
				t.Errorf("the narrowed token's exp is %d, %d seconds after its subject's %d", answer.ExpiresAt, answer.ExpiresAt-at.Unix(), at.Unix())
			}
			var row time.Time
			if err := f.pool.QueryRow(context.Background(), `select expires_at from oauth_narrowed_access_tokens where jwk_thumbprint = $1`,
				thumbprint(t, tr.readKey())).Scan(&row); err != nil {
				t.Fatalf("read the narrowed token's row: %v", err)
			}
			if row.After(at) {
				t.Errorf("the narrowed token's row expires at %s, after its subject's %s", row, at)
			}
			seconds, _ := body["expires_in"].(float64)
			if seconds <= 0 || before.Add(time.Duration(seconds)*time.Second).After(at) {
				t.Errorf("the token answer's expires_in is %v, which ends after its subject's %s", body["expires_in"], at)
			}
		})
	}
}
