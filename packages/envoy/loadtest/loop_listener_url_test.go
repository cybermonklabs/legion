package main

import (
	"bytes"
	"context"
	"log"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestALoopListenerThatIsNoURLIsRefusedWithNoneOfIt: a -loop-listeners endpoint url.Parse
// refuses (an invalid escape in its path), with redacttest's credentials in its user information
// and its query, is refused by its place in the list, with none of the value (refused), where the
// refusal quoted the value whole; the twin shows url.Parse's reading of it quotes the
// credentials. Captures loadtest.loop-listener-url/refused.
func TestALoopListenerThatIsNoURLIsRefusedWithNoneOfIt(t *testing.T) {
	value := "http://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/%zz?access" + "_token=" + redacttest.Token
	t.Run("twin", func(t *testing.T) {
		if _, err := url.Parse(value); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("url.Parse = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		_, err := parseLoopListeners("http://127.0.0.1:2," + value)
		if err == nil {
			t.Fatal("parseLoopListeners took an endpoint that is no URL")
		}
		redacttest.RequireNoPiece(t, "loadtest.loop-listener-url/refused", err.Error())
		if want := "-loop-listeners endpoint 2 is not a URL with a scheme and a host (it is not printed)"; err.Error() != want {
			t.Errorf("parseLoopListeners = %q; want %q", err, want)
		}
	})
}

// TestTheLoopDriversLineNamesItsListenerWithNoCredential: the step's line about the loop's driver
// session names the -loop-listeners endpoint it registered on without its user information and
// its query (redactURL), as every line of the load test names an endpoint, where it
// printed the endpoint as it was given. Captures loadtest.loop-listener-url/driver-line.
func TestTheLoopDriversLineNamesItsListenerWithNoCredential(t *testing.T) {
	fl := startFakeListener(t)
	_, natsURL := testnats.Start(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	driver := strings.Replace(fl.server.URL, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1) + "/?password=" + redacttest.Token
	var logged bytes.Buffer
	st := &step{
		cfg: runConfig{
			loop: loopTestSeed(t), loopListeners: []string{fl.server.URL, driver},
			unitStarts: 1, unitLife: time.Minute, driverTopics: 1, receiptDeadline: time.Second, rngSeed: 1,
			ep: endpoints{listenerToken: "listener-token"},
		},
		lg: log.New(&logged, "", 0), nonce: 7,
	}
	ctx, cancel := context.WithCancel(context.Background())
	lp := st.startLoopSessions(ctx, &natsSessions{conns: []*nats.Conn{nc}, receipts: newCollector[receipt](16)})
	cancel()
	lp.stop()
	line := logged.String()
	if !strings.Contains(line, "loop sessions: driver on ") {
		t.Fatalf("the step logged %q; want the driver's line", line)
	}
	redacttest.RequireNoPiece(t, "loadtest.loop-listener-url/driver-line", line)
	if want := "driver on " + fl.server.URL + "/ following"; !strings.Contains(line, want) {
		t.Errorf("the driver's line is %q; want %q in it", line, want)
	}
}
