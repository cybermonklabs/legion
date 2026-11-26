package bus_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A session reply subject is <inbox>.<session id>.<token>, the token 26 characters of
// crypto/rand.Text and new on every call, and the session's grant entry admits it and no other
// session's.
func TestASessionReplySubjectNamesItsSessionAndARandomToken(t *testing.T) {
	t.Parallel()
	inbox := natsgo.NewInbox()
	first, second := bus.SessionReplySubject(inbox, "ses_one"), bus.SessionReplySubject(inbox, "ses_one")
	for _, reply := range []string{first, second} {
		token, ok := strings.CutPrefix(reply, inbox+".ses_one.")
		if !ok || len(token) != 26 || strings.Trim(token, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
			t.Fatalf("reply subject %q, want %s.ses_one.<26 characters of base32>", reply, inbox)
		}
	}
	if first == second {
		t.Fatalf("two reply subjects of one session are both %q", first)
	}
	if !bus.SubjectContains(bus.SessionReplies("ses_one"), first) {
		t.Fatalf("%s does not admit %s", bus.SessionReplies("ses_one"), first)
	}
	if bus.SubjectContains(bus.SessionReplies("ses_two"), first) {
		t.Fatalf("%s admits ses_one's %s", bus.SessionReplies("ses_two"), first)
	}
}

// A user whose one publish entry under _INBOX. is a session's (SessionReplies) publishes to that
// session's reply subjects and to no other _INBOX. subject: not another session's, not a plain
// inbox (_INBOX.<id>, which the listener's connection probe asked on before), not nats.go's request
// inbox (_INBOX.<id>.<token>, which Dispatch's agent stream replay asked on before) or the relay's
// old counter token (_INBOX.<id>.<n>, the same shape), and not the session's id with nothing after
// it. The server enforces it: a watcher on _INBOX.> hears exactly the admitted publish, and the
// session's connection is told of every refusal, which is the control that the refusals are the
// grant's and the watcher is not deaf.
func TestASessionsReplyGrantAdmitsItsOwnReplySubjectsAlone(t *testing.T) {
	t.Parallel()
	seed, public := testnats.User(t)
	config := fmt.Sprintf(`authorization {
  users = [
    { user: watcher, password: watcher }
    { nkey: %q, permissions: { publish: { allow: [%q] }, subscribe: { allow: ["nothing.here"] } } }
  ]
}
`, public, bus.SessionReplies("ses_one"))
	_, uri := testnats.StartConfig(t, config, nil, false)
	watcher, err := natsgo.Connect(uri, natsgo.UserInfo("watcher", "watcher"))
	if err != nil {
		t.Fatalf("connect the watcher: %v", err)
	}
	t.Cleanup(watcher.Close)
	heard, err := watcher.SubscribeSync(natsgo.InboxPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe the watcher: %v", err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	var mu sync.Mutex
	var refused []string
	session, err := testnats.ConnectSeed(t, uri, seed, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
		mu.Lock()
		defer mu.Unlock()
		refused = append(refused, err.Error())
	}))
	if err != nil {
		t.Fatalf("connect the session: %v", err)
	}
	t.Cleanup(session.Close)

	inbox := natsgo.NewInbox()
	own := bus.SessionReplySubject(inbox, "ses_one")
	others := []string{
		bus.SessionReplySubject(inbox, "ses_two"),
		inbox,
		inbox + ".rH8xg2hl",
		inbox + ".1",
		inbox + ".ses_one",
	}
	for _, subject := range append([]string{own}, others...) {
		if err := session.Publish(subject, []byte("done")); err != nil {
			t.Fatalf("publish to %s: %v", subject, err)
		}
	}
	if err := session.Flush(); err != nil {
		t.Fatalf("flush the session: %v", err)
	}
	// No span of its own: the delivery can come late on a loaded machine with nothing wrong.
	msg, err := heard.NextMsgWithContext(testwait.Context(t))
	if err != nil || msg.Subject != own {
		t.Fatalf("the watcher heard %v (%v), want the session's own reply subject %s", msg, err, own)
	}
	if extra, err := heard.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("the watcher heard a publish to %s, which the grant should refuse", extra.Subject)
	}
	got := testwait.Until(t, fmt.Sprintf("the server to refuse each of %v (the last read is the refusals)", others), func() ([]string, bool) {
		mu.Lock()
		got := slices.Clone(refused)
		mu.Unlock()
		missing := slices.DeleteFunc(slices.Clone(others), func(subject string) bool {
			return slices.ContainsFunc(got, func(text string) bool {
				return strings.Contains(text, `Permissions Violation for Publish to "`+subject+`"`)
			})
		})
		return got, len(missing) == 0
	})
	if len(got) != len(others) {
		t.Fatalf("the server refused %v; want exactly one refusal for each of %v", got, others)
	}
}
