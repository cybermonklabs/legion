package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// probeWords quotes a URL carrying a credential in its user information and its query, as a
// refusal's description may. Every credential here is made up.
const probeWords = "the issuer at https://u:not-a-secret-password@h.example/p?access" + "_token=not-a-secret-token"

var probeSecrets = []string{"not-a-secret-password", "not-a-secret-token"}

func holdsProbeSecret(text string) bool {
	for _, secret := range probeSecrets {
		if strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

// TestTheRuleReadsItsOwnReadingOfEveryFixtureRowAsItIs: the rule's unquoted reading of each fixture
// row (ruled), which a RetrieveError NewRetrieveError or OAuth2 builds holds, is its own reading,
// and so is Field of it, so sanitized and AnswerText, which read those fields by the rule again,
// print what they printed of a RetrieveError holding the field. The quoted reading is not
// idempotent on every field: the counterexample is a field the rule quotes (OneLine), whose
// quoting puts a backslash after a slash, which a second reading takes for a URL start it cannot
// read; it is why the RetrieveError holds the unquoted reading, and it is the control that this
// comparison can fail.
func TestTheRuleReadsItsOwnReadingOfEveryFixtureRowAsItIs(t *testing.T) {
	for _, row := range readRemoteFixture(t) {
		if once := Field(row.Field); Field(once) != once {
			t.Errorf("row %s: Field = %q, and Field of that = %q", row.ID, once, Field(once))
		}
		if once := ruled(row.Field); ruled(once) != once {
			t.Errorf("row %s: ruled = %q, and ruled of that = %q", row.ID, once, ruled(once))
		}
	}
	quoted := "/\x94"
	if once := Field(quoted); Field(once) == once || Field(once) != WordsNotReadable {
		t.Errorf("Field(%q) = %q, and Field of that = %q; want the second reading not readable", quoted, once, Field(once))
	}
	if once := ruled(quoted); ruled(once) != once {
		t.Errorf("ruled(%q) = %q, and ruled of that = %q", quoted, once, ruled(once))
	}
}

// FuzzTheUnquotedReadingIsItsOwnReading is the unquoted half of
// TestTheRuleReadsItsOwnReadingOfEveryFixtureRowAsItIs over any field.
func FuzzTheUnquotedReadingIsItsOwnReading(f *testing.F) {
	for _, seed := range []string{"/\x94", "see /\npath", probeWords, "(https://h/p?q=1)"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, field string) {
		if once := ruled(field); ruled(once) != once {
			t.Errorf("ruled(%q) = %q, and ruled of that = %q", field, once, ruled(once))
		}
	})
}

// TestNewRetrieveErrorHoldsTheRulesReading: the RetrieveError NewRetrieveError builds holds the
// rule's unquoted reading of the code and of the description, and no error_uri or body, so
// printing it whole - its Error, each fmt verb, encoding/json - prints none of a fixture row's
// secrets, while a RetrieveError holding the same fields as they came prints them: the control.
func TestNewRetrieveErrorHoldsTheRulesReading(t *testing.T) {
	rows := readRemoteFixture(t)
	controlLeaked := false
	for _, row := range append(rows, remoteRow{ID: "probe-words", Field: probeWords, Secrets: probeSecrets}) {
		re := NewRetrieveError(http.StatusBadRequest, NewRemoteText("invalid_grant"), NewRemoteText(row.Field))
		if re.ErrorCode != "invalid_grant" || re.ErrorDescription != ruled(row.Field) || re.ErrorURI != "" || re.Body != nil {
			t.Errorf("row %s: NewRetrieveError holds %q %q %q %q, want the rule's reading and no error_uri or body",
				row.ID, re.ErrorCode, re.ErrorDescription, re.ErrorURI, re.Body)
		}
		raw := &oauth2.RetrieveError{Response: re.Response, ErrorCode: "invalid_grant", ErrorDescription: row.Field}
		for form, text := range wholePrints(re) {
			for _, secret := range row.Secrets {
				if strings.Contains(text, secret) {
					t.Errorf("row %s: %s of NewRetrieveError's answer prints %q: %s", row.ID, form, secret, text)
				}
			}
		}
		for _, text := range wholePrints(raw) {
			for _, secret := range row.Secrets {
				controlLeaked = controlLeaked || strings.Contains(text, secret)
			}
		}
	}
	if !controlLeaked {
		t.Error("no whole print of a RetrieveError holding the fields as they came prints a secret: the check above cannot fail")
	}
}

// wholePrints is re printed whole, each way a caller can print it without reading a field.
func wholePrints(re *oauth2.RetrieveError) map[string]string {
	out := map[string]string{"Error()": re.Error()}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		out[verb] = fmt.Sprintf(verb, re)
	}
	if data, err := json.Marshal(re); err == nil {
		out["json.Marshal"] = string(data)
	} else {
		out["json.Marshal"] = "error: " + err.Error()
	}
	return out
}

