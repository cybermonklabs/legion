// Package record implements the credential-request record: its canonical body and content-addressed
// id, who may decide it, and verification of the requester's signed request object (the shared
// broker contract).
package record

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// RequestTyp is the JWS "typ" header for a credential-request object. It is disjoint from
// proof's "agent-secrets-proof+jwt": each verifier refuses the other's typ.
const RequestTyp = "agent-secrets-request+jwt"

// canonicalHeader is the first line of every canonical body.
const canonicalHeader = "agent-secrets-record/v1"

// maxRequestLifetime bounds how far past iat a request object's exp may sit.
const maxRequestLifetime = 600 * time.Second

// ErrRequestInvalid is wrapped around every request-object verification failure. The wrapped
// reason never quotes the JWS.
var ErrRequestInvalid = errors.New("request object invalid")

var (
	hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$`)
	servicePattern  = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	slotPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	idPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidSlot reports whether slot may name a pod enrollment's slot: one of several independent
// identities in one pod, such as a Legion role and its generation. The empty slot is the
// enrollment's one identity and is not a slot value.
func ValidSlot(slot string) bool {
	return slotPattern.MatchString(slot)
}

// ValidService reports whether service may name a service: lowercase letters, digits and hyphens,
// at most 64, the form a machine login's launcher_credential detail names its service in.
func ValidService(service string) bool {
	return servicePattern.MatchString(service)
}

// ValidID reports whether id has a credential-request record id's form, the lowercase-hex SHA-256
// Body.ID answers, so a caller can refuse anything else before it reads the database with it.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// CanonicalLogin lowercases and trims the name Dispatch signs a person in with, their email (a
// record created before people were named by email keeps the GitHub login it was decided under).
// Every login comparison in the module goes through this form on both sides.
func CanonicalLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// HumanActor is the audit actor of a change a person made: "human:" and their canonical login,
// the person signed in to the broker's own approval page, or, for a machine login's decision or a
// revoke, the person Dispatch's server vouches for from its own signed-in session.
func HumanActor(login string) string {
	return "human:" + CanonicalLogin(login)
}

// LauncherActor is the audit actor of a change a machine login's own launcher proof made:
// "launcher:" and that launcher credential's id, whose own row names its operator.
func LauncherActor(credentialID string) string {
	return "launcher:" + credentialID
}

// judgePrefix opens the audit actor of a decision the model judge made. JudgeActor writes it and
// JudgeLogin reads it, and they are the only two places it is spelled.
const judgePrefix = "judge:"

// JudgeActor is the audit actor of a decision the model judge made: "judge:" and the id of the
// model that decided it, which is what the audit row names in place of a person. It is no
// person's login: a judge's approval is the broker's own, taken inside the bound the airlock
// rules allow the judge, and the person who would otherwise have decided is named nowhere in it.
func JudgeActor(modelID string) string {
	return judgePrefix + modelID
}

// JudgeLogin reads a judge actor back: the model id JudgeActor made it from, and whether actor is
// one at all. It is the one reader of the prefix, so every place that must tell the judge's
// decision from a person's asks it rather than comparing the string for itself. An actor with no
// "judge:" prefix is no judge, and neither is the bare prefix, which names no model.
func JudgeLogin(actor string) (modelID string, ok bool) {
	modelID, ok = strings.CutPrefix(actor, judgePrefix)
	return modelID, ok && modelID != ""
}

// AnyoneApprover is the approver of a record anyone signed in to Dispatch may decide: a request
// for a shared human-tier secret. It is never a person's login, and no login is it.
const AnyoneApprover = "anyone"

// The two kinds of credential-request record: a session's request for agent secrets, and a
// machine login, which mints a launcher credential.
const (
	KindAgentSecret        = "agent_secret"
	KindLauncherCredential = "launcher_credential"
)

// AuthorizationDetail is one entry of a request object's RFC 9396 authorization_details.
type AuthorizationDetail struct {
	Type       string   `json:"type"` // "agent_secret" | "launcher_credential"
	Identifier string   `json:"identifier"`
	Actions    []string `json:"actions,omitempty"` // ["inject"] for agent_secret
	Service    string   `json:"service,omitempty"`
}

// RequestObject is the verified claims of a credential-request object plus the verbatim compact
// JWS it came from.
type RequestObject struct {
	Compact    string
	Thumbprint string // iss, equal to the embedded JWK's RFC 7638 thumbprint
	Audience   string
	JTI        string
	IssuedAt   time.Time
	Expires    time.Time
	Details    []AuthorizationDetail
	Reason     string
	LoginHint  string // machine logins only
	// LifetimeSeconds is the lifetime the request object signed; nil when it named none, and the
	// broker then applies its 12-hour default or default-grant=session.
	LifetimeSeconds *int
}

// Service is the service a machine login is for: its one launcher_credential detail's service, ""
// for a person's machine and for every request for agent secrets.
func (o RequestObject) Service() string {
	if len(o.Details) == 1 && o.Details[0].Type == KindLauncherCredential {
		return o.Details[0].Service
	}
	return ""
}

// requestClaims is the JSON payload of a credential-request object (RFC 9101 §4 shape, RAR
// authorization_details per RFC 9396 §2). lifetime_seconds is this broker's own claim: absent, the
// broker gives the grant 15 minutes when any name is human tier, else 12 hours, or until its
// session ends under default-grant=session. A broker from before it decodes the payload without
// refusing unknown claims, so it ignores the claim and grants for its own fixed lifetime; a client
// that asked for one reads the lifetime the answer echoes to tell.
type requestClaims struct {
	Issuer               string                `json:"iss"`
	Audience             string                `json:"aud"`
	JTI                  string                `json:"jti"`
	IssuedAt             int64                 `json:"iat"`
	Expires              int64                 `json:"exp"`
	AuthorizationDetails []AuthorizationDetail `json:"authorization_details"`
	Reason               string                `json:"reason,omitempty"`
	LoginHint            string                `json:"login_hint,omitempty"`
	LifetimeSeconds      *int                  `json:"lifetime_seconds,omitempty"`
}

// ValidLifetime reports whether seconds may be a grant's lifetime: a whole number of seconds from
// one up to the largest the requests table's integer column holds. There is no other cap.
func ValidLifetime(seconds int) bool {
	return seconds >= 1 && seconds <= math.MaxInt32
}

// VerifyRequestObject enforces the contract: single ES256 JWS, typ RequestTyp, embedded P-256 JWK,
// iss == thumbprint(jwk), aud == audience, iat within skew, exp in (now, iat+10m], reason ≤400 runes
// with categories Cc/Cf/Cs/Co/Zl/Zp refused, details non-empty and single-typed, launcher identifiers
// matching the contract hostname pattern, service matching [a-z0-9-]{1,64}, and a lifetime, when
// named, on a request for agent secrets alone and ValidLifetime. Replay (jti) is the caller's (it
// needs the store). Every failure wraps ErrRequestInvalid with a reason that never quotes the JWS.
func VerifyRequestObject(compact, audience string, skew time.Duration, now time.Time) (RequestObject, error) {
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(sig.Signatures) != 1 {
		return RequestObject{}, fmt.Errorf("%w: not a single ES256 JWS", ErrRequestInvalid)
	}
	header := sig.Signatures[0].Protected
	if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); typ != RequestTyp {
		return RequestObject{}, fmt.Errorf("%w: typ is not %s", ErrRequestInvalid, RequestTyp)
	}
	if header.JSONWebKey == nil {
		return RequestObject{}, fmt.Errorf("%w: no embedded key", ErrRequestInvalid)
	}
	pub, ok := header.JSONWebKey.Key.(*ecdsa.PublicKey)
	if !ok || pub.Curve.Params().Name != "P-256" {
		return RequestObject{}, fmt.Errorf("%w: key is not EC P-256", ErrRequestInvalid)
	}
	payload, err := sig.Verify(pub)
	if err != nil {
		return RequestObject{}, fmt.Errorf("%w: signature", ErrRequestInvalid)
	}
	var c requestClaims
	if err := json.Unmarshal(payload, &c); err != nil || c.JTI == "" {
		return RequestObject{}, fmt.Errorf("%w: claims", ErrRequestInvalid)
	}
	thumbprint, err := proof.Thumbprint(pub)
	if err != nil || c.Issuer == "" || c.Issuer != thumbprint {
		return RequestObject{}, fmt.Errorf("%w: iss is not the embedded key's thumbprint", ErrRequestInvalid)
	}
	if c.Audience == "" || c.Audience != audience {
		return RequestObject{}, fmt.Errorf("%w: aud does not match", ErrRequestInvalid)
	}
	issued := time.Unix(c.IssuedAt, 0)
	if issued.After(now.Add(skew)) || issued.Before(now.Add(-skew)) {
		return RequestObject{}, fmt.Errorf("%w: iat outside %s", ErrRequestInvalid, skew)
	}
	expires := time.Unix(c.Expires, 0)
	if !expires.After(now) || expires.After(issued.Add(maxRequestLifetime)) {
		return RequestObject{}, fmt.Errorf("%w: exp outside (now, iat+%s]", ErrRequestInvalid, maxRequestLifetime)
	}
	if err := validateDetails(c.AuthorizationDetails); err != nil {
		return RequestObject{}, fmt.Errorf("%w: %s", ErrRequestInvalid, err)
	}
	if err := validateReason(c.Reason); err != nil {
		return RequestObject{}, fmt.Errorf("%w: %s", ErrRequestInvalid, err)
	}
	if c.LifetimeSeconds != nil {
		switch {
		case c.AuthorizationDetails[0].Type != KindAgentSecret:
			return RequestObject{}, fmt.Errorf("%w: lifetime_seconds is only for a request for agent secrets", ErrRequestInvalid)
		case !ValidLifetime(*c.LifetimeSeconds):
			return RequestObject{}, fmt.Errorf("%w: lifetime_seconds is not between 1 and %d", ErrRequestInvalid, math.MaxInt32)
		}
	}
	return RequestObject{
		Compact:         compact,
		Thumbprint:      c.Issuer,
		Audience:        c.Audience,
		JTI:             c.JTI,
		IssuedAt:        issued,
		Expires:         expires,
		Details:         c.AuthorizationDetails,
		Reason:          c.Reason,
		LoginHint:       c.LoginHint,
		LifetimeSeconds: c.LifetimeSeconds,
	}, nil
}

// validateDetails enforces: non-empty, and single-typed — every entry "agent_secret", or exactly
// one "launcher_credential" entry whose identifier is a valid hostname and whose optional service
// matches [a-z0-9-]{1,64}.
func validateDetails(details []AuthorizationDetail) error {
	if len(details) == 0 {
		return errors.New("authorization_details is empty")
	}
	allSecret := true
	for _, d := range details {
		if d.Type != KindAgentSecret {
			allSecret = false
			break
		}
	}
	if allSecret {
		for _, d := range details {
			if d.Identifier == "" {
				return errors.New("agent_secret detail has no identifier")
			}
		}
		return nil
	}
	if len(details) != 1 || details[0].Type != KindLauncherCredential {
		return errors.New("authorization_details mixes types")
	}
	d := details[0]
	if len(d.Identifier) > 253 || !hostnamePattern.MatchString(d.Identifier) {
		return errors.New("launcher_credential identifier is not a valid hostname")
	}
	if d.Service != "" && !ValidService(d.Service) {
		return errors.New("launcher_credential service does not match [a-z0-9-]{1,64}")
	}
	return nil
}

// MaxReasonRunes is the longest reason a request may sign, and ReasonRefuses the runes it may
// not carry: Unicode categories Cc, Cf, Cs, Co, Zl and Zp (bidi overrides and zero-width
// characters are Cf). Both are exported because the model judge holds what a model writes to
// the same bound and the same categories (judge.sanitize, judge/model.go), which drops them
// rather than refusing; one definition there and here keeps a reason a person reads in the
// grant log and a reason a requester signed to one rule.
const MaxReasonRunes = 400

// ReasonRefuses reports whether r is a rune no reason may carry.
func ReasonRefuses(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp)
}

// validateReason enforces: at most MaxReasonRunes runes, none a rune ReasonRefuses.
func validateReason(reason string) error {
	if utf8.RuneCountInString(reason) > MaxReasonRunes {
		return fmt.Errorf("reason is over %d runes", MaxReasonRunes)
	}
	for _, r := range reason {
		if ReasonRefuses(r) {
			return errors.New("reason contains a refused rune category")
		}
	}
	return nil
}

// Enrollment identifies the enrollment a credential request came from. A machine login (no
// enrollment) carries "-", "-", "-". Slot is a pod enrollment's slot, "" for every enrollment
// without one.
type Enrollment struct{ Kind, RuntimeID, Operator, Slot string }

// Body is the credential-request record's decision fields, fixed at creation, plus the verbatim
// signed request object. It is never updated after creation.
type Body struct {
	Request  string // compact JWS
	Approver string // canonical login, or AnyoneApprover
	// Enrollment is the requesting session, "-" in every field for a machine login.
	Enrollment Enrollment
	// LifetimeSeconds is how long the grant or launcher credential lasts once approved; nil for a
	// grant that lasts until its session ends, rendered "-".
	LifetimeSeconds *int
	RulesVersion    string
	ExpiresAt       time.Time // UTC, seconds precision
	Code            string    // "" for agent_secret records
}

// Canonical renders the body in the contract's exact line format. Every line is "\n"-terminated;
// an empty operator or code, and a nil lifetime, render as "-". The enrollment line carries a
// pod's slot as a fourth tab-separated field only when there is one: a body with no slot keeps the
// three-field line, the bytes and record id every slotless record is stored under.
func (b Body) Canonical() string {
	operator := b.Enrollment.Operator
	if operator == "" {
		operator = "-"
	}
	code := b.Code
	if code == "" {
		code = "-"
	}
	var sb strings.Builder
	sb.WriteString(canonicalHeader)
	sb.WriteByte('\n')
	sb.WriteString("request: ")
	sb.WriteString(b.Request)
	sb.WriteByte('\n')
	sb.WriteString("approver: ")
	sb.WriteString(b.Approver)
	sb.WriteByte('\n')
	sb.WriteString("enrollment: ")
	sb.WriteString(b.Enrollment.Kind)
	sb.WriteByte('\t')
	sb.WriteString(b.Enrollment.RuntimeID)
	sb.WriteByte('\t')
	sb.WriteString(operator)
	if b.Enrollment.Slot != "" {
		sb.WriteByte('\t')
		sb.WriteString(b.Enrollment.Slot)
	}
	sb.WriteByte('\n')
	sb.WriteString("lifetime_seconds: ")
	if b.LifetimeSeconds == nil {
		sb.WriteByte('-')
	} else {
		sb.WriteString(strconv.Itoa(*b.LifetimeSeconds))
	}
	sb.WriteByte('\n')
	sb.WriteString("rules_version: ")
	sb.WriteString(b.RulesVersion)
	sb.WriteByte('\n')
	sb.WriteString("expires_at: ")
	sb.WriteString(b.ExpiresAt.UTC().Format(time.RFC3339))
	sb.WriteByte('\n')
	sb.WriteString("code: ")
	sb.WriteString(code)
	sb.WriteByte('\n')
	return sb.String()
}

// ID is the lowercase-hex SHA-256 of the body's canonical form.
func (b Body) ID() string {
	sum := sha256.Sum256([]byte(b.Canonical()))
	return hex.EncodeToString(sum[:])
}

// canonicalLines is the fixed number of "\n"-terminated lines Canonical produces (the trailing
// split element after the last "\n" is the ninth, empty, element).
const canonicalLines = 9

// ParseBody is the exact inverse of Canonical: it refuses any deviation from the fixed line
// format, including a value that would still parse but not reproduce the input byte-for-byte.
func ParseBody(canonical string) (Body, error) {
	lines := strings.Split(canonical, "\n")
	if len(lines) != canonicalLines || lines[canonicalLines-1] != "" {
		return Body{}, fmt.Errorf("canonical body: wrong line count")
	}
	if lines[0] != canonicalHeader {
		return Body{}, fmt.Errorf("canonical body: bad header line")
	}
	request, ok := strings.CutPrefix(lines[1], "request: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad request line")
	}
	approver, ok := strings.CutPrefix(lines[2], "approver: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad approver line")
	}
	enrollmentLine, ok := strings.CutPrefix(lines[3], "enrollment: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad enrollment line")
	}
	fields := strings.Split(enrollmentLine, "\t")
	if len(fields) != 3 && len(fields) != 4 {
		return Body{}, fmt.Errorf("canonical body: enrollment is not three or four tab-separated fields")
	}
	slot := ""
	if len(fields) == 4 {
		slot = fields[3]
		if fields[0] != "pod" || !ValidSlot(slot) {
			return Body{}, fmt.Errorf("canonical body: an enrollment's fourth field must be a pod's slot")
		}
	}
	operator := fields[2]
	if operator == "-" {
		operator = ""
	}
	lifetimeStr, ok := strings.CutPrefix(lines[4], "lifetime_seconds: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad lifetime_seconds line")
	}
	var lifetime *int
	if lifetimeStr != "-" {
		n, err := strconv.Atoi(lifetimeStr)
		if err != nil || !ValidLifetime(n) {
			return Body{}, fmt.Errorf("canonical body: lifetime_seconds is neither - nor a positive integer")
		}
		lifetime = &n
	}
	rulesVersion, ok := strings.CutPrefix(lines[5], "rules_version: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad rules_version line")
	}
	expiresStr, ok := strings.CutPrefix(lines[6], "expires_at: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad expires_at line")
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return Body{}, fmt.Errorf("canonical body: expires_at is not RFC3339")
	}
	codeStr, ok := strings.CutPrefix(lines[7], "code: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad code line")
	}
	code := codeStr
	if code == "-" {
		code = ""
	}
	b := Body{
		Request:         request,
		Approver:        approver,
		Enrollment:      Enrollment{Kind: fields[0], RuntimeID: fields[1], Operator: operator, Slot: slot},
		LifetimeSeconds: lifetime,
		RulesVersion:    rulesVersion,
		ExpiresAt:       expiresAt.UTC(),
		Code:            code,
	}
	if b.Canonical() != canonical {
		return Body{}, fmt.Errorf("canonical body: does not round-trip")
	}
	return b, nil
}

// ApproverLogin canonicalizes login and returns it when it may decide this record, a record of
// kind whose verified request object names service (RequestObject.Service), and ErrNotApprover
// otherwise (MayDecide). A record's approver is resolved when it is created — a secret's owner,
// AnyoneApprover for a shared human-tier secret, a person's machine login's login_hint, or
// AnyoneApprover for a service's login — and a service's login is decided by the service its
// signed request names whatever approver its record stored, so this one comparison is every
// decision's and every chain re-check's approver rule, and the login it returns is the one a
// decision records.
func (b Body) ApproverLogin(kind, service, login string) (string, error) {
	login = CanonicalLogin(login)
	if !MayDecide(kind, b.Approver, service, login) {
		return "", ErrNotApprover
	}
	return login, nil
}

// MayDecide reports whether login may decide a record of kind whose approver is approver and whose
// signed request names service: for a machine login that names a service, any login at all, since
// its credential acts as that service and as no person; for an agent_secret record whose approver
// is AnyoneApprover, any login; and otherwise the person the approver names. No login is
// AnyoneApprover itself, and no empty login decides anything. A person's machine login's approver
// is the person whose machine it becomes, so AnyoneApprover there admits no one, whatever binary
// opened the record. Nor is a judge actor (JudgeLogin) ever a login that decides: the judge's own
// approval is recorded under JudgeActor and never as a person, so an owner tag or a login_hint
// spelled "judge:..." makes no record the judge's to approve as a person.
func MayDecide(kind, approver, service, login string) bool {
	login, approver = CanonicalLogin(login), CanonicalLogin(approver)
	_, judge := JudgeLogin(login)
	switch {
	case login == "" || login == AnyoneApprover:
		return false
	case judge:
		return false
	case kind == KindLauncherCredential && service != "":
		return true
	case kind == KindAgentSecret && approver == AnyoneApprover:
		return true
	}
	return login == approver
}

// ErrNotJudgeable is wrapped around every reason the model judge may not decide a record: the
// request names other than one agent secret, the airlock rules allow the judge no lifetime for
// that secret or less than the record asks for, or the request's signed reason names no Dispatch
// issue. A caller that only asks "is this the judge's to decide" collapses them with
// errors.Is(err, ErrNotJudgeable); one that must say why keeps the wrapped reason.
var ErrNotJudgeable = errors.New("not a request the model judge may decide")

// JudgeAllowsLifetime reports whether a signed lifetime in seconds is within the seconds the
// airlock rules give the judge for one secret, which airlock.Rules.JudgeSeconds answers as
// bound. It is the one comparison three callers make — the judge's own check before the model
// (judge.lifetimeAllowed), MayJudge below, which holds a judged approval at decision time and
// again on every release, and requests.judgeStillDecides, which re-reads the rules for a grant
// already made — and it is written once here, so a judge that approves and a state machine
// that then refuses cannot drift apart. Each caller keeps its own refusal wording, which is
// what a person reads; only the comparison is shared.
//
// It lives in this package rather than beside JudgeSeconds in airlock because MayJudge must
// reach it and this package must not import airlock; the other two callers already import
// this one. Both sides are widened to int64, the bound being a uint32 and the lifetime an int,
// so neither is ever cut to the other's range.
func JudgeAllowsLifetime(bound uint32, lifetimeSeconds int) bool {
	return int64(lifetimeSeconds) <= int64(bound)
}

// MayJudge reports whether the model judge may decide this record at all, before any model is
// asked: the airlock rules' own bound on what a judge decides, read here so one rule answers for
// every caller. All three must hold, and the error names which one did not.
//
// judgeSeconds is the rules file's answer for one secret (airlock.Rules.JudgeSeconds); it is
// injected because this package must not import airlock. A nil func is no bound at all and
// refuses, as a record with no lifetime does: an unbounded grant is exactly what the judge's
// seconds are there to bound, so nothing the rules say can admit it.
func MayJudge(body Body, obj RequestObject, judgeSeconds func(secret string) (uint32, bool)) error {
	if len(obj.Details) != 1 {
		return fmt.Errorf("%w: the request names %d authorization details, and the judge decides a request for one secret", ErrNotJudgeable, len(obj.Details))
	}
	if obj.Details[0].Type != KindAgentSecret {
		return fmt.Errorf("%w: the request is for a %s, not an agent secret", ErrNotJudgeable, obj.Details[0].Type)
	}
	name := obj.Details[0].Identifier
	if judgeSeconds == nil {
		return fmt.Errorf("%w: no judge bound is available for %q", ErrNotJudgeable, name)
	}
	if body.LifetimeSeconds == nil {
		return fmt.Errorf("%w: the record names no lifetime, so no judge bound holds it", ErrNotJudgeable)
	}
	bound, ok := judgeSeconds(name)
	if !ok {
		return fmt.Errorf("%w: the rules let the judge decide nothing for %q", ErrNotJudgeable, name)
	}
	if !JudgeAllowsLifetime(bound, *body.LifetimeSeconds) {
		return fmt.Errorf("%w: the record asks for %d seconds of %q, past the %d the rules let the judge decide", ErrNotJudgeable, *body.LifetimeSeconds, name, bound)
	}
	if IssueKeyInReason(obj.Reason) == "" {
		return fmt.Errorf("%w: the request's reason names no Dispatch issue", ErrNotJudgeable)
	}
	return nil
}

// issueKeyPattern is a Dispatch issue key: a project key — an uppercase letter and then uppercase
// letters or digits, as contracts.ProjectKeyPattern spells it — a hyphen, and the issue's number.
// The word boundaries keep a key out of the middle of a longer word, so a reason that names no
// key cannot be read as naming one.
var issueKeyPattern = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-[0-9]+\b`)

// IssueKeyInReason is the first Dispatch issue key in a request's own signed reason, and "" when
// it names none. The proxy puts it in the line it logs for each request it carries a secret on,
// so an operator reading that line can open the issue the session said it was working on, and
// MayJudge reads it to tell whether the reason names an issue at all. It lives here, beside the
// reason the record signs, so the log line and the judge read one definition of an issue key.
func IssueKeyInReason(reason string) string {
	return issueKeyPattern.FindString(reason)
}
