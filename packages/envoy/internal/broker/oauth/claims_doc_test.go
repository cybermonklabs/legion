package oauth

import (
	"context"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// The claim list under "The Dispatch identity the broker can issue" in docs/kubernetes.md is what a
// resource server writes its authorization against: it reads "An issued token carries these claims
// and no others", and then trusts both halves of that sentence. A claim this package issues and the
// list omits is a permission nobody downstream knows to check; a claim the list carries and the
// code never issues is a check written against a key that is always absent, which reads as a denial
// the author did not mean. Neither shows up in a test of the exchange itself, because the exchange
// is right in both cases — only the document is wrong.
//
// This half of that guarantee does not read the guide. It reads claims_doc_extract.txt, this
// package's own copy of that list, which internal/textcheck's TestTheClaimExtractIsTheGuidesClaimList
// holds equal to the guide's bullets; the two together say what the one test used to say. The
// split is what takes docs/kubernetes.md out of this job's filter: a declared read applies to
// every compiled source file of a run, so a test that reads the guide starts the whole envoy Go
// lane on a documentation-only edit however the run selects it, and only a test that does not
// read it at all stops doing that.
//
// What is compared: the names the extract lists, against the keys decide writes into an exchange
// session's extra claims, which is where this provider's opaque tokens carry them. The code's side
// is collected by calling decide for each kind of subject token — a pod's, whose entry gives the
// named project scope, a machine access token's, whose scope is the person's, and a machine's
// Google identity token, whose entry names a service — and taking the union, since `project` is
// written under one scope and deleted under the other. So a claim a new scope or a new decision
// adds is a claim this fails until the list names it. The one claim no decision writes, the
// launcher credential a machine's exchange mints (machineCredential, which runs after decide
// because the credential does not exist until the request's key is bound), is added to that union
// by name; what proves it is written is the live exchange in cmd/broker.
//
// What this cannot do: it compares two sets of names, and nothing more. It does not read a bullet's
// meaning, so a claim whose description is stale, wrong, or swapped with its neighbour's passes
// here; it does not know the order the list is written in; it says nothing about the claims an
// introspection answer adds around them (`exp`, `client_id`, `jkt`); and the union is over the two
// subject kinds it calls decide with, so a claim only a workflow run's token could carry would be
// missed. A reader still owns all of that. What it does own is the two sets drifting apart while
// each side looks right on its own.
//
// claimsExtractFile is read from this package's own directory, where the file sits beside the code
// it is about and needs no declared read of its own.
const claimsExtractFile = "claims_doc_extract.txt"

// The listed identity and the two subject tokens decide is asked to decide on here. The account
// entry fixes no session, so the request names one, and the pod token carries the audience the
// configured issuer accepts and the pod binding Set.Pod requires.
const (
	claimsPodAccount   = "system:serviceaccount:example-ns:example-worker"
	claimsPodUID       = "11111111-2222-3333-4444-555555555555"
	claimsPerson       = "ada@example.com"
	claimsProject      = "ACME"
	claimsSessionName  = "claims-doc-check"
	claimsIssuer       = "https://kubernetes.example/cluster"
	claimsAudience     = "https://broker.example"
	claimsCredentialID = "machine-credential-1"
	// The Google half: the issuer a machine's identity token comes from, the audience it must
	// carry, and the account an entry lists for a fleet that names no person.
	claimsGoogleIssuer   = "https://accounts.google.example"
	claimsGoogleAudience = "https://broker.example/machines"
	claimsGoogleAccount  = "example-fleet@example-project.iam.gserviceaccount.com"
	claimsGoogleService  = "example-fleet"
	// The person half: the audience the device-flow client's ID token carries and the Workspace
	// domain a signing-in person must belong to.
	claimsDeviceAudience = "https://broker.example/signin"
	claimsSignInDomain   = "example.com"
)

// The claims claims_doc_extract.txt lists are exactly the claims an exchanged token carries. Both
// sides are read: the extract's names, and the keys decide writes for the two project scopes a
// token can be issued under.
func TestTheDocumentedClaimsAreTheClaimsAnExchangedTokenCarries(t *testing.T) {
	documented := extractedClaims(t)
	issued := issuedClaims(t)
	slices.Sort(documented)
	if !slices.Equal(documented, issued) {
		t.Fatalf("%s's claim list and the claims the exchange decides on disagree:\n"+
			"  documented: %s\n"+
			"  issued:     %s\n"+
			"that list is docs/kubernetes.md's, which is what a resource server writes its "+
			"authorization against",
			claimsExtractFile, strings.Join(documented, ", "), strings.Join(issued, ", "))
	}
}

// issuedClaims are the claim names an exchanged token carries, sorted: the keys decide writes onto
// an exchange session over each kind of subject token and each project scope, and the launcher
// credential a machine's exchange adds after decide has run. A scope that wrote a claim nothing
// documents, or stopped writing one the list still carries, lands in this set.
func issuedClaims(t *testing.T) []string {
	t.Helper()
	policy, err := exchange.NewSet([]exchange.AccountEntry{{
		Account: claimsPodAccount,
		Project: claimsProject,
		Person:  claimsPerson,
	}}, nil, []exchange.GoogleEntry{{Account: claimsGoogleAccount, Service: claimsGoogleService}}, claimsSignInDomain)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	// Keys is left nil: decide verifies no signature. It checks a pod token's audience against the
	// configured issuer (audience, hasAudience) and the library, which is what resolves a token's
	// key, is not in this call at all.
	provider := &Provider{exchange: ExchangeConfig{
		Policy:     policy,
		Kubernetes: IssuerConfig{Issuer: claimsIssuer, Audience: claimsAudience},
		Google:     IssuerConfig{Issuer: claimsGoogleIssuer, Audience: claimsGoogleAudience},
		// The person's own token is checked against Google's issuer too, under the device-flow
		// client's audience; the actor token below is the machine's, under Google's own.
		PersonAudience: claimsDeviceAudience,
	}}

	subjects := []struct {
		tokenType    string
		token, actor map[string]any
	}{{
		tokenType: oauthwire.SubjectTokenTypeKubernetes,
		token: map[string]any{
			"sub":           claimsPodAccount,
			"aud":           claimsAudience,
			"kubernetes.io": map[string]any{"pod": map[string]any{"uid": claimsPodUID}},
		},
	}, {
		tokenType: tokenTypeAccessToken,
		token: map[string]any{
			"sub":                       claimsPerson,
			oauthwire.ExtraCredentialID: claimsCredentialID,
		},
	}, {
		tokenType: oauthwire.SubjectTokenTypeGoogle,
		token: map[string]any{
			"aud":            claimsGoogleAudience,
			"email_verified": true,
			"email":          claimsGoogleAccount,
		},
	}, {
		tokenType: oauthwire.SubjectTokenTypePerson,
		token: map[string]any{
			"sub":            "109" + claimsPerson,
			"aud":            claimsDeviceAudience,
			"email":          claimsPerson,
			"email_verified": true,
			"hd":             claimsSignInDomain,
		},
		actor: map[string]any{
			"aud":            claimsGoogleAudience,
			"email":          claimsGoogleAccount,
			"email_verified": true,
		},
	}}

	issued := map[string]bool{}
	for _, subject := range subjects {
		session := oauthstore.NewExchangeSession()
		request := &oauth2.Request{
			Form: url.Values{
				FormSubjectTokenType:  {subject.tokenType},
				oauthwire.FormSession: {claimsSessionName},
			},
			Session: session,
		}
		if subject.actor != nil {
			// Where the library puts an actor token it has validated, which decide reads a
			// person's sign-in through (rfc8693.CustomJWTTypeHandler).
			session.SetActorToken(subject.actor)
		}
		if _, err := provider.decide(context.Background(), request, subject.token); err != nil {
			t.Fatalf("the exchange refused the %s subject token this check decides against: %v; "+
				"the claims are then unknown and nothing is being held to the list", subject.tokenType, err)
		}
		// A decision that wrote no claim is this check failing open on the code's side, as an
		// empty bullet list is on the document's.
		if len(session.Extra) == 0 {
			t.Fatalf("the exchange decided the %s subject token and wrote no claim at all", subject.tokenType)
		}
		for name := range session.Extra {
			issued[name] = true
		}
	}
	// The launcher credential a machine's exchange answers with, written onto the session by
	// machineCredential once the request's key is bound and the credential exists, which is
	// after decide has run. It is named here rather than decided, so what holds the code to it
	// is the live exchange in cmd/broker, not this check.
	issued[oauthwire.ExtraCredentialID] = true
	return slices.Sorted(maps.Keys(issued))
}

// extractedClaims reads the claim names from claims_doc_extract.txt, in the file's order: one name
// per line, with the blank lines and the `#` header passed over. A name the file carries twice is
// returned twice, so the caller's slices.Equal fails on it rather than a set hiding it.
//
// The parse is a split and a filter because the extract is written to be parsed that way. It knows
// nothing of the guide's bullets, its backticks or its em dash — reading that shape is
// internal/textcheck's job, on the guide itself — so no reader of the guide's own shape is shared
// between the two halves of this check, which is the parser that could agree with itself while
// both drift. The reading of the extract itself is the same few lines in both halves
// (internal/textcheck's extractLines): a copy rather than a shared package, since the two live in
// different packages of this module and sharing nine lines would cost a package of its own.
func extractedClaims(t *testing.T) []string {
	t.Helper()
	// Read it and fail when it cannot be read: a skip here would be an absence that reads exactly
	// like a pass, and a silent absence is what this check exists to stop.
	text, err := os.ReadFile(claimsExtractFile)
	if err != nil {
		t.Fatalf("read %s: %v", claimsExtractFile, err)
	}
	var claims []string
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		claims = append(claims, line)
	}
	// An empty extract is this check failing open: the file is still there, the parse matched
	// nothing, and every issued claim would then be reported against a list nobody is reading.
	if len(claims) == 0 {
		t.Fatalf("%s names no claim at all: the copy of docs/kubernetes.md's list is gone from it, "+
			"and nothing is being held to that list", claimsExtractFile)
	}
	return claims
}
