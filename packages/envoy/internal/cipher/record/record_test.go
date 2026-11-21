package record

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/internal/cipher/proof"
)

func secretDetail() []AuthorizationDetail {
	return []AuthorizationDetail{
		{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}
}

func TestCanonicalBodyIsByteExactAndIDIsItsSHA256(t *testing.T) {
	b := Body{
		Request:         "eyJ.fake.jws",
		Approver:        "sjawhar",
		Enrollment:      Enrollment{Kind: "session", RuntimeID: "agentbox-1234", Operator: "sjawhar"},
		LifetimeSeconds: new(43200),
		RulesVersion:    "ab12",
		ExpiresAt:       time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
	}
	want := "agent-secrets-record/v1\n" +
		"request: eyJ.fake.jws\n" +
		"approver: sjawhar\n" +
		"enrollment: session\tagentbox-1234\tsjawhar\n" +
		"lifetime_seconds: 43200\n" +
		"rules_version: ab12\n" +
		"expires_at: 2026-09-27T12:00:00Z\n" +
		"code: -\n"
	if got := b.Canonical(); got != want {
		t.Fatalf("canonical body:\n%q\nwant:\n%q", got, want)
	}
	sum := sha256.Sum256([]byte(want))
	if b.ID() != hex.EncodeToString(sum[:]) {
		t.Fatalf("id %s is not the body's sha256", b.ID())
	}
	back, err := ParseBody(want)
	if err != nil || back.Canonical() != b.Canonical() {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestVerifyRequestObjectAcceptsItsOwnSignAndRefusesTheProofTyp(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{
		{Type: "agent_secret", Identifier: "DEEL_API_KEY", Actions: []string{"inject"}},
	}, "deel sync for ACME-1", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	tp, _ := proof.Thumbprint(&key.PublicKey)
	if ro.Thumbprint != tp || ro.Details[0].Identifier != "DEEL_API_KEY" || ro.LoginHint != "" {
		t.Fatalf("claims: %+v", ro)
	}
	// A per-call proof must never pass as a request object.
	p, _ := proof.Sign(key, "enr-1", "POST", "https://secrets.test/v1/requests", now)
	if _, err := VerifyRequestObject(p, "https://secrets.test", time.Minute, now); err == nil {
		t.Fatal("a proof-typ JWS passed as a request object")
	}
}

func TestVerifyRequestObjectRefusals(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	for name, tc := range map[string]struct {
		details []AuthorizationDetail
		reason  string
		hint    string
		aud     string
	}{
		"wrong audience":        {details: secretDetail(), aud: "https://evil.test"},
		"bidi in reason":        {details: secretDetail(), reason: "ok\u202Ekcatta", aud: "https://secrets.test"},
		"zero-width in reason":  {details: secretDetail(), reason: "a\u200bb", aud: "https://secrets.test"},
		"401-rune reason":       {details: secretDetail(), reason: strings.Repeat("r", 401), aud: "https://secrets.test"},
		"mixed types":           {details: append(secretDetail(), AuthorizationDetail{Type: "launcher_credential", Identifier: "h.example"}), aud: "https://secrets.test"},
		"bad hostname":          {details: []AuthorizationDetail{{Type: "launcher_credential", Identifier: "UPPER_case!"}}, aud: "https://secrets.test"},
		"empty details":         {details: nil, aud: "https://secrets.test"},
		"login_hint on session": {details: secretDetail(), hint: "sjawhar", aud: "https://secrets.test"},
	} {
		t.Run(name, func(t *testing.T) {
			compact, err := Sign(key, tc.aud, tc.details, tc.reason, tc.hint, nil, now)
			if err != nil {
				t.Fatal(err)
			}
			ro, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
			if name == "login_hint on session" {
				// Verify returns the claims; the login_hint-forbidden rule is per caller
				// (session route refuses it, machine route requires it), so here it parses.
				if err != nil || ro.LoginHint != "sjawhar" {
					t.Fatalf("login_hint should parse: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

// TestVerifyRequestObjectTellsAnotherAudienceFromEveryOtherRefusal pins the one refusal a caller
// reads apart from the rest: an object signed for another audience, which is a request signed for
// another broker address, is ErrRequestInvalid and ErrAudienceMismatch, and its text is still the
// one the approver's left-out line reports. A bad signature and an iat outside the skew are
// ErrRequestInvalid and never ErrAudienceMismatch, so requests.Machine.Create, which ends a
// pending request on that error alone, ends no request for either of them. The bad signature is
// signed for another audience too: the signature is checked first, so an object nobody signed is
// never read as one signed for another broker.
func TestVerifyRequestObjectTellsAnotherAudienceFromEveryOtherRefusal(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	sign := func(audience, reason string, at time.Time) string {
		t.Helper()
		compact, err := Sign(key, audience, secretDetail(), reason, "", nil, at)
		if err != nil {
			t.Fatal(err)
		}
		return compact
	}

	_, err := VerifyRequestObject(sign("https://another-broker.internal.example", "moved", now), "https://secrets.test", time.Minute, now)
	if !errors.Is(err, ErrRequestInvalid) || !errors.Is(err, ErrAudienceMismatch) || err.Error() != "request object invalid: aud does not match" {
		t.Fatalf("an object signed for another audience = %v; want ErrRequestInvalid and ErrAudienceMismatch, reading %q", err, "request object invalid: aud does not match")
	}

	// Another object's signature over this one's header and payload: one key signed both, so the
	// header is the same and only the signature fails.
	signed := strings.Split(sign("https://another-broker.internal.example", "first", now), ".")
	other := strings.Split(sign("https://another-broker.internal.example", "second", now), ".")
	forged := signed[0] + "." + signed[1] + "." + other[2]
	for _, c := range []struct{ name, compact, reason string }{
		{"a bad signature", forged, "request object invalid: signature"},
		{"an iat outside the skew", sign("https://secrets.test", "late", now.Add(-time.Hour)), "request object invalid: iat outside 1m0s"},
	} {
		_, err := VerifyRequestObject(c.compact, "https://secrets.test", time.Minute, now)
		if !errors.Is(err, ErrRequestInvalid) || errors.Is(err, ErrAudienceMismatch) || err.Error() != c.reason {
			t.Errorf("%s = %v; want ErrRequestInvalid reading %q and not ErrAudienceMismatch", c.name, err, c.reason)
		}
	}
}

func TestParseBodyRefusesAnyEditedLine(t *testing.T) {
	valid := "agent-secrets-record/v1\n" +
		"request: eyJ.fake.jws\n" +
		"approver: sjawhar\n" +
		"enrollment: session\tagentbox-1234\tsjawhar\n" +
		"lifetime_seconds: 43200\n" +
		"rules_version: ab12\n" +
		"expires_at: 2026-09-27T12:00:00Z\n" +
		"code: -\n"
	if _, err := ParseBody(valid); err != nil {
		t.Fatalf("valid body should parse: %v", err)
	}
	lines := strings.Split(valid, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		t.Run(line, func(t *testing.T) {
			mutated := make([]string, len(lines))
			copy(mutated, lines)
			b := []byte(line)
			b[0] ^= 0x01 // flip a bit of the line's first byte
			mutated[i] = string(b)
			if _, err := ParseBody(strings.Join(mutated, "\n")); err == nil {
				t.Fatalf("edited line %d (%q) parsed without error", i, line)
			}
		})
	}
}

// targetDetail is one agent_secret detail for GITHUB_READ whose locations are locations.
func targetDetail(locations ...string) AuthorizationDetail {
	return AuthorizationDetail{Type: KindAgentSecret, Identifier: "GITHUB_READ", Actions: []string{"inject"}, Locations: locations}
}

// TestARequestRefusesEachTargetItCannotHold: a request object's locations are its target, one
// GitHub account or repositories of one account, on its one agent_secret detail
// (validateDetails). Each row breaks one rule, and the refusal names that rule, so a row cannot
// pass on another row's refusal.
func TestARequestRefusesEachTargetItCannotHold(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = "https://github.com/example-org/repository-" + strconv.Itoa(i)
	}
	for _, row := range []struct {
		name    string
		details []AuthorizationDetail
		says    string
	}{
		{"on a launcher_credential detail", []AuthorizationDetail{{Type: KindLauncherCredential, Identifier: "box.example", Locations: []string{"https://github.com/example-org"}}}, "launcher_credential"},
		{"on a request with more than one detail", []AuthorizationDetail{targetDetail("https://github.com/example-org"), {Type: KindAgentSecret, Identifier: "DEEL_API_KEY", Actions: []string{"inject"}}}, "more than one"},
		{"past 100 entries", []AuthorizationDetail{targetDetail(tooMany...)}, "100"},
		{"naming two accounts", []AuthorizationDetail{targetDetail("https://github.com/example-org/proofs", "https://github.com/acme/proofs")}, "two accounts"},
		{"mixing the account form with repository forms", []AuthorizationDetail{targetDetail("https://github.com/example-org", "https://github.com/example-org/proofs")}, "mix the account form"},
		{"the account form twice", []AuthorizationDetail{targetDetail("https://github.com/example-org", "https://github.com/example-org")}, "mix the account form"},
	} {
		t.Run(row.name, func(t *testing.T) {
			compact, err := Sign(key, "https://secrets.test", row.details, "read", "", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
			if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), row.says) {
				t.Fatalf("VerifyRequestObject = %v; want ErrRequestInvalid saying %q", err, row.says)
			}
		})
	}
}

// TestARequestRefusesALocationThatIsNoGitHubTarget: an entry of locations is
// https://github.com/<account> or https://github.com/<account>/<repository>, the account a GitHub
// login in lowercase and the repository a name GitHub can hold, and nothing else: no other scheme
// or host, no deeper path, no query or fragment, no empty name and no rune a name cannot hold, a
// bidi override among them, which the approval page would otherwise print. Anything else is
// refused rather than read as a target it does not spell.
func TestARequestRefusesALocationThatIsNoGitHubTarget(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	for _, location := range []string{
		"",
		"example-org",
		"http://github.com/example-org",
		"https://example.com/example-org",
		"https://github.com",
		"https://github.com/",
		"https://github.com//proofs",
		"https://github.com/example-org/",
		"https://github.com/Example-Org",
		"https://github.com/example-org/proofs/tree",
		"https://github.com/example-org?tab=repositories",
		"https://github.com/example-org/proofs#readme",
		"https://user@github.com/example-org",
		"https://github.com:443/example-org",
		"https://github.com/-example-org",
		"https://github.com/" + strings.Repeat("a", 40),
		"https://github.com/example-org/..",
		"https://github.com/example-org/pro ofs",
		"https://github.com/example-org/proofs\u202e",
		"https://github.com/example-org/" + strings.Repeat("r", 101),
	} {
		t.Run(location, func(t *testing.T) {
			compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{targetDetail(location)}, "read", "", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
			if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "is no GitHub target") {
				t.Fatalf("VerifyRequestObject(%q) = %v; want ErrRequestInvalid saying it is no GitHub target", location, err)
			}
		})
	}
}

// TestATargetIsReadBackFromItsLocations: a verified request keeps the locations it signed, and
// Target reads them as the one account and its repositories, sorted and each once, or as the zero
// Target for a detail that signs none. Target.Locations builds the signed locations back, sorted
// and each once, which is what a release answers; String names the target as a refusal and the
// approval page do, its account or each repository as account/name; and Equal holds a target
// equal to the same account and repositories, and not to another account's.
func TestATargetIsReadBackFromItsLocations(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	for _, row := range []struct {
		name         string
		locations    []string
		account      string
		repositories []string
		rebuilt      []string
		words        string
	}{
		{"no target", nil, "", nil, nil, ""},
		{"an account", []string{"https://github.com/example-org"}, "example-org", nil, []string{"https://github.com/example-org"}, "example-org"},
		{"repositories of one account", []string{"https://github.com/example-org/proofs", "https://github.com/example-org/lean", "https://github.com/example-org/proofs"}, "example-org", []string{"lean", "proofs"}, []string{"https://github.com/example-org/lean", "https://github.com/example-org/proofs"}, "example-org/lean, example-org/proofs"},
	} {
		t.Run(row.name, func(t *testing.T) {
			compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{targetDetail(row.locations...)}, "read", "", nil, now)
			if err != nil {
				t.Fatal(err)
			}
			obj, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
			if err != nil {
				t.Fatalf("VerifyRequestObject: %v", err)
			}
			if !slices.Equal(obj.Details[0].Locations, row.locations) {
				t.Fatalf("the verified detail's locations = %q, want the signed %q", obj.Details[0].Locations, row.locations)
			}
			target := obj.Details[0].Target()
			if target.Account != row.account || !slices.Equal(target.Repositories, row.repositories) {
				t.Fatalf("Target() = %+v; want %q, %q", target, row.account, row.repositories)
			}
			if rebuilt := target.Locations(); !slices.Equal(rebuilt, row.rebuilt) {
				t.Fatalf("Target().Locations() = %q, want %q", rebuilt, row.rebuilt)
			}
			if words := target.String(); words != row.words {
				t.Fatalf("Target().String() = %q, want %q", words, row.words)
			}
			same := Target{Account: row.account, Repositories: slices.Clone(row.repositories)}
			for _, other := range []Target{
				{Account: "acme", Repositories: row.repositories},
				{Account: row.account, Repositories: append(slices.Clone(row.repositories), "site")},
			} {
				if !target.Equal(same) || target.Equal(other) {
					t.Fatalf("Target().Equal: %+v equals %+v %t and %+v %t; want true and false", target, same, target.Equal(same), other, target.Equal(other))
				}
			}
		})
	}
}

func TestSignExpiresTenMinutesAfterIat(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", secretDetail(), "reason", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ro.Expires.Unix(), ro.IssuedAt.Unix()+600; got != want {
		t.Fatalf("exp = %d, want iat+600 = %d", got, want)
	}
}

func TestMachineBodyUsesDashEnrollmentAndCarriesCode(t *testing.T) {
	b := Body{
		Request:         "eyJ.fake.jws",
		Approver:        "sjawhar",
		Enrollment:      Enrollment{Kind: "-", RuntimeID: "-", Operator: ""},
		LifetimeSeconds: new(604800),
		RulesVersion:    "ab12",
		ExpiresAt:       time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
		Code:            "ABCD-EFGH",
	}
	got := b.Canonical()
	if !strings.Contains(got, "enrollment: -\t-\t-\n") {
		t.Fatalf("canonical body missing dash enrollment:\n%q", got)
	}
	if !strings.Contains(got, "code: ABCD-EFGH\n") {
		t.Fatalf("canonical body missing code:\n%q", got)
	}
	back, err := ParseBody(got)
	if err != nil || back.Canonical() != b.Canonical() {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// podBody is a pod enrollment's record body with no slot: the three-field form every slotless
// record is stored and addressed under.
func podBody(request string) Body {
	return Body{
		Request:         request,
		Approver:        "sjawhar",
		Enrollment:      Enrollment{Kind: "pod", RuntimeID: "0b6c2f6e-pod-uid"},
		LifetimeSeconds: new(3600),
		RulesVersion:    "ab12",
		ExpiresAt:       time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
	}
}

// TestASlotIsAFourthEnrollmentFieldAndASlotlessBodyKeepsItsBytes pins the slot's place in the
// signed record: a body with no slot renders the exact three-field enrollment line and parses back
// to the same bytes, and a pod's slot is a fourth field that round-trips and gives the record an id
// of its own, distinct for every slot of one pod.
func TestASlotIsAFourthEnrollmentFieldAndASlotlessBodyKeepsItsBytes(t *testing.T) {
	legacyBody := podBody("eyJ.fake.jws")
	old := legacyBody.Canonical()
	if want := "agent-secrets-record/v1\n" +
		"request: eyJ.fake.jws\n" +
		"approver: sjawhar\n" +
		"enrollment: pod\t0b6c2f6e-pod-uid\t-\n" +
		"lifetime_seconds: 3600\n" +
		"rules_version: ab12\n" +
		"expires_at: 2026-09-27T12:00:00Z\n" +
		"code: -\n"; old != want {
		t.Fatalf("slotless pod body:\n%q\nwant:\n%q", old, want)
	}
	parsed, err := ParseBody(old)
	if err != nil || parsed.Canonical() != legacyBody.Canonical() {
		t.Fatalf("legacy signed record changed: %+v %v", parsed, err)
	}

	implementer := legacyBody
	implementer.Enrollment.Slot = "implementer-g3"
	got := implementer.Canonical()
	if want := strings.Replace(old, "enrollment: pod\t0b6c2f6e-pod-uid\t-\n", "enrollment: pod\t0b6c2f6e-pod-uid\t-\timplementer-g3\n", 1); got != want {
		t.Fatalf("slotted pod body:\n%q\nwant:\n%q", got, want)
	}
	back, err := ParseBody(got)
	if err != nil || back.Canonical() != implementer.Canonical() || back.Canonical() != got {
		t.Fatalf("slotted round trip: %+v %v", back, err)
	}
	reviewer := implementer
	reviewer.Enrollment.Slot = "reviewer-g3"
	ids := map[string]string{legacyBody.ID(): "no slot", implementer.ID(): "implementer-g3", reviewer.ID(): "reviewer-g3"}
	if len(ids) != 3 {
		t.Fatalf("record ids %v, want one per slot of the pod and one for no slot", ids)
	}
}

// TestParseBodyRefusesAnEnrollmentFourthFieldThatIsNotAPodSlot pins ParseBody's slot rule: the
// enrollment line's fourth field is a pod's nonempty slot matching ValidSlot, and nothing else.
func TestParseBodyRefusesAnEnrollmentFourthFieldThatIsNotAPodSlot(t *testing.T) {
	withEnrollment := func(enrollment string) string {
		return strings.Replace(podBody("eyJ.fake.jws").Canonical(), "enrollment: pod\t0b6c2f6e-pod-uid\t-\n", "enrollment: "+enrollment+"\n", 1)
	}
	longest := "a" + strings.Repeat("b", 62)
	if _, err := ParseBody(withEnrollment("pod\tuid-1\t-\t" + longest)); err != nil {
		t.Fatalf("a 63-character slot must parse: %v", err)
	}
	for name, enrollment := range map[string]string{
		"empty fourth field":      "pod\tuid-1\t-\t",
		"uppercase slot":          "pod\tuid-1\t-\tImplementer-g1",
		"leading digit":           "pod\tuid-1\t-\t1mplementer",
		"leading hyphen":          "pod\tuid-1\t-\t-implementer",
		"64-character slot":       "pod\tuid-1\t-\t" + longest + "c",
		"slot on a box":           "box\tbox-1\tsjawhar\timplementer-g1",
		"slot on a host":          "host\thost-1\tsjawhar\timplementer-g1",
		"slot on a machine login": "-\t-\t-\timplementer-g1",
		"five fields":             "pod\tuid-1\t-\timplementer-g1\textra",
	} {
		t.Run(name, func(t *testing.T) {
			if b, err := ParseBody(withEnrollment(enrollment)); err == nil {
				t.Fatalf("enrollment %q parsed as %+v", enrollment, b)
			}
		})
	}
}

// TestChainVerifierHoldsASlotlessAndASlottedRecordToTheirOwnBytes runs ChainVerifier over a pod
// record with no slot and one with a slot, each signed by the requester and approved by its
// approver: both verify, and a stored body that lost its slot no longer reproduces the slotted
// record's id.
func TestChainVerifierHoldsASlotlessAndASlottedRecordToTheirOwnBytes(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", secretDetail(), "need it", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyBody := podBody(compact)
	slotted := legacyBody
	slotted.Enrollment.Slot = "implementer-g3"
	stored := map[string]string{legacyBody.ID(): legacyBody.Canonical(), slotted.ID(): slotted.Canonical()}
	verifier := &ChainVerifier{
		Audience: "https://secrets.test", Skew: time.Minute,
		FetchRecord: func(_ context.Context, id string) (string, time.Time, bool, error) {
			body, ok := stored[id]
			return body, now, ok, nil
		},
		FetchDecisions: func(context.Context, string) ([]TerminalEvent, error) {
			return []TerminalEvent{{Event: "approved", Login: "sjawhar"}}, nil
		},
	}
	for _, b := range []Body{legacyBody, slotted} {
		got, err := verifier.Verify(context.Background(), b.ID())
		if err != nil || got.Canonical() != b.Canonical() {
			t.Fatalf("Verify(%q slot) = %+v, %v; want the stored body back", b.Enrollment.Slot, got, err)
		}
	}
	stored[slotted.ID()] = legacyBody.Canonical()
	if _, err := verifier.Verify(context.Background(), slotted.ID()); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("Verify(slotted id, slotless body) = %v, want ErrChainBroken", err)
	}
}

// TestAnyoneApproverAdmitsEveryLoginButItself pins the approver AnyoneApprover against a person's:
// an agent_secret record naming anyone is decided by any login, never by the sentinel itself or an
// empty login, and its chain verifies over whichever login approved it; a record naming a person
// is decided by that person alone, the sentinel included; and a record of any other kind naming
// anyone (a person's machine login, which an older binary could open, or a kind not spelled
// exactly agent_secret) is decided by no login, and a person's machine login's chain verifies over
// none. Its request object asks for a secret, so it names no service.
func TestAnyoneApproverAdmitsEveryLoginButItself(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", secretDetail(), "shared", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	shared := podBody(compact)
	shared.Approver = AnyoneApprover
	owned := podBody(compact)
	owned.Approver = "sami@example.com"
	for _, c := range []struct {
		kind  string
		body  Body
		login string
		want  string
	}{
		{KindAgentSecret, shared, " Bob@Example.com ", "bob@example.com"},
		{KindAgentSecret, shared, "sami@example.com", "sami@example.com"},
		{KindAgentSecret, shared, AnyoneApprover, ""},
		{KindAgentSecret, shared, " ANYONE ", ""},
		{KindAgentSecret, shared, "  ", ""},
		{KindAgentSecret, owned, "Sami@Example.com", "sami@example.com"},
		{KindAgentSecret, owned, "bob@example.com", ""},
		{KindAgentSecret, owned, AnyoneApprover, ""},
		{KindLauncherCredential, shared, "bob@example.com", ""},
		{KindLauncherCredential, shared, AnyoneApprover, ""},
		{"", shared, "bob@example.com", ""},
		{"Agent_Secret", shared, "bob@example.com", ""},
		{KindLauncherCredential, owned, "Sami@Example.com", "sami@example.com"},
	} {
		got, err := c.body.ApproverLogin(c.kind, "", c.login)
		if c.want == "" && !errors.Is(err, ErrNotApprover) || c.want != "" && (err != nil || got != c.want) {
			t.Errorf("ApproverLogin(%s, approver %q, login %q) = %q, %v; want %q", c.kind, c.body.Approver, c.login, got, err, c.want)
		}
	}
	for _, c := range []struct {
		kind, decider string
		verifies      bool
	}{
		{KindAgentSecret, "bob@example.com", true},
		{KindAgentSecret, AnyoneApprover, false},
		{KindLauncherCredential, "bob@example.com", false},
	} {
		verifier := &ChainVerifier{
			Kind: c.kind, Audience: "https://secrets.test", Skew: time.Minute,
			FetchRecord: func(context.Context, string) (string, time.Time, bool, error) {
				return shared.Canonical(), now, true, nil
			},
			FetchDecisions: func(context.Context, string) ([]TerminalEvent, error) {
				return []TerminalEvent{{Event: "approved", Login: c.decider}}, nil
			},
		}
		if _, err := verifier.Verify(context.Background(), shared.ID()); (err == nil) != c.verifies {
			t.Errorf("Verify(%s record naming anyone, approved by %q) = %v, want verified %v", c.kind, c.decider, err, c.verifies)
		}
	}
}

// machineLoginBody is a machine login's record body naming approver, its request object signed
// for one launcher_credential detail on host example-host-cluster naming service ("" for a
// person's machine) and carrying loginHint.
func machineLoginBody(t *testing.T, approver, service, loginHint string, now time.Time) Body {
	t.Helper()
	key, _ := proof.NewKey()
	compact, err := Sign(key, "https://secrets.test", []AuthorizationDetail{
		{Type: KindLauncherCredential, Identifier: "example-host-cluster", Service: service},
	}, "", loginHint, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return Body{
		Request: compact, Approver: approver, Enrollment: Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(3600), RulesVersion: "ab12", ExpiresAt: now.Add(15 * time.Minute).UTC().Truncate(time.Second),
		Code: "ABCD-EFGH",
	}
}

// TestAServiceLoginIsDecidedByAnyoneAndAPersonsByThemAlone pins the machine-login half of the
// approver rule, which keys on the service the signed request names, never on the approver stored
// beside it: a service's login is decided by any login but the sentinel and an empty one, whether
// its record names anyone (every one a broker before token logins opened), the person an older daemon put in its
// login_hint, or a person a broker from before this rule stored, and its chain verifies over
// whichever login approved it; a person's machine login is decided by that person alone, and one
// naming anyone, which an older binary could open, by no one, its chain verifying over none.
func TestAServiceLoginIsDecidedByAnyoneAndAPersonsByThemAlone(t *testing.T) {
	now := time.Now()
	service := machineLoginBody(t, AnyoneApprover, "legion-daemon", "", now)
	hinted := machineLoginBody(t, AnyoneApprover, "legion-daemon", "sami@example.com", now)
	stored := machineLoginBody(t, "sami@example.com", "legion-daemon", "sami@example.com", now)
	personal := machineLoginBody(t, "sami@example.com", "", "sami@example.com", now)
	personalAnyone := machineLoginBody(t, AnyoneApprover, "", AnyoneApprover, now)
	for _, c := range []struct {
		name  string
		body  Body
		login string
		want  string
	}{
		{"a service's login, by a person it names nowhere", service, " Bob@Example.com ", "bob@example.com"},
		{"a service's login, by the sentinel", service, " ANYONE ", ""},
		{"a service's login, by no one", service, "  ", ""},
		{"a service's login an older daemon named a person in, by another", hinted, "bob@example.com", "bob@example.com"},
		{"a service's login a broker before this rule stored a person on, by another", stored, "bob@example.com", "bob@example.com"},
		{"a person's machine login, by that person", personal, "Sami@Example.com", "sami@example.com"},
		{"a person's machine login, by another", personal, "bob@example.com", ""},
		{"a person's machine login naming anyone, by a person", personalAnyone, "bob@example.com", ""},
		{"a person's machine login naming anyone, by the sentinel", personalAnyone, AnyoneApprover, ""},
	} {
		obj, err := VerifyRequestObject(c.body.Request, "https://secrets.test", time.Minute, now)
		if err != nil {
			t.Fatalf("%s: VerifyRequestObject: %v", c.name, err)
		}
		got, err := c.body.ApproverLogin(KindLauncherCredential, obj.Service(), c.login)
		if c.want == "" && !errors.Is(err, ErrNotApprover) || c.want != "" && (err != nil || got != c.want) {
			t.Errorf("%s: ApproverLogin(login %q) = %q, %v; want %q", c.name, c.login, got, err, c.want)
		}
		verifier := &ChainVerifier{
			Kind: KindLauncherCredential, Audience: "https://secrets.test", Skew: time.Minute,
			FetchRecord: func(context.Context, string) (string, time.Time, bool, error) {
				return c.body.Canonical(), now, true, nil
			},
			FetchDecisions: func(context.Context, string) ([]TerminalEvent, error) {
				return []TerminalEvent{{Event: "approved", Login: CanonicalLogin(c.login)}}, nil
			},
		}
		if _, err := verifier.Verify(context.Background(), c.body.ID()); (err == nil) != (c.want != "") {
			t.Errorf("%s: Verify(approved by %q) = %v, want verified %v", c.name, c.login, err, c.want != "")
		}
	}
}

func TestForgedSignatureFailsForRequestObject(t *testing.T) {
	attackerKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	victimKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	victimThumbprint, err := proof.Thumbprint(&victimKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: attackerKey}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: RequestTyp},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload, err := json.Marshal(requestClaims{
		Issuer:               victimThumbprint,
		Audience:             "https://secrets.test",
		JTI:                  "forged-jti",
		IssuedAt:             now.Unix(),
		Expires:              now.Unix() + 600,
		AuthorizationDetails: secretDetail(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}

	// Swap the embedded JWK in the protected header for the victim's public key, so the request
	// object claims to be signed by the victim while the bytes were actually signed by the
	// attacker. Re-verifying against the swapped protected header must fail: the signing input
	// no longer matches what the attacker actually signed.
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("expected compact JWS with 3 parts, got %d", len(parts))
	}
	rawProtected, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var protected map[string]any
	if err := json.Unmarshal(rawProtected, &protected); err != nil {
		t.Fatal(err)
	}
	victimJWK, err := (jose.JSONWebKey{Key: &victimKey.PublicKey}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var jwkMap map[string]any
	if err := json.Unmarshal(victimJWK, &jwkMap); err != nil {
		t.Fatal(err)
	}
	protected["jwk"] = jwkMap
	forgedProtected, err := json.Marshal(protected)
	if err != nil {
		t.Fatal(err)
	}
	forged := base64.RawURLEncoding.EncodeToString(forgedProtected) + "." + parts[1] + "." + parts[2]

	_, err = VerifyRequestObject(forged, "https://secrets.test", time.Minute, now)
	if err == nil || !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("forged signature (embedded key not the actual signer) must be rejected with ErrRequestInvalid mentioning signature, got %v", err)
	}
}

func TestUnsignedTypHeaderFailsForRequestObject(t *testing.T) {
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{EmbedJWK: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(requestClaims{
		Issuer:               thumbprint,
		Audience:             "https://secrets.test",
		JTI:                  "no-typ-jti",
		IssuedAt:             now.Unix(),
		Expires:              now.Unix() + 600,
		AuthorizationDetails: secretDetail(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("expected compact JWS with 3 parts, got %d", len(parts))
	}
	// The protected header this JWS was actually signed over carries no "typ" claim at all. A
	// JSON-serialized JWS lets an attacker attach one through the unsigned "header" field
	// instead; the typ check must only trust the protected (signed) header.
	jsonSerialized, err := json.Marshal(map[string]any{
		"payload":   parts[1],
		"protected": parts[0],
		"header":    map[string]string{"typ": RequestTyp},
		"signature": parts[2],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRequestObject(string(jsonSerialized), "https://secrets.test", time.Minute, now); err == nil || !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("typ present only in the unsigned header must be rejected with ErrRequestInvalid, got %v", err)
	}
}

// TestARequestNamesItsGrantsLifetimeOrNone pins the lifetime_seconds claim: a request for agent
// secrets may sign one, which verifies back as the grant's lifetime; one that names none verifies
// as 0, a grant that lasts until its session ends; and a lifetime outside 1..MaxInt32, or on a
// machine login, is refused.
func TestARequestNamesItsGrantsLifetimeOrNone(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	verify := func(compact string) (RequestObject, error) {
		t.Helper()
		return VerifyRequestObject(compact, "https://secrets.test", time.Minute, now)
	}
	lifetime := 120
	withLifetime, err := Sign(key, "https://secrets.test", secretDetail(), "two minutes", "", &lifetime, now)
	if err != nil {
		t.Fatal(err)
	}
	if ro, err := verify(withLifetime); err != nil || ro.LifetimeSeconds == nil || *ro.LifetimeSeconds != lifetime {
		t.Fatalf("a request naming 120 seconds = %+v, %v; want lifetime 120", ro, err)
	}
	without, err := Sign(key, "https://secrets.test", secretDetail(), "until the session ends", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if ro, err := verify(without); err != nil || ro.LifetimeSeconds != nil {
		t.Fatalf("a request naming no lifetime = %+v, %v; want no lifetime", ro, err)
	}
	for _, seconds := range []int{0, -1, math.MaxInt32 + 1} {
		compact, err := Sign(key, "https://secrets.test", secretDetail(), "", "", &seconds, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verify(compact); !errors.Is(err, ErrRequestInvalid) {
			t.Fatalf("lifetime %d = %v, want ErrRequestInvalid", seconds, err)
		}
	}
	loginLifetime := 120
	login, err := Sign(key, "https://secrets.test", []AuthorizationDetail{{Type: KindLauncherCredential, Identifier: "h.example"}}, "", "", &loginLifetime, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify(login); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("a machine login naming a lifetime = %v, want ErrRequestInvalid", err)
	}
}

// TestABodyWithNoLifetimeRendersADash pins the record line for a grant that lasts until its session
// ends: lifetime_seconds is "-", the body round-trips, and a stored 0 or negative count is refused
// rather than read as no lifetime, so no other spelling addresses the same record.
func TestABodyWithNoLifetimeRendersADash(t *testing.T) {
	b := Body{
		Request:      "eyJ.fake.jws",
		Approver:     "sjawhar",
		Enrollment:   Enrollment{Kind: "session", RuntimeID: "example-host-devbox:1:2", Operator: "sjawhar"},
		RulesVersion: "ab12",
		ExpiresAt:    time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
	}
	got := b.Canonical()
	if !strings.Contains(got, "\nlifetime_seconds: -\n") {
		t.Fatalf("canonical body of a grant with no lifetime:\n%q\nwant the line lifetime_seconds: -", got)
	}
	back, err := ParseBody(got)
	if err != nil || back.Canonical() != b.Canonical() {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	for _, spelled := range []string{"0", "-5", ""} {
		if _, err := ParseBody(strings.Replace(got, "lifetime_seconds: -", "lifetime_seconds: "+spelled, 1)); err == nil {
			t.Fatalf("lifetime_seconds: %q parsed, want it refused", spelled)
		}
	}
}

// TestJudgeLoginReadsBackWhatJudgeActorWrote pins the judge's audit actor against every other
// actor the broker writes: the model id goes in and comes back out, and a human actor, a launcher
// actor, a bare login, the prefix with no model and a differently-cased prefix are each no judge,
// so nothing but JudgeActor's own output reads as one.
func TestJudgeLoginReadsBackWhatJudgeActorWrote(t *testing.T) {
	for _, modelID := range []string{"claude-opus-5", "anthropic/claude-opus-5", "sami@example.com"} {
		if model, ok := JudgeLogin(JudgeActor(modelID)); !ok || model != modelID {
			t.Errorf("JudgeLogin(JudgeActor(%q)) = %q, %v; want %q, true", modelID, model, ok, modelID)
		}
	}
	for _, actor := range []string{
		"",
		"judge:",
		"Judge:claude-opus-5",
		" judge:claude-opus-5",
		"judged:claude-opus-5",
		HumanActor("sami@example.com"),
		LauncherActor("0b6c2f6e-cred"),
		AnyoneApprover,
	} {
		if model, ok := JudgeLogin(actor); ok {
			t.Errorf("JudgeLogin(%q) = %q, true; want it read as no judge", actor, model)
		}
	}
}

// TestNoJudgeActorDecidesARecordAsAPerson pins the judge out of the approver rule: a login
// spelling a judge actor decides nothing, whatever the record's approver and kind — the sentinel
// anyone, the judge actor itself as an owner tag, and a service's machine login, which admits
// every other login there is. A person is still admitted in each of those places, so the refusal
// takes the judge out and nobody else.
func TestNoJudgeActorDecidesARecordAsAPerson(t *testing.T) {
	judge := JudgeActor("claude-opus-5")
	for _, c := range []struct{ kind, approver, service string }{
		{KindAgentSecret, AnyoneApprover, ""},
		{KindAgentSecret, judge, ""},
		{KindAgentSecret, "sami@example.com", ""},
		{KindLauncherCredential, AnyoneApprover, "legion-daemon"},
		{KindLauncherCredential, judge, "legion-daemon"},
	} {
		if MayDecide(c.kind, c.approver, c.service, judge) {
			t.Errorf("MayDecide(%s, approver %q, service %q, login %q) admitted the judge as a person", c.kind, c.approver, c.service, judge)
		}
		if !MayDecide(c.kind, c.approver, c.service, "sami@example.com") && c.approver != judge {
			t.Errorf("MayDecide(%s, approver %q, service %q) refused a person too", c.kind, c.approver, c.service)
		}
	}
}

// TestMayJudgeAdmitsBoundedJudgeKeys pins the conditions a record must meet before the judge's
// grant of it is admitted, and which one each refusal names: agent secrets alone, each key the
// judge decided a key the rules give the judge seconds of, and a lifetime within every one of
// those seconds (a record with none is bounded by nothing and is refused, one asking for exactly
// the bound is admitted). A key the request needed no decision for — one its session gets at once
// — is no key the judge decided, and no bound of the judge's holds it; a record whose keys needed
// no decision at all is no record the judge decided. A nil decided is every key decided. The
// reason is the judge's to read and no condition here: a record whose reason names no issue is
// admitted.
func TestMayJudgeAdmitsBoundedJudgeKeys(t *testing.T) {
	judged := func(secret string) (uint32, bool) {
		switch secret {
		case "DEEL_API_KEY":
			return 900, true
		case "SHORT_KEY":
			return 300, true
		}
		return 0, false
	}
	object := func(details []AuthorizationDetail, reason string) RequestObject {
		return RequestObject{Details: details, Reason: reason}
	}
	secret := func(name string) AuthorizationDetail {
		return AuthorizationDetail{Type: KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	reason := "post the release notes"
	body := func(seconds *int) Body {
		b := podBody("eyJ.fake.jws")
		b.LifetimeSeconds = seconds
		return b
	}
	// onlyDeel is a request whose one key that needed a decision is DEEL_API_KEY: the rest are
	// keys its session gets at once.
	onlyDeel := func(secret string) bool { return secret == "DEEL_API_KEY" }
	none := func(string) bool { return false }
	for _, c := range []struct {
		name         string
		body         Body
		obj          RequestObject
		judgeSeconds func(string) (uint32, bool)
		decided      func(string) bool
		want         string // the refusal names this, "" for a record the judge may decide
	}{
		{
			name: "a judge key beside a key its session gets at once",
			body: body(new(900)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("OTHER_KEY")}, reason), judgeSeconds: judged, decided: onlyDeel,
		},
		{
			name: "a judge key beside a key its session gets at once, past the judge key's bound",
			body: body(new(901)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("OTHER_KEY")}, reason), judgeSeconds: judged, decided: onlyDeel,
			want: "past the 900",
		},
		{
			name: "keys that all needed no decision",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("OTHER_KEY")}, reason), judgeSeconds: judged, decided: none,
			want: "no key that needed a decision",
		},
		{
			name: "one secret, inside the bound, its reason naming no issue",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: judged,
		},
		{
			name: "a lifetime of exactly the bound",
			body: body(new(900)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: judged,
		},
		{
			name: "two judge keys inside both bounds",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("SHORT_KEY")}, reason), judgeSeconds: judged,
		},
		{
			name: "two judge keys past the shorter bound",
			body: body(new(600)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("SHORT_KEY")}, reason), judgeSeconds: judged,
			want: `past the 300`,
		},
		{
			name: "a judge key beside a key the rules give the judge nothing of",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("OTHER_KEY")}, reason), judgeSeconds: judged,
			want: `decide nothing for "OTHER_KEY"`,
		},
		{
			name: "a machine login, not an agent secret",
			body: body(new(300)), judgeSeconds: judged,
			obj:  object([]AuthorizationDetail{{Type: KindLauncherCredential, Identifier: "example-host-cluster"}}, reason),
			want: "not an agent secret",
		},
		{
			name: "no lifetime at all",
			body: body(nil), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: judged,
			want: "names no lifetime",
		},
		{
			name: "a second past the bound",
			body: body(new(901)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: judged,
			want: "past the 900",
		},
		{
			name: "a secret the rules give the judge no bound for",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("OTHER_KEY")}, reason), judgeSeconds: judged,
			want: `decide nothing for "OTHER_KEY"`,
		},
		{
			name: "no bound to ask at all",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: nil,
			want: "no judge bound is available",
		},
		{
			name: "no secret at all",
			body: body(new(300)), obj: object(nil, reason), judgeSeconds: judged,
			want: "names no authorization detail",
		},
	} {
		err := MayJudge(c.body, c.obj, c.judgeSeconds, c.decided)
		switch {
		case c.want == "":
			if err != nil {
				t.Errorf("%s: MayJudge = %v, want the judge admitted", c.name, err)
			}
		case !errors.Is(err, ErrNotJudgeable):
			t.Errorf("%s: MayJudge = %v, want ErrNotJudgeable", c.name, err)
		case !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: MayJudge = %v, want it to name %q", c.name, err, c.want)
		}
	}
}

// judgeModel is the model the test's judge decides as, in the audit actor its approved event
// carries.
const judgeModel = "claude-opus-5"

// TestChainVerifierAdmitsAJudgesApprovalOnlyInsideTheRulesBound pins the arm ChainVerifier.Verify
// takes for an approval the model judge made: the event's actor chooses it, and MayJudge, over
// the verifier's own JudgeSeconds, decides it. A judged approval of a bounded judge key verifies,
// whatever its reason names; one beside a key the rules give the judge nothing of, one past the
// entry's judge, and one against a verifier left no bound at all are each ErrChainBroken naming
// the model and the reason MayJudge gave. A judged event whose row also carries a login is still
// the judge's, so no login written beside it can buy a record MayJudge refuses. The person's arm
// is untouched: the approver's own approval verifies and another login's does not.
//
// Every judged case would be refused by MayDecide alone, an empty login deciding nothing, so
// each refusal is checked to name what MayJudge said rather than only to be a refusal.
func TestChainVerifierAdmitsAJudgesApprovalOnlyInsideTheRulesBound(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	judged := func(secret string) (uint32, bool) {
		if secret == "DEEL_API_KEY" {
			return 900, true
		}
		return 0, false
	}
	secret := func(name string) AuthorizationDetail {
		return AuthorizationDetail{Type: KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	// stored is the record a request for details with reason, asking for lifetime seconds, was
	// stored as: the request object really signed, so Verify's own re-verification holds.
	stored := func(details []AuthorizationDetail, reason string, lifetime int) Body {
		t.Helper()
		compact, err := Sign(key, "https://secrets.test", details, reason, "", &lifetime, now)
		if err != nil {
			t.Fatal(err)
		}
		body := podBody(compact)
		body.LifetimeSeconds = &lifetime
		return body
	}
	byJudge := TerminalEvent{Event: "approved", Actor: JudgeActor(judgeModel)}
	byPerson := func(login string) TerminalEvent {
		return TerminalEvent{Event: "approved", Login: login, Actor: HumanActor(login)}
	}
	one := []AuthorizationDetail{secret("DEEL_API_KEY")}
	two := []AuthorizationDetail{secret("DEEL_API_KEY"), secret("SLACK_MCP_XOXP_TOKEN")}
	issue := "unblock INFRA-1353"
	// onlyDeel answers that DEEL_API_KEY was the one key of the record's request that needed a
	// decision, as the broker's own read of its request rows answers it.
	onlyDeel := func(context.Context, string) (func(string) bool, error) {
		return func(secret string) bool { return secret == "DEEL_API_KEY" }, nil
	}
	for _, c := range []struct {
		name         string
		body         Body
		event        TerminalEvent
		judgeSeconds func(string) (uint32, bool)
		decided      func(context.Context, string) (func(string) bool, error)
		want         string // the refusal names this, "" for a chain that verifies
	}{
		{
			name: "the judge, on a judge key beside a key its session got at once",
			body: stored(two, issue, 300), event: byJudge, judgeSeconds: judged, decided: onlyDeel,
		},
		{
			name: "the judge, on one secret inside the bound, naming an issue",
			body: stored(one, issue, 300), event: byJudge, judgeSeconds: judged,
		},
		{
			name: "the judge, on a secret beside one the rules give it nothing of",
			body: stored(two, issue, 300), event: byJudge, judgeSeconds: judged,
			want: `decide nothing for "SLACK_MCP_XOXP_TOKEN"`,
		},
		{
			name: "the judge, past the entry's judge",
			body: stored(one, issue, 901), event: byJudge, judgeSeconds: judged,
			want: "past the 900",
		},
		{
			name: "the judge, on a reason naming no issue",
			body: stored(one, "deploying the broker", 300), event: byJudge, judgeSeconds: judged,
		},
		{
			name: "the judge, against a verifier holding no bound",
			body: stored(one, issue, 300), event: byJudge, judgeSeconds: nil,
			want: "no judge bound is available",
		},
		{
			name: "the judge, on a row that also names the approver as its login",
			body: stored(two, issue, 300), judgeSeconds: judged,
			event: TerminalEvent{Event: "approved", Login: "sjawhar", Actor: JudgeActor(judgeModel)},
			want:  `decide nothing for "SLACK_MCP_XOXP_TOKEN"`,
		},
		{
			name: "the record's own approver",
			body: stored(one, issue, 300), event: byPerson("sjawhar"), judgeSeconds: judged,
		},
		{
			name: "a person the record does not name",
			body: stored(one, issue, 300), event: byPerson("mallory"), judgeSeconds: judged,
			want: `approved by "mallory"`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			verifier := &ChainVerifier{
				Kind: KindAgentSecret, Audience: "https://secrets.test", Skew: time.Minute,
				JudgeSeconds: c.judgeSeconds, Decided: c.decided,
				FetchRecord: func(context.Context, string) (string, time.Time, bool, error) {
					return c.body.Canonical(), now, true, nil
				},
				FetchDecisions: func(context.Context, string) ([]TerminalEvent, error) {
					return []TerminalEvent{c.event}, nil
				},
			}
			got, err := verifier.Verify(context.Background(), c.body.ID())
			if c.want == "" {
				if err != nil || got.Canonical() != c.body.Canonical() {
					t.Fatalf("Verify = %+v, %v; want the stored body back", got, err)
				}
				return
			}
			if !errors.Is(err, ErrChainBroken) {
				t.Fatalf("Verify = %v, want ErrChainBroken", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Verify = %v, want it to name %q", err, c.want)
			}
			if _, byTheJudge := JudgeLogin(c.event.Actor); byTheJudge && !strings.Contains(err.Error(), judgeModel) {
				t.Fatalf("Verify = %v, want the refusal to name the model %q", err, judgeModel)
			}
		})
	}
}

// TestIssueKeyInReason: the proxy's log line names the issue the session said it was working on,
// and the judge reads the same reason for one, which is the first Dispatch key it holds.
func TestIssueKeyInReason(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{"", ""},
		{"deploying the broker", ""},
		{"INFRA-1352", "INFRA-1352"},
		{"work on INFRA-1352 today", "INFRA-1352"},
		{"INFRA-1352 then LEGION-587", "INFRA-1352"},
		{"blocked by LEGION-587, see INFRA-1352", "LEGION-587"},
		{"(INFRA-1352) the airlock", "INFRA-1352"},
		{"dispatch://INFRA-1352/ask/3", "INFRA-1352"},
		{"infra-1352 lower-cased is no key", ""},
		{"INFRA- is no key, nor is -1352", ""},
		{"an UPPERCASE word and 1352 apart", ""},
	} {
		if got := IssueKeyInReason(c.reason); got != c.want {
			t.Errorf("IssueKeyInReason(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

// TestSanitizeLeavesNoRefusedRune pins the filter that text the broker records with nobody to
// refuse it to goes through — a model judge's words, a session call's reason — over two bounds:
// every rune validateReason refuses is gone, and what is left is no longer than the bound.
func TestSanitizeLeavesNoRefusedRune(t *testing.T) {
	refused := "\u0000\u0001\u200b\u202e\u2028\u2029\ufeff\n\r\t"
	for _, c := range []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"a reason of refused runes alone", refused, MaxReasonRunes, ""},
		{"a reason around them", "a" + refused + "b", MaxReasonRunes, "ab"},
		{"a reason at its bound", strings.Repeat("é", 500), MaxReasonRunes, strings.Repeat("é", MaxReasonRunes)},
		{"a longer text at its bound", strings.Repeat("x", 50_000), 2000, strings.Repeat("x", 2000)},
		{"a string inside both bounds", "plain words", MaxReasonRunes, "plain words"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Sanitize(c.in, c.limit)
			if got != c.want {
				t.Errorf("Sanitize(%q, %d) = %q; want %q", c.in, c.limit, got, c.want)
			}
			for _, r := range got {
				if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp) {
					t.Errorf("Sanitize kept %q, a rune a reason may not carry", r)
				}
			}
		})
	}
}

// TestClipArgumentBacksUpOverOneCharacterAtMost: ClipArgument cuts an argument to 256 bytes,
// backing up over the continuation bytes of the one character the cut would split, and over no
// more than a character holds, so what it answers is always long enough for the rule at the cut
// to read its end as where a secret may have been cut through. A long run of stray continuation
// bytes does not pull the cut back past them.
func TestClipArgumentBacksUpOverOneCharacterAtMost(t *testing.T) {
	for _, c := range []struct {
		name, arg string
		want      int
	}{
		{"a two-byte character across the cut", strings.Repeat("a", 255) + "é", 255},
		{"a three-byte character across the cut", strings.Repeat("a", 254) + "€", 254},
		{"stray continuation bytes across the cut", strings.Repeat("a", 200) + strings.Repeat("\x80", 100), 253},
		{"an argument within the cut", "short", 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ClipArgument(c.arg)
			if len(got) != c.want || !strings.HasPrefix(c.arg, got) {
				t.Errorf("ClipArgument kept %d bytes, want the first %d", len(got), c.want)
			}
		})
	}
}

// TestAskRefInReason: a reason that cites a Dispatch ask, which the judge reads as data for the
// model. The reference is the one skill://dispatch writes, dispatch://KEY/ask/<uuid>, and
// anything else names no ask: a key with no ask path after it, and an id in any shape but a
// lower-case uuid.
func TestAskRefInReason(t *testing.T) {
	const first = "4f3c2a10-5b6d-4e7f-8a9b-0c1d2e3f4a5b"
	const second = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	for _, c := range []struct{ reason, wantKey, wantAsk string }{
		{"", "", ""},
		{"reach api.github.com for ACME-179", "", ""},
		{"ACME-179: post it, approved at dispatch://ACME-179/ask/" + first, "ACME-179", first},
		{"dispatch://ACME-179/ask/" + first + " and dispatch://ACME-810/ask/" + second, "ACME-179", first},
		{"dispatch://ACME-179/ask/" + strings.ToUpper(first), "", ""},
		{"dispatch://ACME-179/ask/" + first[:len(first)-1], "", ""},
		{"dispatch://ACME-179 and the ask " + first, "", ""},
		{"dispatch://ACME-179/ask/", "", ""},
		{"dispatch://acme-179/ask/" + first, "", ""},
	} {
		key, ask := AskRefInReason(c.reason)
		if key != c.wantKey || ask != c.wantAsk {
			t.Errorf("AskRefInReason(%q) = %q, %q; want %q, %q", c.reason, key, ask, c.wantKey, c.wantAsk)
		}
	}
}
