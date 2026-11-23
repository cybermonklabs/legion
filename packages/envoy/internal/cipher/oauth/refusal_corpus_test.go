package oauth_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"authelia.com/provider/oauth2"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// The refusal texts the TypeScript broker client prints whole (packages/cipher-client,
// isKnownBrokerRefusal), as this broker's own OAuth library and its own Refusal write them on the
// wire. testdata/refusal-descriptions.json holds them, and cipher-client's refusal-corpus test
// reads that file, so neither the library's sentence ahead of a hint nor a hint's words can change
// without a test that names it. Each hint's format string must also stand in the broker's source,
// so a sentence changed there fails here.
type corpusRefusal struct {
	base   *oauth2.RFC6749Error
	format string
	args   []any
	source string
}

const (
	corpusPerson  = "ada@example.com"
	corpusProject = "DSP"
	corpusSession = "ses_corpus"
)

var corpusRefusals = []corpusRefusal{
	{oauth2.ErrInvalidGrant, "%s revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with `agent-secrets login`.", []any{corpusPerson}, "exchange.go"},
	{oauth2.ErrInvalidGrant, "%s's sign-in on this box key was made on another machine, and a sign-in belongs to the machine it was made on. Sign in again with `agent-secrets login`.", []any{corpusPerson}, "exchange.go"},
	{oauth2.ErrInvalidGrant, "The machine login the subject token belongs to has ended.", nil, "exchange.go"},
	{oauth2.ErrInvalidGrant, "The machine login has ended.", nil, "provider.go"},
	{oauth2.ErrInvalidGrant, "Another live machine login holds the key this proof was signed with. A machine makes a key of its own.", nil, "exchange.go"},
	{oauth2.ErrInvalidGrant, "Another live machine login holds the key this proof was signed with. A sign-in makes a box key of its own.", nil, "exchange.go"},
	{oauth2.ErrInvalidRequest, "%s is a restricted member, whose token names its projects: set AGENT_SECRETS_PROJECTS to the projects this session works on.", []any{corpusPerson}, "exchange.go"},
	{oauth2.ErrInvalidRequest, "The token names a project its person cannot see: %s.", []any{corpusProject}, "exchange.go"},
	{oauth2.ErrInvalidRequest, "The subject token was refused: its entry names the project %s, which its person %s cannot see.", []any{corpusProject, corpusPerson}, "exchange.go"},
	{oauth2.ErrInvalidRequest, "The subject token was refused: its entry names no project, and its person %s is a restricted member, who sees only the projects that name them.", []any{corpusPerson}, "exchange.go"},
	{oauth2.ErrTemporarilyUnavailable, "Dispatch could not be asked whether the token's person sees the projects it names; try again.", nil, "exchange.go"},
	{oauth2.ErrInvalidGrant, "The session name '%s' belongs to another identity: one the broker's configuration fixes it for, or one that has written under it within the last %d days. Ask under a name of your own.", []any{corpusSession, 30}, "exchange.go"},
	// The broker writes this one's description as oauthwire.PodSessionNotRegistered and a colon in
	// place of the library's invalid_grant sentence (registeredPodSession).
	{oauth2.ErrInvalidGrant.WithDescription(oauthwire.PodSessionNotRegistered + ":"), "No registration says this pod may write under the session name '%s'. A pod writes under a session name its deployment registered for it.", []any{corpusSession}, "exchange.go"},
	{oauth2.ErrInvalidGrant, "Refused: this pod's registration for session '%s' is bound to another key; a role trades only for its own session.", []any{corpusSession}, "exchange.go"},
	{oauth2.ErrInvalidGrant, "Refused: this pod's registration for session '%s' was released; a pod trades only under a registration its registrar has not released.", []any{corpusSession}, "exchange.go"},
	// The broker writes these two's descriptions as oauthwire.SessionNameNotThisRegistrars and a
	// colon, as it writes the unregistered pod's above (registeredPodSession): the first for a name
	// another daemon holds, the second for a name bound to none.
	{oauth2.ErrInvalidGrant.WithDescription(oauthwire.SessionNameNotThisRegistrars + ":"), "The session name '%s' belongs to another daemon: a pod trades only under a session name the daemon that registered it holds.", []any{corpusSession}, "exchange.go"},
	{oauth2.ErrInvalidGrant.WithDescription(oauthwire.SessionNameNotThisRegistrars + ":"), "The session name '%s' is bound to no daemon: a pod trades only under a session name the daemon that registered it holds.", []any{corpusSession}, "exchange.go"},
}

