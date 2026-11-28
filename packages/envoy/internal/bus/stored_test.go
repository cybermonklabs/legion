package bus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A stored-message read returns the first message at or after its sequence on a subject its filter
// matches, wildcards included, with the concrete subject, body and stored time; past the last one
// it is nats.ErrMsgNotFound, and with no stream nats.ErrStreamNotFound.
func TestNextStoredMessageReadsByFilterAfterASequence(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	publish := func(subject, body string) uint64 {
		t.Helper()
		ack, err := js.Publish(subject, []byte(body))
		if err != nil {
			t.Fatalf("publish on %s: %v", subject, err)
		}
		return ack.Sequence
	}
	publish("notifications.dispatch.issue.KEY-1.ask.opened", `{"n":1}`)
	publish("notifications.agent.ses_a", `{"n":2}`)
	third := publish("notifications.dispatch.issue.KEY-1.ask.answered", `{"n":3}`)
	// No span of its own: an answer can come late on a loaded machine with nothing wrong.
	ctx := testwait.Context(t)

	stored, err := client.NextStoredMessage(ctx, "notifications.dispatch.issue.KEY-1.>", 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored.Sequence != third || stored.Subject != "notifications.dispatch.issue.KEY-1.ask.answered" || string(stored.Data) != `{"n":3}` {
		t.Fatalf("stored = %+v, want seq %d on the answered subject with its body", stored, third)
	}
	if time.Since(stored.Time) > time.Minute || stored.Time.After(time.Now()) {
		t.Fatalf("stored time %s is not when it was stored", stored.Time)
	}

	if _, err := client.NextStoredMessage(ctx, "notifications.dispatch.issue.KEY-1.>", third+1); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("read past the last message = %v, want nats.ErrMsgNotFound", err)
	}

	if err := js.DeleteStream(Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	if _, err := client.NextStoredMessage(ctx, "notifications.agent.ses_a", 1); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("read without the stream = %v, want nats.ErrStreamNotFound", err)
	}
}

// probeHeader is the header the listener's connection probe carries (cmd/listener).
var probeHeader = nats.Header{"Envoy-Probe": []string{"1"}}

// requestCoreClient connects a bus client and a plain connection of its own to one test server,
// for the core requests the client makes and the subscriptions that answer or ignore them.
func requestCoreClient(t *testing.T) (*Client, *nats.Conn) {
	t.Helper()
	uri := testnats.URL(t)
	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	responder, err := Dial("request-core-responder", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect the responder: %v", err)
	}
	t.Cleanup(responder.Close)
	return client, responder
}

// The connection probe's request (RequestCore) carries its header and no body to the subject's
// subscriber, on the reply subject its caller names, and returns that subscriber's reply; a
// subject nobody subscribes is nats.ErrNoResponders, and one whose subscriber keeps silent waits
// out the caller's deadline and returns ctx's error. These are the three answers the send route
// reads a session's connection from: connected, stopped and connected.
func TestARequestCoreCarriesItsHeaderAndReadsTheThreeAnswers(t *testing.T) {
	t.Parallel()
	client, responder := requestCoreClient(t)
	heard := make(chan *nats.Msg, 1)
	if _, err := responder.Subscribe(DeliverSubject("ses_answers"), func(msg *nats.Msg) {
		heard <- msg
		_ = msg.Respond(nil)
	}); err != nil {
		t.Fatalf("subscribe the answering subject: %v", err)
	}
	if _, err := responder.SubscribeSync(DeliverSubject("ses_silent")); err != nil {
		t.Fatalf("subscribe the silent subject: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatalf("flush the subscriptions: %v", err)
	}

	// No span of its own: an answer can come late on a loaded machine with nothing wrong.
	ctx := testwait.Context(t)
	reply := SessionReplySubject(nats.NewInbox(), "ses_answers")
	if _, err := client.RequestCore(ctx, DeliverSubject("ses_answers"), reply, probeHeader); err != nil {
		t.Fatalf("request to a subscriber that answers = %v, want its reply", err)
	}
	select {
	case msg := <-heard:
		if msg.Header.Get("Envoy-Probe") != "1" || len(msg.Data) != 0 || msg.Reply != reply {
			t.Fatalf("the subscriber heard header %v, %d body bytes and reply %q, want Envoy-Probe: 1, no body and reply %q", msg.Header, len(msg.Data), msg.Reply, reply)
		}
	default:
		t.Fatal("the request was answered without reaching the subscriber")
	}

	if _, err := client.RequestCore(ctx, DeliverSubject("ses_nobody"), SessionReplySubject(nats.NewInbox(), "ses_nobody"), probeHeader); !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("request to a subject nobody subscribes = %v, want nats.ErrNoResponders", err)
	}

	// A window, not a wait: the silent subscriber must not answer, and 300 ms is how long the
	// request waits for it before its context ends it.
	silentCtx, silentCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer silentCancel()
	if _, err := client.RequestCore(silentCtx, DeliverSubject("ses_silent"), SessionReplySubject(nats.NewInbox(), "ses_silent"), probeHeader); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request to a subscriber that keeps silent = %v, want context.DeadlineExceeded", err)
	}
}

