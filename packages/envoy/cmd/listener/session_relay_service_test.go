// These rows start the real NATS auth service (internal/testnats/servicecallout); the
// server-change soak keeps them out of its build, as durable_inbox_test.go says.

//go:build !serverchange

package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testnats/servicecallout"
	"github.com/sjawhar/envoy/natsgrant"
)

// A listener the auth service admits by its platform token, which reads only its own replies,
// relays a frame to a session on the session's own grant and has its done acknowledge the message,
// and its connection probe is answered: each asks under the listener account's session reply
// inbox (natsgrant.SessionReplyInbox), the one the auth service grants it, which the session's
// grant answers. The control is the relay asking under nats.go's package inbox, as before, whose
// subscription the listener's grant refuses: the session's done never reaches it.
func TestAListenerOnItsServiceTokenRelaysToASessionOnItsOwnGrant(t *testing.T) {
	const account = "system:serviceaccount:example-ns:relay-listener"
	s1 := newSessionUser(t, "ses_service_relay_")
	grant, err := natsgrant.Session(s1.id)
	if err != nil {
		t.Fatal(err)
	}
	adminSeed, adminKey := testnats.User(t)
	uri := servicecallout.Start(t, "listener/relay-service/*="+account,
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
		testnats.StaticUser{NKey: s1.public, Publish: grant.Publish, Subscribe: grant.Subscribe})
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "relay-service",
		bus.ServiceTokenFileEnvVar+"="+servicecallout.TokenFile(t, account, 50*time.Minute))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})
	listener.waitHealthy(t)
	t.Setenv(bus.NkeySeedEnvVar, adminSeed)
	consumers := observeConsumers(t, uri)
	publisher := connectPublisher(t, uri)
	one := startSessionStandInOn(t, s1.connect(t, uri), s1.id, answerDone)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(s1.id, "epoch-1"))

	item := newEnvelope("agent", contracts.LegacyAgentSubject(s1.id), "for the session")
	publish(t, publisher, item)
	frame := one.collect(t, 1, 10*time.Second)[0]
	if inbox := natsgrant.SessionReplyInbox(account) + "."; !strings.HasPrefix(frame.msg.Reply, inbox) {
		t.Fatalf("the frame asks on %s, want under the listener's session reply inbox %s", frame.msg.Reply, inbox)
	}
	awaitAcked(t, consumers, s1.id, frame.seq)

	status, body := callListener(t, listener.port, http.MethodPost, "/v1/messages/send",
		fmt.Sprintf(`{"target_session":%q,"source":"agent","source_session":"ses_sender","message":"are you there?"}`, s1.id))
	if status != http.StatusOK || !strings.Contains(body, `"state":"`+sessionStateConnected+`"`) {
		t.Fatalf("the send answered %d %s, want 200 and state connected", status, body)
	}
	if reply := *one.probeReply.Load(); !strings.HasPrefix(reply, natsgrant.SessionReplyInbox(account)+".") {
		t.Fatalf("the probe asked on %s, want under the listener's session reply inbox", reply)
	}
	if err := one.conn.Flush(); err != nil {
		t.Fatalf("flush the session's connection: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := s1.refusals(); len(got) != 0 {
		t.Fatalf("the server refused the session publishes to %v, want none", got)
	}
}
