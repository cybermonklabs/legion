package approval_test

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/approval"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestEachRefusalOfThePageNamesItsCodeAndPrintsItsAnswerID drives every refusal the approval page
// writes through writeMessage, each of its sign-in's and decision's and both of its origin
// refusals, behind the answer recorder cmd/broker installs. Each writes exactly one record under
// answer.RefusalMessage naming the code its writer noted, since the page is HTML and the record
// reads no code from it, and each refusal page prints the answer id its X-Broker-Answer-Id header
// carries, so the person refused has the id that record names. A page that refuses nothing
// prints none.
func TestEachRefusalOfThePageNamesItsCodeAndPrintsItsAnswerID(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	a := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
	page := "/approve/" + a.recordID
	b := r.browser(t)
	b.SignIn(t, a.recordID, person(ada))
	formA := decisionForm(t, b.Page(t, a.recordID).Body, a.recordID)
	later := r.request(t, enrollmentID, key, "later", nil, "", "SECOND_SECRET")
	secret, err := base64.RawURLEncoding.DecodeString(b.Cookie(t, "__Host-broker-session"))
	if err != nil {
		t.Fatal(err)
	}
	// started is a fresh browser with a sign-in started and completed at the provider as claims,
	// and the callback URL the provider returned it to.
	started := func(t *testing.T, claims map[string]any) (*brokertest.Browser, *url.URL) {
		fresh := r.browser(t)
		callback, err := url.Parse(fresh.Authorize(t, fresh.StartSignIn(t, page), claims))
		if err != nil {
			t.Fatal(err)
		}
		return fresh, callback
	}
	// withQuery is u with key set to value, or taken out for "".
	withQuery := func(u *url.URL, key, value string) string {
		q := u.Query()
		q.Del(key)
		if value != "" {
			q.Set(key, value)
		}
		changed := *u
		changed.RawQuery = q.Encode()
		return changed.String()
	}
	unreadable := url.Values{"padding": {strings.Repeat("a", 65<<10)}}
	elsewhere := map[string]string{"Origin": "https://elsewhere.example"}

	driven := 0
	for _, row := range []struct {
		name   string
		drive  func(t *testing.T) brokertest.Response
		status int
		code   string
	}{
		{"a page for no record id", func(t *testing.T) brokertest.Response {
			return r.browser(t).Get(t, "/approve/not-a-record")
		}, http.StatusBadRequest, "RECORD_ID_INPUT"},
		{"a record the broker does not have", func(t *testing.T) brokertest.Response {
			return b.Get(t, "/approve/"+strings.Repeat("a", 64))
		}, http.StatusNotFound, "NOT_FOUND"},
		{"a decision form it cannot read", func(t *testing.T) brokertest.Response {
			return b.FromPage(t, "/approve", unreadable)
		}, http.StatusBadRequest, "FORM_UNREADABLE"},
		{"a decision that is neither approve nor deny", func(t *testing.T) brokertest.Response {
			return b.FromPage(t, "/approve", decision(formA, "maybe"))
		}, http.StatusBadRequest, "DECISION_INVALID"},
		{"a decision naming no record", func(t *testing.T) brokertest.Response {
			form := decision(formA, "approve")
			form.Del("record")
			return b.FromPage(t, "/approve", form)
		}, http.StatusBadRequest, "DECISION_RECORDS_INVALID"},
		{"a token made for no sign-in", func(t *testing.T) brokertest.Response {
			form := decision(formA, "approve")
			form.Set("token", strings.Repeat("00", 32))
			return b.FromPage(t, "/approve", form)
		}, http.StatusForbidden, "PAGE_TOKEN_INVALID"},
		{"an expired page", func(t *testing.T) brokertest.Response {
			form := decision(formA, "approve")
			expires := time.Now().Add(-time.Second).Unix()
			form.Set("expires", strconv.FormatInt(expires, 10))
			form.Set("token", approval.PageToken(secret, expires, form["printed"]))
			return b.FromPage(t, "/approve", form)
		}, http.StatusForbidden, "PAGE_EXPIRED"},
		{"a record its page did not print", func(t *testing.T) brokertest.Response {
			form := decision(formA, "approve")
			form.Set("record", later.recordID)
			return b.FromPage(t, "/approve", form)
		}, http.StatusForbidden, "RECORD_NOT_PRINTED"},
		{"a decision from another origin", func(t *testing.T) brokertest.Response {
			return b.Post(t, "/approve", decision(formA, "approve"), elsewhere)
		}, http.StatusForbidden, "ORIGIN_REFUSED"},
		{"a callback with no sign-in in progress", func(t *testing.T) brokertest.Response {
			_, callback := started(t, person(ada))
			return r.browser(t).Get(t, callback.String())
		}, http.StatusBadRequest, "SIGN_IN_NOT_STARTED"},
		{"a state that is not the sign-in's", func(t *testing.T) brokertest.Response {
			fresh, callback := started(t, person(ada))
			return fresh.Get(t, withQuery(callback, "state", "not-the-state"))
		}, http.StatusBadRequest, "SIGN_IN_STATE_INVALID"},
		{"a sign-in returning outside /approve/", func(t *testing.T) brokertest.Response {
			fresh, callback := started(t, person(ada))
			fields := pendingCookie(t, fresh.Cookie(t, "__Host-broker-signin"))
			fields["next"] = "/v1/pending"
			return r.getWith(t, callback.String(), &http.Cookie{Name: "__Host-broker-signin", Value: encodePending(t, fields)})
		}, http.StatusBadRequest, "SIGN_IN_NEXT_INVALID"},
		{"a sign-in the provider refused", func(t *testing.T) brokertest.Response {
			fresh, callback := started(t, person(ada))
			refused, _ := url.Parse(withQuery(callback, "code", ""))
			return fresh.Get(t, withQuery(refused, "error", "access_denied"))
		}, http.StatusForbidden, "SIGN_IN_PROVIDER_REFUSED"},
		{"a forged code", func(t *testing.T) brokertest.Response {
			fresh, callback := started(t, person(ada))
			return fresh.Get(t, withQuery(callback, "code", "forged-code"))
		}, http.StatusForbidden, "SIGN_IN_UNVERIFIED"},
		{"an account of another domain", func(t *testing.T) brokertest.Response {
			claims := person(ada)
			claims["hd"] = "other.example"
			fresh, callback := started(t, claims)
			return fresh.Get(t, callback.String())
		}, http.StatusForbidden, "SIGN_IN_ACCOUNT_REFUSED"},
		{"a sign-out form it cannot read", func(t *testing.T) brokertest.Response {
			return b.FromPage(t, "/signin/logout", unreadable)
		}, http.StatusBadRequest, "FORM_UNREADABLE"},
		{"a sign-out from another origin", func(t *testing.T) brokertest.Response {
			return b.Post(t, "/signin/logout", url.Values{}, elsewhere)
		}, http.StatusForbidden, "ORIGIN_REFUSED"},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			res := row.drive(t)
			id := res.Header.Get(answer.Header)
			if res.Status != row.status || len(id) != 32 {
				t.Fatalf("%s = %d with answer id %q, want %d with one:\n%s", row.name, res.Status, id, row.status, res.Body)
			}
			if !strings.Contains(res.Body, "<code>"+id+"</code>") {
				t.Errorf("the refusal page does not print its answer id %s:\n%s", id, res.Body)
			}
			var codes []any
			for _, record := range capture.Records() {
				fields := map[string]any{}
				record.Attrs(func(a slog.Attr) bool { fields[a.Key] = a.Value.Any(); return true })
				if record.Message == answer.RefusalMessage && fields["answer_id"] == id {
					if record.Level != slog.LevelWarn {
						t.Errorf("the refusal's record is at %v, want WARN", record.Level)
					}
					codes = append(codes, fields["code"])
				}
			}
			if len(codes) != 1 || codes[0] != row.code {
				t.Errorf("the refusal wrote records naming the codes %v, want one naming %s", codes, row.code)
			}
		})
	}
	if driven != 17 {
		t.Fatalf("the table drove %d of the page's 17 refusals", driven)
	}

	// The page that answers a sign-out refuses nothing, and prints no answer id.
	if res := b.FromPage(t, "/signin/logout", url.Values{"next": {page}}); res.Status != http.StatusOK || strings.Contains(res.Body, "answer id") {
		t.Errorf("a sign-out = %d, want 200 with no answer id printed:\n%s", res.Status, res.Body)
	}
}

// TestTheRateLimitedPagePrintsNoAnswerID: the page's 429 (Page.WriteRateLimited), behind the
// answer recorder cmd/broker installs, carries its answer id in its X-Broker-Answer-Id header
// alone. A 429 is counted and writes no record (answer.Refused), so the page prints no id that
// would name a line the broker never wrote.
func TestTheRateLimitedPagePrintsNoAnswerID(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	limited := answer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		new(approval.Page).WriteRateLimited(w, 1)
	}), nil)
	w := httptest.NewRecorder()
	limited.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/approve/not-a-record", nil))
	id := w.Header().Get(answer.Header)
	if w.Code != http.StatusTooManyRequests || len(id) != 32 {
		t.Fatalf("the rate-limited page = %d with answer id %q, want 429 with one:\n%s", w.Code, id, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, id) || strings.Contains(body, "answer id") {
		t.Errorf("the rate-limited page prints an answer id, though a 429 writes no record:\n%s", body)
	}
	for _, record := range capture.Records() {
		if record.Message == answer.RefusalMessage {
			t.Errorf("the rate-limited page wrote a refusal record: %v", record)
		}
	}
}