// The exchange policy's refusals of a subject token, which the broker hands on as "The subject
// token was refused: <the policy's error>." Each is the format the policy writes it with, filled
// with values of the shape cipher-client's slots take; the format must stand in the policy's
// source. The refusal that quotes an error text of its own (`not a person's Google identity token
// (%v)`) is not among them: cipher-client withholds it.
type policyRefusal struct {
	format string
	args   []any
	source string
}

const (
	corpusAccount    = "system:serviceaccount:legion:worker"
	corpusMachine    = "builder@legion-prod.iam.gserviceaccount.com"
	corpusRepository = "cybermonklabs/legion"
	corpusWorkflow   = ".github/workflows/ci.yaml"
	corpusRef        = "refs/heads/main"
	policySource     = "../exchange/exchange.go"
)

const corpusSessionRuleFormat = "a session name is printable, carries no whitespace, '*' or '>', and is at most %d bytes"

var corpusSessionRule = fmt.Sprintf(corpusSessionRuleFormat, exchange.MaxSessionBytes)

var policyRefusals = []policyRefusal{
	{"%w: %s presented a token bound to no pod, which proves only that the account exists", []any{exchange.ErrNoPodBinding, corpusAccount}, policySource},
	{"%w: no entry lists the service account %s", []any{exchange.ErrNoEntry, corpusAccount}, policySource},
	{"%w: %s's tokens all name the session %s, which every pod of it may use, so its pods are registered for no name", []any{exchange.ErrSessionFixed, corpusAccount, "legion-broker"}, policySource},
	{"%w: the registration of %s names a session of %d bytes; %s", []any{exchange.ErrSessionInvalid, corpusAccount, 230, corpusSessionRule}, policySource},
	{"%w: %s ran %s, whose job is written in %s; an entry lists the workflow a job runs in", []any{exchange.ErrNoEntry, corpusRepository, corpusRepository + "/" + corpusWorkflow + "@" + corpusRef, "cybermonklabs/actions/.github/workflows/build.yaml@refs/heads/main"}, policySource},
	{"%w: the subject token's workflow_ref is not <owner>/<name>/<path>@<ref> under its repository %s", []any{exchange.ErrClaimMissing, corpusRepository}, policySource},
	{"%w: no entry lists the workflow %s of %s on %s", []any{exchange.ErrNoEntry, corpusWorkflow, corpusRepository, corpusRef}, policySource},
	{"%w: the subject token carries no email claim, so it names no account (ask for it with format=full)", []any{exchange.ErrClaimMissing}, policySource},
	{"%w: no entry lists the Google account %s", []any{exchange.ErrNoEntry, corpusMachine}, policySource},
	{"%w: %s presented a token whose email_verified is not true", []any{exchange.ErrEmailNotVerified, corpusMachine}, policySource},
	{"%w: %s is the service %s and names no person, and the request named %s", []any{exchange.ErrProjectsNoPerson, corpusMachine, "builder", oauthwire.JoinProjects([]string{"DSP", "INFRA"})}, policySource},
	{"%w: %s reads as the service %s and names no person, and the request named %s", []any{exchange.ErrProjectsNoPerson, corpusAccount, "builder", oauthwire.JoinProjects([]string{"DSP", "INFRA"})}, policySource},
	{"%w: %s presented a token carrying no Compute Engine identity, and its entry takes machines of the project %s named %s", []any{exchange.ErrNoComputeEngine, corpusMachine, "legion-prod", "worker-*"}, policySource},
	{"%w: %s presented a token minted in the project %s, and its entry takes machines of %s", []any{exchange.ErrMachineNotAllowed, corpusMachine, "other-project", "legion-prod"}, policySource},
	{"%w: %s presented a token minted on the instance %s, and its entry takes the names %s", []any{exchange.ErrMachineNotAllowed, corpusMachine, "box-1", "worker-*"}, policySource},
	{"%w: a person signs in on a machine, so the machine's own Google identity token is presented as the actor token (%s)", []any{exchange.ErrNoActorToken, oauthwire.ActorTokenTypeMachine}, policySource},
	{"%w: the actor token carries no email claim, so it names no machine (ask for it with format=full)", []any{exchange.ErrClaimMissing}, policySource},
	{"%w: no entry lists the Google account %s, which the actor token names as the machine", []any{exchange.ErrNoEntry, corpusMachine}, policySource},
	{"%w: the machine %s presented an actor token whose email_verified is not true", []any{exchange.ErrEmailNotVerified, corpusMachine}, policySource},
	{"%w: the subject token carries no sub claim, so it names no person", []any{exchange.ErrClaimMissing}, policySource},
	{"%w: the subject token carries no email claim of an address's shape, so it names no person", []any{exchange.ErrClaimMissing}, policySource},
	{"%w: %s signed in, and this broker takes the Google Workspace domain BROKER_SIGNIN_DOMAIN names; sign in again with `agent-secrets login` as an account of that domain", []any{exchange.ErrNotInDomain, corpusPerson}, policySource},
	{"%w: the subject token names no machine credential id, so it is not a machine login's token", []any{exchange.ErrClaimMissing}, policySource},
	{"%w: the machine credential records no approver to act for", []any{exchange.ErrClaimMissing}, policySource},
	{"%w: %s's tokens name the session %s", []any{exchange.ErrSessionMismatch, corpusAccount, "legion-broker"}, policySource},
	{"%w: %s is listed with no session of its own, so the request names one", []any{exchange.ErrSessionRequired, corpusAccount}, policySource},
	{"%w: %s named a session of %d bytes; %s", []any{exchange.ErrSessionInvalid, corpusMachine, 230, corpusSessionRule}, policySource},
	{"%w: the subject token carries no %s claim", []any{exchange.ErrClaimMissing, "repository"}, policySource},
	{"%w: the subject token's %s claim is not a non-empty string", []any{exchange.ErrClaimMissing, "sub"}, policySource},
	{"%w: %s's entry names no project, and the request named %s", []any{exchange.ErrProjectsFixed, corpusAccount, oauthwire.JoinProjects([]string{"DSP", "INFRA"})}, policySource},
	{"%w: %s writes the project %s that its entry names, and the request named %s", []any{exchange.ErrProjectsFixed, corpusRepository + "/" + corpusWorkflow, "DSP", "INFRA"}, policySource},
	{"the subject token was not minted for %s, which is the audience this broker accepts from %s", []any{"legion-broker", "https://kubernetes.default.svc"}, "exchange.go"},
}

