// packages/envoy/internal/cipherclient/membership_held_test.go
//
// What the client does when the broker answers that Dispatch holds the person a credential
// names: the token endpoint's `membership_held`. A hold is not a revocation. It ends at the
// person's next Dispatch sign-in, or the next refresh that confirms them, and the credential the
// client holds is the one the broker serves again then, with no new login. So the refusal is one
// a later trade cures: the renewal loop goes on trading, and a person's sign-in on this machine
// is not remembered as refused.

package cipherclient

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// heldDescription is the broker's description of its membership_held refusal, with the person's
// login in it.
const heldDescription = "ada@example.com's credentials are held by Dispatch; they work again when ada@example.com signs in to Dispatch, or when Dispatch can confirm ada@example.com with Google again"

// directoryHeldDescription is the same refusal from an Identity-Aware Proxy deployment's broker
// (BROKER_HOLD_REQUIRES_DEADLINE), where the Workspace Directory's next active answer, not a
// sign-in, ends the hold (hold.Verdict.HeldWords).
const directoryHeldDescription = "Google's Workspace Directory no longer confirms ada@example.com; their credentials work again once it does"

// TestMembershipHeldIsNotPermanent: the broker's `membership_held` is not a refusal the client
// stops on, in either of its wordings, and the wire corpus both clients replay holds one row for
// each wording and says the same of each.
func TestMembershipHeldIsNotPermanent(t *testing.T) {
	for _, description := range []string{heldDescription, directoryHeldDescription} {
		refused := &RefusedError{Status: 400, Code: "membership_held", Description: description}
		if refused.Permanent() {
			t.Errorf("the client reads membership_held (%q) as permanent; want a refusal it trades again on: Dispatch releases the hold, and the credential in hand is served again then", description)
		}
	}

	rows := map[string]*corpusExchange{}
	read := readCorpus(t)
	for i := range read.Exchanges {
		row := &read.Exchanges[i]
		if code, _ := row.Answer.Body["error"].(string); code != "membership_held" {
			continue
		}
		description, _ := row.Answer.Body["error_description"].(string)
		if earlier := rows[description]; earlier != nil {
			t.Fatalf("the wire corpus names membership_held with %q in %q and in %q; want one row for each wording", description, earlier.Name, row.Name)
		}
		rows[description] = row
		if row.Answer.Status != 400 || row.Expect.Outcome != "refuse" || row.Expect.Permanent {
			t.Errorf("the corpus row %q answers HTTP %d and expects outcome %q, permanent=%t; want HTTP 400, refuse, permanent=false",
				row.Name, row.Answer.Status, row.Expect.Outcome, row.Expect.Permanent)
		}
		if got := newRefusedError("ada@example.com's sign-in on this machine", row.Answer.Status, "membership_held", description).Permanent(); got != row.Expect.Permanent {
			t.Errorf("the client reads the corpus row %q as permanent=%t; the row says %t", row.Name, got, row.Expect.Permanent)
		}
	}
	for _, description := range []string{heldDescription, directoryHeldDescription} {
		if rows[description] == nil {
			t.Errorf("the wire corpus has no membership_held row worded %q, so the TypeScript client is held to nothing about it", description)
		}
	}
	if len(rows) != 2 {
		t.Errorf("the wire corpus names membership_held in %d wordings; want the broker's two, each in one row", len(rows))
	}
}

// TestMembershipHeldKeepsTheSignInAuthority: a held person's trade is refused, and the sign-in is
// not marked refused. The enrolment fails with the broker's words rather than falling back to
// this machine's own identity, each later enrolment trades again, and the first after the
// release is the person's.
func TestMembershipHeldKeepsTheSignInAuthority(t *testing.T) {
	const person = "ada@example.com"
	var held atomic.Bool
	held.Store(true)
	rig := newSignInRig(t, func(n int, _ url.Values) (int, map[string]any) {
		if held.Load() {
			return 400, map[string]any{"error": "membership_held", "error_description": heldDescription}
		}
		return 200, mintedToken(n + 1)
	})
	rig.signIn(t, person)

	for i := range 2 {
		authority, err := rig.authority.Current(context.Background())
		var refused *RefusedError
		if authority != nil || !errors.As(err, &refused) || refused.Code != "membership_held" {
			t.Fatalf("enrolment %d while %s is held answered %+v, %v; want no authority and the broker's membership_held refusal, so the session neither enrols under this machine nor loses the sign-in", i, person, authority, err)
		}
		if want := "the secrets broker refused " + person + "'s sign-in on this machine: HTTP 400 membership_held"; !strings.Contains(err.Error(), want) {
			t.Fatalf("enrolment %d while %s is held answered %q; want it to name the identity refused, %q", i, person, err, want)
		}
		if n := len(rig.broker.Requests()); n != i+1 {
			t.Fatalf("enrolment %d while %s is held: %d trades; want %d, each enrolment trading again rather than remembering the sign-in as refused", i, person, n, i+1)
		}
	}

	held.Store(false)
	authority, err := rig.authority.Current(context.Background())
	if err != nil {
		t.Fatalf("the first enrolment after the release: %v", err)
	}
	if authority == nil || authority.Operator != person {
		t.Fatalf("the first enrolment after the release answered %+v; want %s's credential, traded under the same sign-in", authority, person)
	}
	if n := len(rig.broker.Requests()); n != 3 {
		t.Fatalf("%d trades; want three: the two refused while held and the one after the release", n)
	}
}

// TestARefusalNamesTheIdentityItRefused: a refusal of a machine source, through the client's real
// trade, reads "this machine's identity", as a refusal always has, and carries no configured value
// its Describe names: a pod's projected token prints no file path. A person's sign-in names the
// person instead; TestMembershipHeldKeepsTheSignInAuthority asserts that through a real
// PersonIdentity.
func TestARefusalNamesTheIdentityItRefused(t *testing.T) {
	const tokenFile = "/var/run/secrets/tokens/not-a-secret-path"
	for _, tc := range []struct {
		name   string
		source Source
		names  string // how the refusal names the identity
	}{
		{"a pod's projected token", &scriptedSource{token: "projected.service.account.token", tokenType: oauthwire.SubjectTokenTypeKubernetes,
			describe: NewProjectedToken(tokenFile).Describe()}, "this machine's identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
				return 400, map[string]any{"error": "invalid_grant", "error_description": "no entry lists this identity"}
			})
			client, err := New(Config{BrokerURL: cipheraddr.New(broker.URL()), Key: testKey(t), Subject: tc.source})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Token(context.Background())
			want := "the secrets broker refused " + tc.names + ": HTTP 400 invalid_grant: no entry lists this identity"
			if err == nil || err.Error() != want {
				t.Fatalf("the refusal reads %v; want %q", err, want)
			}
			if strings.Contains(err.Error(), tokenFile) {
				t.Fatalf("the refusal reads %q: it prints the token file's path, a configured value", err)
			}
		})
	}
}