// TestTheRuleNeverChangesWhichOAuthCodeAFieldIs: the closed OAuth code a field reads as is the
// one its rule's reading reads as, for each named code, each fixture row, and each row with each
// named code before or after it: a named code is a fixed point of the rule, and nothing else the
// rule answers is one. So RefusalCode decides the same branch from NewRetrieveError's answer as
// from the code as it came. The control is a decision the rule does change, a prefix: it reads the
// code before a credential's URL the rule cannot read, and not the rule's reading of it.
func TestTheRuleNeverChangesWhichOAuthCodeAFieldIs(t *testing.T) {
	var fields []string
	for code, name := range oauthCodeNames {
		if name == "" {
			continue
		}
		if Field(name) != name || NewRemoteText(name).OAuthCode() != OAuthCode(code) {
			t.Errorf("%s: Field = %q, OAuthCode = %v", name, Field(name), NewRemoteText(name).OAuthCode())
		}
		fields = append(fields, name, name+" "+probeWords, probeWords+" "+name, `"`+name+`"`, name+"\n", "https://"+name+"@h.example/")
	}
	for _, row := range readRemoteFixture(t) {
		fields = append(fields, row.Field)
	}
	for _, field := range fields {
		if raw, ruled := NewRemoteText(field).OAuthCode(), NewRemoteText(Field(field)).OAuthCode(); raw != ruled {
			t.Errorf("%q reads as %v, and its rule's reading %q as %v", field, raw, Field(field), ruled)
		}
	}
	prefixed := func(field string) bool { return strings.HasPrefix(field, "invalid_grant") }
	changed := "invalid_grant: see https://u:p@h.example/%2Fx"
	if prefixed(changed) == prefixed(Field(changed)) {
		t.Errorf("the control: a prefix of %q reads the same from the rule's reading %q", changed, Field(changed))
	}
}

// FuzzTheRuleNeverChangesWhichOAuthCodeAFieldIs is TestTheRuleNeverChangesWhichOAuthCodeAFieldIs
// over any field.
func FuzzTheRuleNeverChangesWhichOAuthCodeAFieldIs(f *testing.F) {
	for _, name := range oauthCodeNames {
		if name != "" {
			f.Add(name)
			f.Add(name + " " + probeWords)
		}
	}
	f.Add(probeWords)
	f.Fuzz(func(t *testing.T, field string) {
		if raw, ruled := NewRemoteText(field).OAuthCode(), NewRemoteText(Field(field)).OAuthCode(); raw != ruled {
			t.Errorf("%q reads as %v, and its rule's reading %q as %v", field, raw, Field(field), ruled)
		}
	})
}

// coded is an error that holds its own code, as brokerclient's RefusedError does.
type coded struct {
	code   OAuthCode
	answer error
}

func (c *coded) Error() string          { return "refused" }
func (c *coded) Unwrap() error          { return c.answer }
func (c *coded) RefusalCode() OAuthCode { return c.code }
func retrieved(code string) *oauth2.RetrieveError {
	return &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, ErrorCode: code}
}

