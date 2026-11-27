// packages/envoy/internal/cipher/proxy/enrollment_test.go
//
// The rows about every live grant of one enrollment: a command holds the placeholders of each
// grant its session has, under one agent-secrets run nested in another or a role's own grant
// beside one agent-secrets asked for, and its proxy password is the placeholder of one of them.
// So a host any live grant of the enrollment binds is read (serveConnect), a request is handled
// for the grant whose placeholder it carries (serveBound), and a call carried for a grant other
// than the connection's own is held to that grant as well (watchCarried).
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// A second grant of the rig's enrollment, and one grant of another enrollment, each holding one
// stored secret of its own.
const (
	peerName          = "SECOND_TOKEN"
	peerValue         = "the-real-second-token"
	peerGrantID       = "grant-00000002"
	foreignName       = "FOREIGN_TOKEN"
	foreignValue      = "the-real-foreign-token"
	foreignGrantID    = "grant-00000003"
	foreignEnrollment = "enrollment-00000003"
	// chatHost is the host the connection's own grant binds in the rows where it binds no
	// host the call goes to, as a Slack grant of a role binds slack.com and not GitHub.
	chatHost = "chat.example.net"
	chatName = "CHAT_TOKEN"
)

var (
	peerPlaceholder    = placeholder.Prefix + "secondsecondsecondsecondsecond02"
	foreignPlaceholder = placeholder.Prefix + "foreignforeignforeignforeignfor3"
	chatPlaceholder    = placeholder.Prefix + "chatchatchatchatchatchatchatcha4"
)

// addGrant is one more live grant the rig's broker holds: placeholder opens it, it holds name's
// value slot alone, and its values are value.
func (r *rig) addGrant(placeholder, grantID, enrollmentID, name, value string) {
	r.grants.setConnection(placeholder, requests.AirlockConnection{
		GrantID: grantID, EnrollmentID: enrollmentID, RequestID: "request-" + grantID, Issue: testIssue,
		SessionID: testSession, RuntimeID: testRuntime,
		Names: map[string]requests.AirlockGrantName{name: {Slots: map[string]string{requests.SlotValue: placeholder}}},
	})
	r.grants.setValues(grantID, storedValues(name, value))
}

// ProxyGrantsOfEnrollment is the broker's read of the live grants of one enrollment that hold a
// placeholder of one of names: each grant a placeholder the row wrote opens, of that enrollment,
// in grant id order, as the broker's read answers them. A grant any of whose placeholders the row
// refused is ended, as the broker's own row is for every placeholder of it, so it is left out. An
// empty enrollment id is refused, as the store refuses it (its enrollment_id column is a uuid):
// that is a minted connection's, which the proxy must never ask about.
func (f *fakeGrants) ProxyGrantsOfEnrollment(_ context.Context, enrollmentID, exceptGrantID string, names []string) ([]requests.AirlockConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollmentCalls++
	if f.enrollmentErr != nil {
		return nil, f.enrollmentErr
	}
	if enrollmentID == "" {
		return nil, errors.New(`cannot parse "" as a uuid enrollment id`)
	}
	ended := map[string]bool{}
	for placeholder := range f.refusals {
		if conn, ok := f.connections[placeholder]; ok {
			ended[conn.GrantID] = true
		}
	}
	seen := map[string]bool{}
	var live []requests.AirlockConnection
	for _, placeholder := range slices.Sorted(maps.Keys(f.connections)) {
		conn := f.connections[placeholder]
		if conn.EnrollmentID != enrollmentID || conn.GrantID == exceptGrantID || ended[conn.GrantID] || seen[conn.GrantID] {
			continue
		}
		if !slices.ContainsFunc(names, func(name string) bool { _, held := conn.Names[name]; return held }) {
			continue
		}
		if err := f.grantErrs[conn.GrantID]; err != nil {
			return nil, err
		}
		seen[conn.GrantID] = true
		live = append(live, conn)
	}
	slices.SortFunc(live, func(a, b requests.AirlockConnection) int { return strings.Compare(a.GrantID, b.GrantID) })
	return live, nil
}

