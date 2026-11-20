package daemon

// Step A10's two checks of the handoff-records proof (handoff_records_live_test.go, built with
// `-tags e2e`), and their table test. They carry no build tag, so `go test` of this package runs
// the table, and the live proof's build, which only `-test.run "^TestHandoffRecordsLive$"`
// drives, compiles the same checks.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// unansweredWriteText is how a dispatch CLI command begins its failure when its request to the
// Dispatch at base got no answer at all: the message sessionContext's fetch throws
// (packages/envoy-client/src/dispatch-commands/context.ts), naming the server the CLI was told
// of. The runtime's own account of the failure follows it.
func unansweredWriteText(base string) string {
	return "Dispatch at " + base + " is unreachable: "
}

// writeUnanswered is whether a `dispatch handoff-write` that exited code printing out failed
// because the Dispatch at base sent no answer.
func writeUnanswered(out string, code int, base string) bool {
	return code != 0 && strings.Contains(out, unansweredWriteText(base))
}

// unansweredReadText is how the daemon's refusal of a completion begins when its read of record
// id got no answer from Dispatch: internal/api/handoff.go's DISPATCH_READ_FAILED, carrying the
// error internal/dispatch's client gives a request that got no response (`call Dispatch GET
// <path>: `). A read that Dispatch answered with a 5xx is refused DISPATCH_READ_FAILED too, and
// carries that answer instead.
func unansweredReadText(id string) string {
	return "Dispatch did not answer the read of handoff record " + id + " (call Dispatch GET /api/v1/handoffs/" + id + ": "
}

// readUnanswered is whether the daemon refused a completion naming record id because its read of
// the record got no answer from Dispatch.
func readUnanswered(status int, failure api.Failure, id string) bool {
	return status == http.StatusBadGateway && failure.Code == "DISPATCH_READ_FAILED" && strings.HasPrefix(failure.Error, unansweredReadText(id))
}

// TestA10KnowsTheStoppedDispatchByThisRepositorysOwnWords holds A10's two checks to what each
// proves: the CLI's failure names the Dispatch that sent no answer, and the daemon's refusal is
// of a read of the named record that got no answer. The rows hold what the CLI and the daemon
// print, read from this proof's runs and from internal/api/handoff.go: the CLI against a stand-in
// that closes each connection, against a port nothing serves and against a server that answers
// 503, and the daemon after a read that got no answer and after one answered 503. A row that
// changes the address, the record, the exit code or the text says so in its name. A check that
// matched a word of the runtime's account (the "connection" of Bun's `ECONNRESET: The socket
// connection was closed unexpectedly`) accepted the runtime's words alone, another Dispatch, a
// Dispatch that answered, and another record; each of those is refused here.
func TestA10KnowsTheStoppedDispatchByThisRepositorysOwnWords(t *testing.T) {
	const base = "http://127.0.0.1:38503"
	const id = "76f21f17-0217-40ec-b915-6ca5ae7a0d98"
	const other = "19ae0d78-2ecf-4076-befc-795307537a97"
	const advice = ". If the Dispatch URL changed, restart this agent process so it picks up the new configuration.\n"
	const reset = "ECONNRESET: The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()"
	const refused = "ECONNREFUSED: Unable to connect. Is the computer able to access the url?"
	for _, c := range []struct {
		name string
		out  string
		code int
		want bool
	}{
		{"the stand-in on the stopped Dispatch's port closed the connection", "Dispatch at " + base + " is unreachable: " + reset + advice, 1, true},
		{"nothing serves the stopped Dispatch's port", "Dispatch at " + base + " is unreachable: " + refused + advice, 1, true},
		{"the runtime's words alone, naming no Dispatch", reset + ".\n", 1, false},
		{"another Dispatch sent no answer", "Dispatch at http://127.0.0.1:1 is unreachable: " + refused + advice, 1, false},
		{"the stopped Dispatch's port answered 503", "HTTP_503: POST " + base + "/api/v1/issues/HRD9CF34-5/handoffs/plan answered 503 Service Unavailable with a body that is not Dispatch's error JSON (\"down\"), which looks like a proxy or gateway page rather than Dispatch's own answer, so the write may or may not have reached Dispatch: check whether it took effect before retrying it.\n", 1, false},
		{"the write exited 0", "Dispatch at " + base + " is unreachable: " + reset + advice, 0, false},
	} {
		if got := writeUnanswered(c.out, c.code, base); got != c.want {
			t.Errorf("handoff-write %s: writeUnanswered(%q, %d) = %v, want %v", c.name, c.out, c.code, got, c.want)
		}
	}
	const retry = "): complete again once Dispatch answers; this completion changed nothing"
	for _, c := range []struct {
		name    string
		status  int
		failure api.Failure
		want    bool
	}{
		{"the read got no answer", http.StatusBadGateway, api.Failure{Code: "DISPATCH_READ_FAILED", Error: "Dispatch did not answer the read of handoff record " + id + " (call Dispatch GET /api/v1/handoffs/" + id + ": Get \"" + base + "/api/v1/handoffs/" + id + "\": EOF" + retry}, true},
		{"Dispatch answered the read 503", http.StatusBadGateway, api.Failure{Code: "DISPATCH_READ_FAILED", Error: "Dispatch did not answer the read of handoff record " + id + " (down" + retry}, false},
		{"the read of another record got no answer", http.StatusBadGateway, api.Failure{Code: "DISPATCH_READ_FAILED", Error: "Dispatch did not answer the read of handoff record " + other + " (call Dispatch GET /api/v1/handoffs/" + other + ": Get \"" + base + "/api/v1/handoffs/" + other + "\": EOF" + retry}, false},
		{"Dispatch has no such record", http.StatusNotFound, api.Failure{Code: "HANDOFF_RECORD_NOT_FOUND", Error: "Dispatch has no handoff record " + id + " (HANDOFF_NOT_FOUND: handoff " + id + " not found): name the record `dispatch handoff-read --issue HRD9CF34-5 --phase plan` shows, or write one with `dispatch handoff-write --issue HRD9CF34-5 --phase plan --body-file <file>`, then complete again; this completion changed nothing"}, false},
	} {
		if got := readUnanswered(c.status, c.failure, id); got != c.want {
			t.Errorf("the completion when %s: readUnanswered(%d, %+v) = %v, want %v", c.name, c.status, c.failure, got, c.want)
		}
	}
}
