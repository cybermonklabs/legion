package record

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/internal/broker/proof"
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

// TestMayJudgeAdmitsOneBoundedSecretNamingAnIssue pins the three conditions a record must meet
// before any model is asked, and which one each refusal names: one agent secret, a lifetime the
// rules let the judge decide for that secret (a record with none is bounded by nothing and is
// refused, one asking for exactly the bound is admitted), and a reason naming a Dispatch issue.
func TestMayJudgeAdmitsOneBoundedSecretNamingAnIssue(t *testing.T) {
	judged := func(secret string) (uint32, bool) {
		if secret == "DEEL_API_KEY" {
			return 900, true
		}
		return 0, false
	}
	object := func(details []AuthorizationDetail, reason string) RequestObject {
		return RequestObject{Details: details, Reason: reason}
	}
	secret := func(name string) AuthorizationDetail {
		return AuthorizationDetail{Type: KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	reason := "work on INFRA-1353 today"
	body := func(seconds *int) Body {
		b := podBody("eyJ.fake.jws")
		b.LifetimeSeconds = seconds
		return b
	}
	for _, c := range []struct {
		name         string
		body         Body
		obj          RequestObject
		judgeSeconds func(string) (uint32, bool)
		want         string // the refusal names this, "" for a record the judge may decide
	}{
		{
			name: "one secret, inside the bound, naming an issue",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: judged,
		},
		{
			name: "a lifetime of exactly the bound",
			body: body(new(900)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY")}, reason), judgeSeconds: judged,
		},
		{
			name: "two secrets",
			body: body(new(300)), obj: object([]AuthorizationDetail{secret("DEEL_API_KEY"), secret("OTHER_KEY")}, reason), judgeSeconds: judged,
			want: "2 authorization details",
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
			name: "a reason naming no issue",
			body: body(new(300)), judgeSeconds: judged,
			obj:  object([]AuthorizationDetail{secret("DEEL_API_KEY")}, "deploying the broker"),
			want: "names no Dispatch issue",
		},
	} {
		err := MayJudge(c.body, c.obj, c.judgeSeconds)
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
// the verifier's own JudgeSeconds, decides it. A judged approval of one bounded secret naming an
// issue verifies; one of two secrets, one past the entry's judge, one whose reason names no
// issue, and one against a verifier left no bound at all are each ErrChainBroken naming the model
// and the reason MayJudge gave. A judged event whose row also carries a login is still the
// judge's, so no login written beside it can buy a record MayJudge refuses. The person's arm is
// untouched: the approver's own approval verifies and another login's does not.
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
	for _, c := range []struct {
		name         string
		body         Body
		event        TerminalEvent
		judgeSeconds func(string) (uint32, bool)
		want         string // the refusal names this, "" for a chain that verifies
	}{
		{
			name: "the judge, on one secret inside the bound, naming an issue",
			body: stored(one, issue, 300), event: byJudge, judgeSeconds: judged,
		},
		{
			name: "the judge, on two secrets",
			body: stored(two, issue, 300), event: byJudge, judgeSeconds: judged,
			want: "2 authorization details",
		},
		{
			name: "the judge, past the entry's judge",
			body: stored(one, issue, 901), event: byJudge, judgeSeconds: judged,
			want: "past the 900",
		},
		{
			name: "the judge, on a reason naming no issue",
			body: stored(one, "deploying the broker", 300), event: byJudge, judgeSeconds: judged,
			want: "names no Dispatch issue",
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
			want:  "2 authorization details",
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
				JudgeSeconds: c.judgeSeconds,
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