// ProxyPlaceholderGrant is the grant a placeholder the row wrote stands for, live or ended: the
// password of a connection the row wrote or any slot of its grant, and "" for a placeholder the
// row never wrote, as the broker's own read answers before its sweep deletes an ended grant's
// rows.
func (f *fakeGrants) ProxyPlaceholderGrant(_ context.Context, placeholder string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, conn := range f.connections {
		for _, held := range conn.Names {
			if slices.Contains(slices.Collect(maps.Values(held.Slots)), placeholder) {
				return conn.GrantID, nil
			}
		}
	}
	return "", nil
}

// enrollmentReads is how often every live grant of an enrollment has been read.
func (f *fakeGrants) enrollmentReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enrollmentCalls
}

// failEnrollmentReads is what every read of an enrollment's live grants answers from here on.
func (f *fakeGrants) failEnrollmentReads(why error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollmentErr = why
}

// failGrantCheck is what a read of the enrollment's grants answers from here on whenever it reaches
// grant grantID: that grant's own check failing.
func (f *fakeGrants) failGrantCheck(grantID string, why error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantErrs == nil {
		f.grantErrs = map[string]error{}
	}
	f.grantErrs[grantID] = why
}

// enrollmentRules binds the rig's own name, the second grant's and the foreign grant's to the
// bound host, each in a header of its own so one request can carry two of them, and chatName to
// chatHost alone.
func enrollmentRules() proxyrules.Rules {
	return proxyrules.Rules{
		Kinds: []proxyrules.Kind{
			swapKind("worker", []string{boundHost}, "authorization"),
			swapKind("peer", []string{boundHost}, "x-second"),
			swapKind("foreign", []string{boundHost, tunnelHost}, "x-foreign"),
			swapKind("chat", []string{chatHost}, "authorization"),
		},
		Names: []proxyrules.Binding{
			swapBinding(workerName, "worker"),
			swapBinding(peerName, "peer"),
			swapBinding(foreignName, "foreign"),
			swapBinding(chatName, "chat"),
		},
	}
}

// TestARequestIsHandledForTheLiveGrantOfTheEnrollmentWhosePlaceholderItCarries: a connection is
// opened with the placeholder of one grant, and the command holds another grant of the same
// session beside it - a mail client's write placeholder on the connection of its read grant. A
// request that carries the second grant's placeholder and none of the first's is handled for the
// second: its value is swapped in, and the line names its grant. A request carrying the own
// grant's placeholder on the same connection is handled for the own grant. The controls: a
// placeholder of a grant of another enrollment, and of the second grant once it has ended, are
// swapped by no grant and reach no service (every such case is a row of
// TestARequestThatStillCarriesAPlaceholderAfterTheSwapIsRefused).
func TestARequestIsHandledForTheLiveGrantOfTheEnrollmentWhosePlaceholderItCarries(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{})
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	r.addGrant(foreignPlaceholder, foreignGrantID, foreignEnrollment, foreignName, foreignValue)
	client := r.client(workerPlaceholder)

	r.get(client, "https://"+boundHost+"/v1/second", "X-Second", "Token "+peerPlaceholder)
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want the second grant's one", len(seen))
	}
	if got, want := seen[0].Header.Get("X-Second"), "Token "+peerValue; got != want {
		t.Fatalf("the service saw X-Second %q; want %q: the request carried the second grant's placeholder on the first grant's connection", got, want)
	}
	line := r.logs.callsAtLeast(t, 1)[0]
	if line.fields["grant"] != peerGrantID || line.fields["secret"] != peerName || line.fields["enrollment"] != testEnrollID {
		t.Fatalf("the call logged %v; want grant %s, secret %s, enrollment %s", line.fields, peerGrantID, peerName, testEnrollID)
	}

	r.get(client, "https://"+boundHost+"/v1/own", "Authorization", "Bearer "+workerPlaceholder)
	seen = r.requests()
	if len(seen) != 2 {
		t.Fatalf("the service saw %d requests; want two", len(seen))
	}
	if got := seen[1].Header.Get("Authorization"); got != "Bearer "+workerValue {
		t.Errorf("the service saw Authorization %q; want the connection's own grant's value", got)
	}
	if grant := r.logs.callsAtLeast(t, 2)[1].fields["grant"]; grant != testGrantID {
		t.Errorf("the call of the own grant's placeholder logged grant %v; want %s", grant, testGrantID)
	}

	r.get(client, "https://"+boundHost+"/v1/foreign", "X-Foreign", "Token "+foreignPlaceholder)
	r.grants.refuseConnection(peerPlaceholder, requests.ErrGrantNotLive)
	r.get(client, "https://"+boundHost+"/v1/ended", "X-Second", "Token "+peerPlaceholder)
	if seen := r.requests(); len(seen) != 2 {
		t.Fatalf("the service saw %d requests; want the two above alone: another enrollment's placeholder and an ended grant's are swapped by no grant of this session", len(seen))
	}
}