// The OAuth error codes cipher-client shows (refusal-text.ts, KNOWN_OAUTH_ERROR_CODES), as this
// broker's library writes them: each is the library's own error value's code, so the client's
// list and the codes the broker can answer are read from one place. Every library error the
// broker's token endpoint returns (exchange.go, provider.go, clientauth.go) must be one of these.
var corpusCodes = map[string]*oauth2.RFC6749Error{
	"ErrInvalidRequest":         oauth2.ErrInvalidRequest,
	"ErrInvalidClient":          oauth2.ErrInvalidClient,
	"ErrInvalidGrant":           oauth2.ErrInvalidGrant,
	"ErrUnauthorizedClient":     oauth2.ErrUnauthorizedClient,
	"ErrUnsupportedGrantType":   oauth2.ErrUnsupportedGrantType,
	"ErrInvalidScope":           oauth2.ErrInvalidScope,
	"ErrAccessDenied":           oauth2.ErrAccessDenied,
	"ErrServerError":            oauth2.ErrServerError,
	"ErrTemporarilyUnavailable": oauth2.ErrTemporarilyUnavailable,
	"ErrInvalidTarget":          oauth2.ErrInvalidTarget,
	"ErrInvalidDPoPProof":       oauth2.ErrInvalidDPoPProof,
	"ErrUseDPoPNonce":           oauth2.ErrUseDPoPNonce,
}