// TestRefusalCodeReadsTheFirstAnswerDownTheChain: an error that holds its own code answers before
// the answer it unwraps to; an x/oauth2 answer answers from its own code, through a wrapper and an
// errors.Join; a code no name covers is OAuthOther, and a chain with no answer OAuthNone.
func TestRefusalCodeReadsTheFirstAnswerDownTheChain(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		want OAuthCode
	}{
		{"its own code first", &coded{code: InvalidGrant, answer: retrieved("access_denied")}, InvalidGrant},
		{"x/oauth2's answer", retrieved("invalid_grant"), InvalidGrant},
		{"under a wrapper", fmt.Errorf("refresh: %w", retrieved("expired_token")), ExpiredToken},
		{"inside a join", errors.Join(errors.New("other"), retrieved("access_denied")), AccessDenied},
		{"a code no name covers", retrieved("server_error"), OAuthOther},
		{"no code", retrieved(""), OAuthNone},
		{"no answer", errors.New("dial tcp: refused"), OAuthNone},
		{"nil", nil, OAuthNone},
		{"NewRetrieveError's answer, the code beside a credential's URL", NewRetrieveError(400, NewRemoteText("invalid_grant "+probeWords), NewRemoteText("")), OAuthOther},
		{"NewRetrieveError's answer", NewRetrieveError(400, NewRemoteText("invalid_grant"), NewRemoteText(probeWords)), InvalidGrant},
	} {
		if got := RefusalCode(row.err); got != row.want {
			t.Errorf("%s: RefusalCode = %v, want %v", row.name, got, row.want)
		}
	}
	if InvalidGrant.String() != "invalid_grant" || holdsProbeSecret(OAuthOther.String()) || OAuthCode(200).String() != OAuthOther.String() {
		t.Errorf("OAuthCode prints %q, %q, %q", InvalidGrant, OAuthOther, OAuthCode(200))
	}
}

// TestSendBackAnswersOnlyWhatTheRulePrintsAsItCame: an id the rule prints as it came is sent back,
// one outside a session key's alphabet among them; a field the rule changes or cannot read, and an
// empty one, are not.
func TestSendBackAnswersOnlyWhatTheRulePrintsAsItCame(t *testing.T) {
	for _, row := range []struct {
		field RemoteText
		want  string
		sent  bool
	}{
		{NewRemoteText("ses_01a9"), "ses_01a9", true},
		{NewRemoteText("ses:bad"), "ses:bad", true},
		{NewRemoteText(probeWords), "", false},
		{NewRemoteText("u:not-a-secret-password@h.example"), "", false},
		{NewRemoteText("ses\n1"), "", false},
		{NewRemoteText(""), "", false},
		{RemoteText{}, "", false},
	} {
		if got, sent := row.field.SendBack(); got != row.want || sent != row.sent {
			t.Errorf("SendBack of %q = %q, %t; want %q, %t", row.field, got, sent, row.want, row.sent)
		}
	}
}

// TestTheParsesAnswerAValueAndNoText: Time, Int64, EqualFold and OAuthCode read the field as it
// came and answer a value, a bool or a closed code; a field that does not parse answers false.
func TestTheParsesAnswerAValueAndNoText(t *testing.T) {
	if at, ok := NewRemoteText("2026-12-01T00:00:00Z").Time(time.RFC3339); !ok || !at.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Time of an RFC 3339 time = %v, %t", at, ok)
	}
	if at, ok := NewRemoteText(probeWords).Time(time.RFC3339); ok || !at.IsZero() {
		t.Errorf("Time of words = %v, %t; want the zero time and false", at, ok)
	}
	if n, ok := NewRemoteText("3600").Int64(); !ok || n != 3600 {
		t.Errorf("Int64 of 3600 = %d, %t", n, ok)
	}
	if n, ok := NewRemoteText(probeWords).Int64(); ok || n != 0 {
		t.Errorf("Int64 of words = %d, %t", n, ok)
	}
	if !NewRemoteText("dpop").EqualFold("DPoP") || NewRemoteText("Bearer").EqualFold("DPoP") || !(RemoteText{}).EqualFold("") {
		t.Error("EqualFold")
	}
	if _, ok := (RemoteText{}).Time(time.RFC3339); ok {
		t.Error("Time of the zero RemoteText parsed")
	}
}