// TestAConnectToAHostOnlyAnotherGrantOfTheEnrollmentBindsIsRead: a role's own grant binds GitHub,
// and inside agent-secrets for another secret the command's proxy password is that secret's
// placeholder, whose grant binds no GitHub host. The host is still read, since a live grant of the
// session binds it, and the role's placeholder is swapped. The service's own certificate is in the
// command's trust here, so a tunnel would reach it too and show the placeholder unswapped. The
// controls: a host only another enrollment's grant binds, and a host no grant binds, are tunnels.
func TestAConnectToAHostOnlyAnotherGrantOfTheEnrollmentBindsIsRead(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{})
	r.grants.connections = map[string]requests.AirlockConnection{}
	r.addGrant(chatPlaceholder, testGrantID, testEnrollID, chatName, "the-real-chat-token")
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	r.addGrant(foreignPlaceholder, foreignGrantID, foreignEnrollment, foreignName, foreignValue)
	r.trust.AddCert(r.service.Authority().Certificate())

	answer, body := r.get(r.client(chatPlaceholder), "https://"+boundHost+"/v1/repos", "X-Second", "Token "+peerPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the call answered %d %q; want 200", answer.StatusCode, body)
	}
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want one", len(seen))
	}
	if got, want := seen[0].Header.Get("X-Second"), "Token "+peerValue; got != want {
		t.Fatalf("the service saw X-Second %q; want %q: a live grant of the session binds the host, so the CONNECT is read and the placeholder swapped", got, want)
	}
	if tunnels := r.logs.tunnels(); len(tunnels) != 0 {
		t.Fatalf("the proxy logged %d tunnel(s); want none to a host a grant of the session binds", len(tunnels))
	}
	if grant := r.logs.callsAtLeast(t, 1)[0].fields["grant"]; grant != peerGrantID {
		t.Errorf("the call logged grant %v; want %s, whose placeholder it carried", grant, peerGrantID)
	}

	for _, host := range []string{tunnelHost, echoHost} {
		// tunnelHost is bound by the foreign grant alone, echoHost by no grant. Each is a
		// service that writes back what it is sent, so what comes back is what reached it: the
		// bytes the command sent, a placeholder among them, as it sent them.
		answer, conn, _ := r.connectRaw(net.JoinHostPort(host, "443"), basic(chatPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT to %s answered %d; want 200 Connection established", host, answer.StatusCode)
		}
		stream := net.Conn(conn)
		if host == tunnelHost {
			client := tls.Client(conn, &tls.Config{ServerName: tunnelHost, RootCAs: r.trust})
			if err := client.Handshake(); err != nil {
				t.Fatalf("the handshake through the CONNECT to %s: %v", host, err)
			}
			if err := client.ConnectionState().PeerCertificates[0].CheckSignatureFrom(r.authority.Certificate()); err == nil {
				t.Fatalf("%s was answered a certificate of the proxy's own: a grant of another enrollment opened it", host)
			}
			stream = client
		}
		sent := "Authorization: Token " + foreignPlaceholder + "\n"
		if _, err := io.WriteString(stream, sent); err != nil {
			t.Fatalf("write through the tunnel to %s: %v", host, err)
		}
		got := make([]byte, len(sent))
		if _, err := io.ReadFull(stream, got); err != nil || string(got) != sent {
			t.Fatalf("the tunnel to %s carried back %q (%v); want the bytes the command sent, placeholder included, %q", host, got, err, sent)
		}
		conn.Close()
	}
	if tunnels := r.logs.tunnelsAtLeast(t, 2); len(tunnels) != 2 {
		t.Fatalf("the proxy logged %d tunnels; want one for each host no grant of the session binds", len(tunnels))
	}
}

