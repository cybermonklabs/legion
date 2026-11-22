package approval_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/answer"
)

// signedOutAnswer is a browser with no session's whole answer to GET /approve/{id}: its status, its
// headers and its body, as one string. Only the values every answer or every sign-in draws fresh
// are taken out: the answer's own id (its X-Broker-Answer-Id header, new on every answer whatever
// it names), and the sign-in's state, nonce and PKCE challenge, in the redirect and in each
// cookie's value. Everything else compares byte for byte.
func signedOutAnswer(t *testing.T, r *rig, id string) string {
	t.Helper()
	res := r.browser(t).Get(t, "/approve/"+id)
	header := res.Header.Clone()
	if len(header.Get(answer.Header)) != 32 {
		t.Fatalf("GET /approve/%s with no session carries no answer id; want an answer the broker wrote", id)
	}
	header.Del(answer.Header)
	header.Del("Date")
	if res.Status == http.StatusFound {
		location, err := url.Parse(header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		query := location.Query()
		for _, fresh := range []string{"state", "nonce", "code_challenge"} {
			if query.Get(fresh) == "" {
				t.Fatalf("GET /approve/%s with no session redirects to %s, with no %s", id, location, fresh)
			}
			query.Set(fresh, "-")
		}
		location.RawQuery = query.Encode()
		header.Set("Location", location.String())
	}
	cookies := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, cookie := range cookies {
		pair, attributes, _ := strings.Cut(cookie, ";")
		name, _, _ := strings.Cut(pair, "=")
		header.Add("Set-Cookie", name+"=-;"+attributes)
	}
	var whole bytes.Buffer
	fmt.Fprintf(&whole, "%d\n", res.Status)
	if err := header.Write(&whole); err != nil {
		t.Fatal(err)
	}
	whole.WriteString(res.Body)
	return whole.String()
}

// TestASignedOutSignInIsTheSameForEveryRecordID: a browser with no session that asks for a record's
// page is sent to sign in, and is sent the same way whatever the id names: a pending secret
// request, one that expired, one its approver decided, a machine login, or no record at all. The
// page reads no record before a person signs in, so the sign-in names nobody (no login_hint), and
// nothing in the answer says whether the record exists or whose it is. Before, a record's sign-in
// named its approver and an unknown id's named no one.
func TestASignedOutSignInIsTheSameForEveryRecordID(t *testing.T) {
	r := newRig(t)
	enrollmentID, key := r.benSession(t, "ben-box")
	pending := r.request(t, enrollmentID, key, "deploy", nil, "", "TEST_SECRET")
	expired := r.request(t, enrollmentID, key, "rotate", nil, "", "SECOND_SECRET")
	if _, err := r.st.Pool.Exec(t.Context(), `update requests set pending_expires_at = now() - interval '1 second' where id = $1`, expired.requestID); err != nil {
		t.Fatal(err)
	}
	r.sweeper.Tick(t.Context())
	if state := r.state(t, expired.recordID); state != "expired" {
		t.Fatalf("the expired request's record is %s, want expired", state)
	}
	decided := r.request(t, enrollmentID, key, "read the key", nil, "", "TEST_SECRET")
	approver := r.browser(t)
	approver.SignIn(t, decided.recordID, person(ada))
	approver.Decide(t, decided.recordID, false)
	if state := r.state(t, decided.recordID); state != "denied" {
		t.Fatalf("the decided request's record is %s, want denied", state)
	}
	login, _ := r.machineLogin(t, newKey(t), ben, "ben-laptop")

	records := map[string]string{
		"a pending secret request":  pending.recordID,
		"an expired secret request": expired.recordID,
		"a decided secret request":  decided.recordID,
		"a machine login":           login,
		"an id no record has":       strings.Repeat("0", 64),
	}
	unknown := signedOutAnswer(t, r, records["an id no record has"])
	if !strings.HasPrefix(unknown, "302\n") || strings.Contains(unknown, "login_hint") {
		t.Fatalf("an unknown id with no session answered:\n%s\nwant a 302 to sign in that names nobody", unknown)
	}
	for what, id := range records {
		if got := signedOutAnswer(t, r, id); got != unknown {
			t.Errorf("GET /approve/{id} with no session, naming %s, answered:\n%s\nand naming an id no record has:\n%s\nwant the same answer", what, got, unknown)
		}
	}
}