// TestARemoteNumberDecodesAsAJSONNumberDoes: a JSON number and a JSON string holding one decode
// into the number; a string that holds none is an error that quotes nothing of it, where
// json.Number's quotes the string; any other value is a type error; null leaves it empty. It
// prints as the rule reads its literal.
func TestARemoteNumberDecodesAsAJSONNumberDoes(t *testing.T) {
	decode := func(body string) (RemoteNumber, error) {
		var got struct {
			N RemoteNumber `json:"n"`
		}
		err := json.Unmarshal([]byte(body), &got)
		return got.N, err
	}
	for body, want := range map[string]int64{`{"n":3600}`: 3600, `{"n":"3600"}`: 3600, `{"n":-1}`: -1} {
		n, err := decode(body)
		if got, ok := n.Int64(); err != nil || !ok || got != want {
			t.Errorf("%s: %d, %t, %v; want %d", body, got, ok, err, want)
		}
	}
	if n, err := decode(`{"n":null}`); err != nil || !n.IsEmpty() {
		t.Errorf("null: %v, %v", n, err)
	}
	if n, err := decode(`{"n":3600.5}`); err != nil {
		t.Errorf("3600.5: %v", err)
	} else if _, ok := n.Int64(); ok {
		t.Error("3600.5 read as an integer")
	}
	words, _ := json.Marshal(map[string]string{"n": probeWords})
	_, err := decode(string(words))
	var asNumber struct {
		N json.Number `json:"n"`
	}
	control := json.Unmarshal(words, &asNumber)
	if err == nil || holdsProbeSecret(err.Error()) || control == nil || !holdsProbeSecret(control.Error()) {
		t.Errorf("a string that holds no number: %v; json.Number's error, the control: %v", err, control)
	}
	var typeErr *json.UnmarshalTypeError
	if _, err := decode(`{"n":true}`); !errors.As(err, &typeErr) {
		t.Errorf("true: %v, want a type error", err)
	}
	if printed := fmt.Sprintf("%v %+v %#v %s", RemoteNumber{NewRemoteText("3600")}, RemoteNumber{NewRemoteText("3600")}, RemoteNumber{NewRemoteText("3600")}, RemoteNumber{NewRemoteText("3600")}); holdsProbeSecret(printed) {
		t.Errorf("printed %s", printed)
	}
}

// TestNewOAuthRefusalSplitsBeforeItHolds: the device authorization endpoint's invalid_request
// described "<CODE>: <message>" is CODE and message; any other answer is its code and description.
func TestNewOAuthRefusalSplitsBeforeItHolds(t *testing.T) {
	for _, row := range []struct{ code, description, wantCode, wantMessage string }{
		{"invalid_request", "LOGIN_PENDING: a login is already pending", "LOGIN_PENDING", "a login is already pending"},
		{"invalid_request", "RATE_LIMITED: " + probeWords, "RATE_LIMITED", probeWords},
		{"invalid_request", "no code here", "invalid_request", "no code here"},
		{"access_denied", "LOGIN_PENDING: not split", "access_denied", "LOGIN_PENDING: not split"},
	} {
		code, message := NewOAuthRefusal(row.code, row.description)
		if !code.Equal(row.wantCode) || !message.Equal(row.wantMessage) {
			t.Errorf("NewOAuthRefusal(%q, %q) = %v, %v", row.code, row.description, code, message)
		}
		if holdsProbeSecret(code.String() + message.String()) {
			t.Errorf("NewOAuthRefusal(%q, %q) prints a secret", row.code, row.description)
		}
	}
}