// TestPlainHTTPToAHostOnlyAnotherGrantOfTheEnrollmentBindsIsRefused: what is bound is what any
// live grant of the session binds, for plain HTTP as for a CONNECT, so a request in the clear to
// such a host is refused as one to a host the connection's own grant binds is.
func TestPlainHTTPToAHostOnlyAnotherGrantOfTheEnrollmentBindsIsRefused(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{})
	r.grants.connections = map[string]requests.AirlockConnection{}
	r.addGrant(chatPlaceholder, testGrantID, testEnrollID, chatName, "the-real-chat-token")
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	answer, body := r.get(r.client(chatPlaceholder), "http://"+boundHost+"/v1/things", "X-Second", "Token "+peerPlaceholder)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "plain http") {
		t.Fatalf("plain http to a host the session's second grant binds answered %d %q; want 403 refusing plain http", answer.StatusCode, body)
	}
	if r.dials.Load() != 0 {
		t.Fatalf("the proxy dialled %d time(s) for a refused plain-http request; want none", r.dials.Load())
	}
}

// TestARequestOfTheConnectionsOwnGrantReadsNoOtherGrant: a request carrying a placeholder of the
// connection's own grant is handled as before and costs no read of the enrollment's other grants;
// only a request that carries none of them reads those, and a read of them that did not happen is
// refused 503 in the broker's own words rather than forwarded.
func TestARequestOfTheConnectionsOwnGrantReadsNoOtherGrant(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{})
	client := r.client(workerPlaceholder)
	r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if reads := r.grants.enrollmentReads(); reads != 0 {
		t.Fatalf("a CONNECT and a request of the connection's own grant read the enrollment's grants %d time(s); want none", reads)
	}
	r.grants.failEnrollmentReads(databaseDown)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things", "X-Second", "Token "+peerPlaceholder)
	if answer.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a request whose grant could not be read answered %d %q; want 503", answer.StatusCode, body)
	}
	for _, word := range []string{"user=broker", "10.20.0.7", "db.internal"} {
		if strings.Contains(body, word) {
			t.Fatalf("the answer %q carries the store's own words", body)
		}
	}
	if seen := r.requests(); len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want the first alone", len(seen))
	}
}

// TestACallCarriedForAnotherGrantOfTheEnrollmentEndsWithThatGrant: a streamed call handled for
// the second grant on the first grant's connection is held to the second grant, whose value it
// carries: once that grant ends, the watcher cuts the call. The named revert:
// proxy-carry-reads-connection-grant-only, the watcher reading the connection's own grant alone,
// which holds no name of the call and so cuts it at once, while the grant it is handled for
// still lives.
func TestACallCarriedForAnotherGrantOfTheEnrollmentEndsWithThatGrant(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{Watch: watchFast})
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	ended := streamStandIn(r)
	client := r.client(workerPlaceholder)
	client.Timeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), 2*cutWait)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+boundHost+"/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Second", "Token "+peerPlaceholder)
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("the streamed call through the proxy: %v", err)
	}
	defer answer.Body.Close()
	reader := bufio.NewReader(answer.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "a line of the stream\n" {
		t.Fatalf("the stream's first line is %q (%v), want the service's", line, err)
	}
	if got := r.requests()[0].Header.Get("X-Second"); got != "Token "+peerValue {
		t.Fatalf("the service received X-Second %q, want the second grant's value", got)
	}
	// While the second grant lives the call goes on, for many watch intervals: the watcher reads
	// the grant the call is handled for, and the connection's own grant holds no such name.
	for deadline := time.Now().Add(10 * watchFast); time.Now().Before(deadline); {
		if line, err := reader.ReadString('\n'); err != nil || line != "a line of the stream\n" {
			t.Fatalf("the stream read %q (%v) while its grant lives; want it carried on", line, err)
		}
	}
	if cuts := r.logs.cuts(); len(cuts) != 0 {
		t.Fatalf("the proxy cut the call while its grant lives: %v", cuts[0].fields)
	}

	r.grants.refuseConnection(peerPlaceholder, requests.ErrGrantNotLive)
	cut := time.Now()
	_, err = io.Copy(io.Discard, reader)
	if took := time.Since(cut); err == nil || errors.Is(err, context.DeadlineExceeded) || took > cutWait {
		t.Fatalf("the stream went on %v after its grant ended and then ended with %v; want it cut within %v, the watch interval being %v", took, err, cutWait, watchFast)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the service's answer ended with %v, want it cancelled", err)
		}
	case <-time.After(cutWait):
		t.Fatal("the service went on streaming after the proxy cut the command's side")
	}
	cuts := r.logs.cuts()
	if len(cuts) != 1 {
		t.Fatalf("the proxy logged %d lines for the cut, want 1", len(cuts))
	}
	if got := cuts[0].fields["grant"]; got != peerGrantID {
		t.Errorf("the cut logged grant %v, want %s", got, peerGrantID)
	}
}