func TestTheCodeCorpusIsWhatTheLibraryWrites(t *testing.T) {
	path := filepath.Join("testdata", "refusal-codes.json")
	var want []string
	for _, value := range corpusCodes {
		want = append(want, value.ErrorField)
	}
	sort.Strings(want)
	if os.Getenv("UPDATE_REFUSAL_CORPUS") == "1" {
		encoded, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s is not a JSON list of strings: %v", path, err)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%s is not the library's codes; run UPDATE_REFUSAL_CORPUS=1 go test -run %s ./internal/cipher/oauth and check cipher-client's list.\nwant: %v\ngot:  %v", path, t.Name(), want, got)
	}
}

func TestEveryLibraryErrorTheTokenEndpointReturnsIsInTheCodeCorpus(t *testing.T) {
	used := regexp.MustCompile(`oauth2\.(Err[A-Z][A-Za-z]*)`)
	found := 0
	for _, source := range []string{"exchange.go", "provider.go", "clientauth.go"} {
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, match := range used.FindAllStringSubmatch(line, -1) {
				name := match[1]
				if strings.HasPrefix(name, "ErrorTo") {
					continue
				}
				found++
				if _, ok := corpusCodes[name]; !ok {
					t.Errorf("%s returns oauth2.%s, whose code cipher-client does not show: add it to corpusCodes and KNOWN_OAUTH_ERROR_CODES", source, name)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no oauth2.Err value found in the token endpoint's source: the scan reads nothing")
	}
}

// The broker's own refusal, as its token endpoint writes its rate limit.
const corpusRateLimit = "too many token requests; try again later"

func corpusDescriptions(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, refusal := range corpusRefusals {
		hint := refusal.format
		if len(refusal.args) > 0 {
			hint = fmt.Sprintf(refusal.format, refusal.args...)
		}
		out = append(out, refusal.base.WithHint(hint).GetDescription())
	}
	// As the broker's token exchange hands a policy refusal on (exchange.go, "The subject token was
	// refused: %s.").
	for _, refusal := range policyRefusals {
		err := fmt.Errorf(refusal.format, refusal.args...)
		out = append(out, oauth2.ErrInvalidRequest.WithHintf("The subject token was refused: %s.", err).GetDescription())
	}
	recorder := httptest.NewRecorder()
	oauth.WriteRefusal(recorder, &oauth.Refusal{Status: 429, Code: "RATE_LIMITED", Message: corpusRateLimit})
	var body struct {
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the rate limit's answer is not JSON: %v", err)
	}
	return append(out, body.Description)
}

func TestTheRefusalCorpusIsWhatTheBrokerWrites(t *testing.T) {
	path := filepath.Join("testdata", "refusal-descriptions.json")
	want := corpusDescriptions(t)
	if os.Getenv("UPDATE_REFUSAL_CORPUS") == "1" {
		encoded, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s is not a JSON list of strings: %v", path, err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s is not what this broker writes; run UPDATE_REFUSAL_CORPUS=1 go test -run %s ./internal/cipher/oauth and check cipher-client's list.\nwant:\n%s\ngot:\n%s", path, t.Name(), strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestEachCorpusHintStandsInTheBrokersSource(t *testing.T) {
	sources := map[string]string{}
	read := func(path string) string {
		text, ok := sources[path]
		if !ok {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text = string(raw)
			sources[path] = text
		}
		return text
	}
	// The format as Go source writes it: a backquote inside a double-quoted string is itself.
	for _, refusal := range corpusRefusals {
		if !strings.Contains(read(refusal.source), refusal.format) {
			t.Errorf("%s holds no hint %q: the broker's sentence changed, so the corpus and cipher-client's list must too", refusal.source, refusal.format)
		}
	}
	for _, refusal := range policyRefusals {
		if !strings.Contains(read(refusal.source), refusal.format) {
			t.Errorf("%s holds no refusal %q: the policy's sentence changed, so the corpus and cipher-client's list must too", refusal.source, refusal.format)
		}
	}
	if !strings.Contains(read(policySource), strconv.Quote(corpusSessionRuleFormat)) {
		t.Errorf("%s holds no session rule as the corpus writes it", policySource)
	}
	raw, err := os.ReadFile(filepath.Join("..", "api", "handlers_oauth.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), corpusRateLimit) {
		t.Errorf("api/handlers_oauth.go holds no rate-limit message %q", corpusRateLimit)
	}
}
