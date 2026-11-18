package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// roleRevisionCreateUnanswered is the WARN the registry's open logs for each attempt of its
// role-revision scan whose consumer create the server left unanswered (internal/store's
// roleRevisions).
const roleRevisionCreateUnanswered = "role revisions: the scan's consumer create drew no answer; the open makes the scan again and waits for it"

// A listener whose NATS server leaves the registry open's role-revision scan unanswered, as a
// server on a loaded machine answers late, waits for the server rather than ending its start: past
// the 10 s the open once gave the scan it is still running, has logged a WARN for each create that
// went unanswered, and answers /healthz 200 "starting", neither ready nor down. Once the server
// answers, the listener restores its role claims and reports healthy, having logged no ERROR. A
// start that gave up at the 10 s exited 1 with "role revisions: create the scan's consumer: context
// deadline exceeded (waited 10s)", which ended INFRA-4240's starvation check. The listener reaches
// the server through a relay that holds back from the server every consumer create of the role
// bucket's stream until the row lets them through (requestHoldingRelay), so the server never reads
// one and each runs out of nats.go's 5 s wait unanswered; the row holds three, 15 s.
func TestAListenerWhoseRoleRevisionScanTheServerHoldsAnswersStartingAndStartsOnceAnswered(t *testing.T) {
	relay := startRequestHoldingRelay(t, testnats.URL(t), "$JS.API.CONSUMER.CREATE.KV_"+bus.RoleClaimBucket)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), relay.url, "role-scan-held")
	const unanswered = 3
	warnings := func() int {
		return countRecords(listener, roleRevisionCreateUnanswered, func(record map[string]any) bool { return record["level"] == "WARN" })
	}
	testwait.Until(t, fmt.Sprintf("the listener to log %d role-revision scans the server left unanswered", unanswered), func() (string, bool) {
		if warnings() >= unanswered {
			return "", true
		}
		select {
		case <-listener.Exited:
			// Exited closes only once the output is whole, so this read is final.
			output := listener.Output.String()
			t.Fatalf("the listener exited with %s while the server held back its role-revision scan's consumer create, after %d WARNs of %d; its last line was %q:\n%s",
				listener.Cmd.ProcessState, warnings(), unanswered, lastOutputLine(output), output)
		default:
		}
		return fmt.Sprintf("%d WARNs; the listener's last line was %q", warnings(), lastOutputLine(listener.Output.String())), false
	})
	status, answer := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/healthz"))
	if status != http.StatusOK || !strings.Contains(answer, `"status":"starting"`) {
		t.Fatalf("/healthz while the server holds back the role-revision scan: status %d %q, want 200 starting\n%s", status, answer, listener.Output.String())
	}
	if held := relay.held.Load(); held < unanswered {
		t.Fatalf("the relay held back %d consumer creates of the role bucket, want at least %d: the WARNs were not the held creates'", held, unanswered)
	}

	relay.release()
	listener.waitHealthy(t)
	listener.WaitForOutput(t, "restored role claims")
	for line := range strings.SplitSeq(listener.Output.String(), "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a start whose role-revision scan the server answered late logged an error: %s", line)
		}
	}
}

// A listener whose NATS server refuses the registry open's role-revision scan, its consumer create
// answered with an error, ends its start: it exits 1, its last line the server's refusal named
// after the request and naming no wait, since nothing waited, and it logs no WARN that the create
// went unanswered. The server refuses the create because the role bucket's stream already holds as
// many consumers as its max_consumers allows: one, a durable the row creates.
func TestAListenerWhoseRoleRevisionScanTheServerRefusesExitsNamingTheRefusal(t *testing.T) {
	uri := testnats.URL(t)
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	stream := "KV_" + bus.RoleClaimBucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	capped := info.Config
	capped.MaxConsumers = 1
	if _, err := js.UpdateStream(&capped); err != nil {
		t.Fatalf("cap %s at one consumer: %v", stream, err)
	}
	if _, err := js.AddConsumer(stream, &natsgo.ConsumerConfig{Durable: "filler", AckPolicy: natsgo.AckExplicitPolicy}); err != nil {
		t.Fatalf("create the consumer that fills %s to its cap: %v", stream, err)
	}

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "role-scan-refused")
	listener.WaitExit(t, "the server refused its role-revision scan")
	output := listener.Output.String()
	if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code = %d, want 1:\n%s", code, output)
	}
	last := lastOutputLine(output)
	if !strings.Contains(last, "role revisions: create the scan's consumer: ") || !strings.Contains(last, "maximum consumers limit reached") || strings.Contains(last, "waited") {
		t.Fatalf("the listener's last line was %q, want the server's refusal of the role-revision scan named after the request, with no wait:\n%s", last, output)
	}
	if n := countRecords(listener, roleRevisionCreateUnanswered, func(map[string]any) bool { return true }); n != 0 {
		t.Fatalf("the listener logged %d WARNs that the refused create went unanswered, want none:\n%s", n, output)
	}
}

// requestHoldingRelay relays every NATS connection made to url on to the server, and holds back
// from the server, until release, each request a client publishes to a subject that starts with the
// prefix it was started with: it drops the request, so the server never reads it and the client's
// own wait for its answer runs out, as it does against a server too loaded to answer. Everything
// else passes both ways as it came. held counts the requests it held back.
type requestHoldingRelay struct {
	url      string
	held     atomic.Int32
	released atomic.Bool
}

// startRequestHoldingRelay starts the relay to the server at server, a NATS URL whose credentials
// the relay's own URL keeps, holding back the requests to subjects that start with prefix.
func startRequestHoldingRelay(t *testing.T, server, prefix string) *requestHoldingRelay {
	t.Helper()
	target, err := url.Parse(server)
	if err != nil {
		t.Fatalf("read the NATS URL %q: %v", server, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	relayed := *target
	relayed.Host = listener.Addr().String()
	r := &requestHoldingRelay{url: relayed.String()}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", target.Host)
			if err != nil {
				_ = client.Close()
				continue
			}
			go func() {
				defer func() { _ = upstream.Close() }()
				r.forward(upstream, client, prefix)
			}()
			go func() {
				defer func() { _ = client.Close() }()
				_, _ = io.Copy(client, upstream)
			}()
		}
	}()
	return r
}

// release lets every request sent from now on through to the server.
func (r *requestHoldingRelay) release() { r.released.Store(true) }

// forward copies what a client sends to the server one protocol operation at a time, and drops each
// PUB or HPUB to a subject that starts with prefix while the relay holds them back. The last field
// of either operation's line is the size of the payload that follows it, its trailing CRLF aside.
func (r *requestHoldingRelay) forward(to io.Writer, from io.Reader, prefix string) {
	reader := bufio.NewReader(from)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		operation := line
		if fields := strings.Fields(string(line)); len(fields) >= 3 && (strings.EqualFold(fields[0], "PUB") || strings.EqualFold(fields[0], "HPUB")) {
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil {
				return
			}
			payload := make([]byte, size+len("\r\n"))
			if _, err := io.ReadFull(reader, payload); err != nil {
				return
			}
			if strings.HasPrefix(fields[1], prefix) && !r.released.Load() {
				r.held.Add(1)
				continue
			}
			operation = append(line, payload...)
		}
		if _, err := to.Write(operation); err != nil {
			return
		}
	}
}