// TestARequestThatStillCarriesAPlaceholderAfterTheSwapIsRefused: a placeholder is the password of
// its grant's proxy connections, so one that would leave the proxy is refused rather than sent to
// the service. Each row is one request on a connection opened with the live grant's placeholder,
// and after the swap it still carries a placeholder: an ended grant's, of the connection's own
// name and of another; another enrollment's live grant's; the connection's own grant's outside
// the header its entry names, in another header, the path or the query; a second live grant's
// beside the own grant's; one inside a Basic credential, as git sends it; and one no grant holds.
// Each is refused 403 PLACEHOLDER_NOT_SWAPPED, the service sees nothing, and the refusal line
// names the grant the placeholder was minted for when the broker holds one. The controls, on the
// same connection after the rows: the own grant's and the second live grant's placeholders, each
// in the header its entry names, are swapped and sent. The named revert:
// proxy-placeholder-left-forwarded, which sends each row's request on with its placeholder.
func TestARequestThatStillCarriesAPlaceholderAfterTheSwapIsRefused(t *testing.T) {
	const (
		endedSameGrant  = "grant-00000005"
		endedOtherGrant = "grant-00000006"
	)
	endedSame := placeholder.Prefix + "endedsameendedsameendedsameended"
	endedOther := placeholder.Prefix + "endedotherendedotherendedotheren"
	unknown := placeholder.Prefix + "unknownunknownunknownunknownunkn"
	// A minted capability's placeholder (placeholder.MintedPrefix), which its own host's dpop
	// path swaps and no other request's.
	minted := placeholder.MintedPrefix("dispatch") + "mintedmintedmintedmintedmintedmi"
	r := newRuledRig(t, enrollmentRules(), Options{})
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	r.addGrant(foreignPlaceholder, foreignGrantID, foreignEnrollment, foreignName, foreignValue)
	r.addGrant(endedSame, endedSameGrant, testEnrollID, workerName, "the-real-worker-token-of-the-ended-grant")
	r.addGrant(endedOther, endedOtherGrant, testEnrollID, peerName, "the-real-second-token-of-the-ended-grant")
	r.grants.refuseConnection(endedSame, requests.ErrGrantNotLive)
	r.grants.refuseConnection(endedOther, requests.ErrGrantNotLive)
	client := r.client(workerPlaceholder)

	for _, row := range []struct {
		name, path string
		headers    []string
		grant      string
	}{
		{"an ended grant's placeholder of the connection's own name", "/v1/things",
			[]string{"Authorization", "Bearer " + endedSame}, endedSameGrant},
		{"an ended grant's placeholder of another name", "/v1/things",
			[]string{"X-Second", "Token " + endedOther}, endedOtherGrant},
		{"an ended grant's placeholder inside a Basic credential", "/v1/things",
			[]string{"Authorization", gitBasic(endedSame)}, endedSameGrant},
		{"another enrollment's live grant's placeholder", "/v1/things",
			[]string{"X-Foreign", "Token " + foreignPlaceholder}, foreignGrantID},
		{"the own grant's placeholder in a header its entry does not name", "/v1/things",
			[]string{"Authorization", "Bearer " + workerPlaceholder, "X-Other", "carried: " + workerPlaceholder}, testGrantID},
		{"the own grant's placeholder in the query", "/v1/things?token=" + workerPlaceholder,
			[]string{"Authorization", "Bearer " + workerPlaceholder}, testGrantID},
		{"the own grant's placeholder in the path", "/v1/things/" + workerPlaceholder,
			[]string{"Authorization", "Bearer " + workerPlaceholder}, testGrantID},
		{"a second live grant's placeholder beside the own grant's", "/v1/things",
			[]string{"Authorization", "Bearer " + workerPlaceholder, "X-Second", "Token " + peerPlaceholder}, peerGrantID},
		{"a placeholder no grant holds", "/v1/things",
			[]string{"Authorization", "Bearer " + unknown}, ""},
		// No boundary before the prefix: a placeholder written right after a letter would go
		// upstream unswapped with one, which is what this refusal closes. The named revert:
		// proxy-placeholder-left-bounded.
		{"a placeholder written right after a letter in the path", "/v1/x" + unknown,
			[]string{"Authorization", "Bearer " + workerPlaceholder}, ""},
		{"a placeholder written right after a letter in a header", "/v1/things",
			[]string{"Authorization", "Bearer " + workerPlaceholder, "X-Note", "x" + unknown}, ""},
		// A minted capability's placeholder: the prefix, its kind's word and a hyphen, then the
		// 32. The named revert: proxy-leftover-stored-form-only.
		{"a minted capability's placeholder in a header no entry names", "/v1/things",
			[]string{"Authorization", "Bearer " + workerPlaceholder, "X-Note", minted}, ""},
		{"a minted capability's placeholder in the query", "/v1/things?capability=" + minted,
			[]string{"Authorization", "Bearer " + workerPlaceholder}, ""},
		// The query read pair by pair: an escape that is no escape elsewhere in it does not hide a
		// placeholder one of whose letters is written as an escape.
		{"the own grant's placeholder, one letter escaped, in a query with a bad escape", "/v1/things?bad=%zz&k=" +
			workerPlaceholder[:len(workerPlaceholder)-1] + fmt.Sprintf("%%%02X", workerPlaceholder[len(workerPlaceholder)-1]),
			[]string{"Authorization", "Bearer " + workerPlaceholder}, testGrantID},
		// The same in a pair that also holds ';', which url.ParseQuery skips whole.
		{"the own grant's placeholder, one letter escaped, in a pair holding ';' beside a bad escape", "/v1/things?bad=%zz&k=" +
			workerPlaceholder[:len(workerPlaceholder)-1] + fmt.Sprintf("%%%02X", workerPlaceholder[len(workerPlaceholder)-1]) + ";x",
			[]string{"Authorization", "Bearer " + workerPlaceholder}, testGrantID},
		// A bad escape in the same value as the placeholder, which an unescape of that value
		// alone refuses whole.
		{"the own grant's placeholder, one letter escaped, after a bad escape in the same value", "/v1/things?k=%zz" +
			workerPlaceholder[:len(workerPlaceholder)-1] + fmt.Sprintf("%%%02X", workerPlaceholder[len(workerPlaceholder)-1]),
			[]string{"Authorization", "Bearer " + workerPlaceholder}, testGrantID},
		// A Basic credential in base64 with no padding, which neither the swap nor a padded decode
		// reads.
		{"an ended grant's placeholder inside an unpadded Basic credential", "/v1/things",
			[]string{"Authorization", "Bearer " + workerPlaceholder, "X-Note", "Basic " + base64.RawStdEncoding.EncodeToString([]byte("x-access-token:"+endedSame))}, endedSameGrant},
	} {
		refusals := len(r.logs.refusals())
		answer, body := r.get(client, "https://"+boundHost+row.path, row.headers...)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "placeholder") {
			t.Errorf("%s: answered %d %q; want 403 naming the placeholder left after the swap", row.name, answer.StatusCode, body)
			continue
		}
		lines := r.logs.refusals()[refusals:]
		if len(lines) != 1 || lines[0].fields["code"] != codePlaceholderLeft {
			t.Errorf("%s: the refusal logged %v; want one line coded %s", row.name, lines, codePlaceholderLeft)
			continue
		}
		if got, named := lines[0].fields["placeholder_grant"]; row.grant == "" && named || row.grant != "" && got != row.grant {
			t.Errorf("%s: the refusal line named placeholder_grant %v; want %q", row.name, got, row.grant)
		}
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service saw %d request(s) carrying a placeholder; want none: %v", len(seen), seen)
	}

	r.get(client, "https://"+boundHost+"/v1/own", "Authorization", "Bearer "+workerPlaceholder)
	r.get(client, "https://"+boundHost+"/v1/second", "X-Second", "Token "+peerPlaceholder)
	seen := r.requests()
	if len(seen) != 2 || seen[0].Header.Get("Authorization") != "Bearer "+workerValue || seen[1].Header.Get("X-Second") != "Token "+peerValue {
		t.Fatalf("the controls reached the service as %v; want the own grant's and the second grant's values swapped in", seen)
	}
	// The prefix with no whole placeholder after it is no placeholder, in a header or inside a
	// Basic credential, so it travels as the command wrote it.
	prefixOnly := placeholder.Prefix + "abc and the docs"
	r.get(client, "https://"+boundHost+"/v1/note", "Authorization", "Bearer "+workerPlaceholder, "X-Note", prefixOnly, "X-Second", gitBasic(prefixOnly))
	seen = r.requests()
	if len(seen) != 3 || seen[2].Header.Get("X-Note") != prefixOnly || seen[2].Header.Get("X-Second") != gitBasic(prefixOnly) {
		t.Fatalf("the request carrying the prefix alone reached the service as %v; want it sent with its headers as written", seen)
	}
}