// The connection probe's request takes its reply over a channel of nats.RequestChanLen slots, not
// conn.SubscribeSync's, which nats.go sizes at nats.Options.SubChanLen (65,536 pointers, 512 KiB
// allocated and zeroed per request): the send route probes once per send, so a send would cost
// half a megabyte to hold one reply. requestConfirmed, which still subscribes that way, is
// measured beside it, so a measurement that could not see the channel fails here rather than
// passing the probe.
func TestARequestCoreAllocatesNoSubscriptionChannelOfTheDefaultSize(t *testing.T) {
	client, responder := requestCoreClient(t)
	subject := DeliverSubject("ses_allocation")
	if _, err := responder.Subscribe(subject, func(msg *nats.Msg) { _ = msg.Respond(nil) }); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	const requests = 64
	perRequest := func(request func(ctx context.Context) error) uint64 {
		t.Helper()
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
		ctx := testwait.Context(t)
		if err := request(ctx); err != nil {
			t.Fatalf("warm-up request: %v", err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range requests {
			if err := request(ctx); err != nil {
				t.Fatalf("request: %v", err)
			}
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / requests
	}
	probe := perRequest(func(ctx context.Context) error {
		_, err := client.RequestCore(ctx, subject, SessionReplySubject(nats.NewInbox(), "ses_allocation"), probeHeader)
		return err
	})
	confirmed := perRequest(func(ctx context.Context) error {
		_, err := client.requestConfirmed(ctx, subject, nil)
		return err
	})
	t.Logf("bytes allocated per request: RequestCore %d, requestConfirmed %d", probe, confirmed)
	if confirmed < 512<<10 {
		t.Fatalf("requestConfirmed allocated %d bytes per request, under its 512 KiB channel, so this measurement cannot see the channel", confirmed)
	}
	if probe > 64<<10 {
		t.Fatalf("RequestCore allocated %d bytes per request, want at most 64 KiB", probe)
	}
}

func TestSubjectContainsFollowsNATSWildcards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, subject string
		want             bool
	}{
		{"notifications.agent.>", "notifications.agent.ses_a", true},
		{"notifications.agent.>", "notifications.agent.ses_a.more", true},
		{"notifications.agent.>", "notifications.agent.*", true},
		{"notifications.agent.>", "notifications.agent.>", true},
		{"notifications.agent.>", "notifications.agent", false},
		{"notifications.agent.>", "notifications.>", false},
		{"notifications.dispatch.>", "notifications.dispatch.issue.KEY-1.>", true},
		{"notifications.dispatch.>", "notifications.team.x.y", false},
		{"notifications.*.x", "notifications.team.x", true},
		{"notifications.*.x", "notifications.*.x", true},
		{"notifications.*.x", "notifications.>", false},
		{"notifications.*.x", "notifications.team.x.y", false},
		{"notifications.team", "notifications.*", false},
		{"notifications.team", "notifications.team", true},
		{"notifications.team.x", "notifications.team", false},
		{">", "anything.at.all", true},
	} {
		if got := SubjectContains(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("SubjectContains(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func TestCheckFilterSubjectRefusesWhatNATSCannotFilterOn(t *testing.T) {
	t.Parallel()
	for subject, refused := range map[string]bool{
		"notifications.dispatch.issue.KEY-1.>": false,
		"notifications.*.ses_a":                false,
		"":                                     true,
		"notifications..agent":                 true,
		"notifications.agent.":                 true,
		"notifications.>.agent":                true,
		"notifications.agent.a b":              true,
	} {
		err := CheckFilterSubject(subject)
		if refused != (err != nil) {
			t.Errorf("CheckFilterSubject(%q) = %v, want refused %v", subject, err, refused)
		}
		if err != nil && !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("CheckFilterSubject(%q) = %v, want ErrInvalidSubject", subject, err)
		}
	}
}

// ControllerNoticeRole pairs one project's controller notice lane with the role token its holder
// claims, from the project token the lane names, so the listener never builds the two strings
// side by side. Only a literal lane of the family pairs: a wildcard, a bare prefix, an issue
// notice and a longer subject name no role.
func TestControllerNoticeRolePairsALaneWithItsRoleToken(t *testing.T) {
	t.Parallel()
	for topic, want := range map[string]string{
		"notifications.legion.ACME.controller":      "legion-ACME-controller",
		"notifications.legion.omp.controller":       "legion-omp-controller",
		"notifications.legion.ACME.LEGION-208":      "",
		"notifications.legion.*.controller":         "",
		"notifications.legion.>":                    "",
		"notifications.legion..controller":          "",
		"notifications.legion.controller":           "",
		"notifications.legion.ACME.x.controller":    "",
		"notifications.agent.ses_a":                 "",
		"notifications.role.legion-ACME-controller": "",
	} {
		role, lane := ControllerNoticeRole(topic)
		if role != want || lane != (want != "") {
			t.Errorf("ControllerNoticeRole(%q) = %q, %v; want %q, %v", topic, role, lane, want, want != "")
		}
	}
}

// What a session may register as an interest (/v1/interests/subscribe): anything but a topic
// reaching a private stream subject that is not its own: another session's agent subject by name
// or any wildcard over the agent subjects, a failed-delivery lane of an agent subject (its own
// included) or of a team topic, or a wildcard over either, a controller notice lane of a project
// whose controller role it does not hold, and wildcards reaching any of those. Its own agent
// subject is its own only when it is one literal subject, and so is the notice lane of the
// project whose controller role its claims name: the holder registers that lane because the
// notices reach it through its own consumer alone (INFRA-771), while another project's
// controller lane, and every other private subject, stay out of a controller's registration too.
// The cases are testdata/private-stream-subjects.json, which @legion/envoy-client's mirror of this
// rule runs too (stream-topics.test.ts): one file, read by both, so a case added for either is run
// by both on the next test run and neither can drift from a hand copy. Each case is a session id,
// a topic, the roles that session holds and the answer.
func TestReachesPrivateStreamSubjectBoundsInterests(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "private-stream-subjects.json"))
	if err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	var cases []struct {
		SessionID string   `json:"sessionID"`
		Topic     string   `json:"topic"`
		Roles     []string `json:"roles"`
		Want      bool     `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	// The file held 37 cases when this was written: a read that answers a handful of them, or
	// none, passes while covering nothing.
	if len(cases) < 37 {
		t.Fatalf("the shared cases hold %d rows, want at least 37", len(cases))
	}
	for _, tc := range cases {
		if got := ReachesPrivateStreamSubject(tc.SessionID, tc.Topic, tc.Roles); got != tc.Want {
			t.Errorf("ReachesPrivateStreamSubject(%q, %q, %v) = %v, want %v", tc.SessionID, tc.Topic, tc.Roles, got, tc.Want)
		}
	}
}

// Every private stream subject of the listener has a shared case of its own: a literal subject
// under it that the rule answers private. @legion/envoy-client's mirror runs the same file, so a
// private subject the listener adds without a case fails here, and one whose case the client's
// list does not reach fails there. Its client list once named itself the listener's own while it
// lacked the kept and resume lanes, and no case under either lane said so.
func TestEveryPrivateStreamSubjectHasALiteralSharedCase(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "private-stream-subjects.json"))
	if err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	var cases []struct {
		Topic string `json:"topic"`
		Want  bool   `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("read the shared cases: %v", err)
	}
	for _, private := range privateStreamSubjects {
		covered := false
		for _, tc := range cases {
			literal := !strings.ContainsAny(tc.Topic, "*>")
			if tc.Want && literal && SubjectContains(private, tc.Topic) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("testdata/private-stream-subjects.json holds no literal subject under %q that the rule answers private: add one, so the client's mirror is checked on it", private)
		}
	}
}