// TestAFailedReadOfTheEnrollmentsGrantsDecidesOnlyWhatItCouldChange: the enrollment's other
// grants are read only where they could change the answer, so a read of them that does not happen
// fails no call they had no part in. With every such read failing: a CONNECT to a host no entry of
// the rules binds is a tunnel that carries the command's bytes, as on a broker that never reads
// other grants; a request the connection's own grant binds the host for, and whose headers hold no
// whole placeholder, is handled for that grant and sent. The controls, in the same run: a CONNECT to
// a host only another grant of the session could bind is refused 503, and a request carrying
// another grant's placeholder on the own grant's connection is refused 503, since each needs the
// read; and the read count shows the two allowed calls read nothing.
func TestAFailedReadOfTheEnrollmentsGrantsDecidesOnlyWhatItCouldChange(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{})
	r.grants.connections = map[string]requests.AirlockConnection{}
	r.addGrant(chatPlaceholder, testGrantID, testEnrollID, chatName, "the-real-chat-token")
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	chat, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Hosts: []string{chatHost}, Authority: r.service.Authority()})
	if err != nil {
		t.Fatalf("the stand-in %s: %v", chatHost, err)
	}
	t.Cleanup(chat.Close)
	r.addresses[chatHost] = []netip.Addr{netip.MustParseAddr("93.184.216.39")}
	r.dialTo[net.JoinHostPort(chatHost, "443")] = chat.Addr()
	r.grants.failEnrollmentReads(databaseDown)

	answer, conn, _ := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(chatPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to %s, which no entry binds, answered %d with the enrollment read failing; want 200 and a tunnel", echoHost, answer.StatusCode)
	}
	sent := "bytes for a host no entry binds\n"
	if _, err := io.WriteString(conn, sent); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	got := make([]byte, len(sent))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != sent {
		t.Fatalf("the tunnel carried back %q (%v); want %q", got, err, sent)
	}
	conn.Close()
	if reads := r.grants.enrollmentReads(); reads != 0 {
		t.Fatalf("a CONNECT to a host no entry binds read the enrollment's grants %d time(s); want none", reads)
	}

	if answer, body := r.get(r.client(chatPlaceholder), "https://"+chatHost+"/v1/plain", "X-Plain", "nothing of any grant"); answer.StatusCode != http.StatusOK {
		t.Fatalf("a request the own grant binds the host for, holding no placeholder, answered %d %q with the enrollment read failing; want 200", answer.StatusCode, body)
	}
	if len(chat.Requests()) != 1 {
		t.Fatalf("%s saw %d requests; want the one the own grant handled", chatHost, len(chat.Requests()))
	}
	if reads := r.grants.enrollmentReads(); reads != 0 {
		t.Fatalf("a request holding no placeholder read the enrollment's grants %d time(s); want none", reads)
	}

	answer, _, _ = r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(chatPlaceholder))
	if answer.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("CONNECT to %s, which only another grant of the session binds, answered %d with the enrollment read failing; want 503", boundHost, answer.StatusCode)
	}
	if answer, body := r.get(r.client(chatPlaceholder), "https://"+chatHost+"/v1/plain", "X-Second", "Token "+peerPlaceholder); answer.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a request carrying another grant's placeholder answered %d %q with the enrollment read failing; want 503", answer.StatusCode, body)
	}
	if reads := r.grants.enrollmentReads(); reads != 2 {
		t.Fatalf("the enrollment's grants were read %d time(s); want two, one for each call that needed them", reads)
	}
}

// TestAGrantThatCannotBindTheHostDoesNotDecideTheCall: the session holds a grant whose one name is
// bound to tunnelHost alone, and that grant's own check fails - its project check while Dispatch
// cannot answer. Two calls to boundHost carry the peer grant's placeholder, and neither reads the
// failing grant, since it holds no name bound to boundHost:
//
//   - one on a connection whose password's grant binds nothing on boundHost, so the CONNECT is
//     decrypted on sessionBinds' read of the session's other grants, and the request is handled on
//     handledFor's;
//   - one on a connection whose password's grant binds boundHost, so the CONNECT reads nothing and
//     the request is handled on handledFor's read alone.
//
// Each is read and swapped. The control, in the same run: a CONNECT to tunnelHost from a password
// whose grant binds nothing there reads the failing grant, and is refused 503. The named revert:
// proxy-other-grants-every-name, which reads every grant of the session for any bound host.
func TestAGrantThatCannotBindTheHostDoesNotDecideTheCall(t *testing.T) {
	const (
		failingGrant = "grant-00000009"
		failingName  = "FAILING_TOKEN"
		chatGrant    = "grant-00000008"
	)
	failing := placeholder.Prefix + "failingfailingfailingfailingfail"
	rules := proxyrules.Rules{
		Kinds: []proxyrules.Kind{
			swapKind("worker", []string{boundHost}, "authorization"),
			swapKind("peer", []string{boundHost}, "x-second"),
			swapKind("chat", []string{chatHost}, "authorization"),
			swapKind("failing", []string{tunnelHost}, "authorization"),
		},
		Names: []proxyrules.Binding{
			swapBinding(workerName, "worker"),
			swapBinding(peerName, "peer"),
			swapBinding(chatName, "chat"),
			swapBinding(failingName, "failing"),
		},
	}
	r := newRuledRig(t, rules, Options{})
	r.addGrant(peerPlaceholder, peerGrantID, testEnrollID, peerName, peerValue)
	r.addGrant(chatPlaceholder, chatGrant, testEnrollID, chatName, "the-real-chat-token")
	r.addGrant(failing, failingGrant, testEnrollID, failingName, "the-real-failing-token")
	r.grants.failGrantCheck(failingGrant, databaseDown)

	for i, password := range []string{chatPlaceholder, workerPlaceholder} {
		answer, body := r.get(r.client(password), "https://"+boundHost+"/v1/second", "X-Second", "Token "+peerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("call %d to %s carrying the peer grant's placeholder answered %d %q while a grant bound only to %s fails its check; want 200", i+1, boundHost, answer.StatusCode, body, tunnelHost)
		}
		if seen := r.requests(); len(seen) != i+1 || seen[i].Header.Get("X-Second") != "Token "+peerValue {
			t.Fatalf("after call %d the service saw %v; want the peer grant's value swapped in", i+1, seen)
		}
	}

	answer, conn, _ := r.connectRaw(net.JoinHostPort(tunnelHost, "443"), basic(chatPlaceholder))
	if conn != nil {
		conn.Close()
	}
	if answer.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("CONNECT to %s, which only the failing grant binds, answered %d; want 503", tunnelHost, answer.StatusCode)
	}
}
