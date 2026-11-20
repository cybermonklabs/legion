package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The clean stop is one DELETE of the session's own key, with the listener's bearer, and it names
// both of the conditions it holds under: the client instance whose registration it means to
// remove (client_instance_id) and the moment the stop belongs to (if_not_registered_after). An
// instance id is a client's own string, so it is escaped rather than written into the query raw.
func TestDeleteSessionAsksTheSessionRouteWithTheBearerTheMomentAndTheInstance(t *testing.T) {
	var method, path, query, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	goneAt := time.Now()

	stopped, err := New(server.URL+"/", "envoy-bearer").DeleteSession(context.Background(), "ses_implementer",
		DeleteConditions{ClientInstanceID: "pi/omp 1+1", NotRegisteredAfter: goneAt})
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if !stopped.Deleted {
		t.Fatalf("DeleteSession = %+v, want the registration taken as deleted", stopped)
	}
	if method != http.MethodDelete || path != "/v1/sessions/ses_implementer" || bearer != "Bearer envoy-bearer" {
		t.Fatalf("%s %s with %q, want a DELETE of the session's key with the listener's bearer", method, path, bearer)
	}
	want := fmt.Sprintf("if_not_registered_after=%d&client_instance_id=pi%%2Fomp+1%%2B1", goneAt.UnixMilli())
	if query != want {
		t.Fatalf("query = %q, want both conditions, the id escaped: %q", query, want)
	}
}

// An entry that names no client process — one a client predating the field wrote — leaves the
// moment to judge the delete alone, and the parameter is left out rather than sent empty: the
// listener compares no instance for an empty value as for an absent one
// (session.InstanceMatches), so the two are the same delete and the query states only the
// condition the stop really holds.
func TestDeleteSessionLeavesOutAnInstanceTheEntryDoesNotName(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	goneAt := time.Now()

	if _, err := New(server.URL, "").DeleteSession(context.Background(), "ses_old_client",
		DeleteConditions{NotRegisteredAfter: goneAt}); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if want := fmt.Sprintf("if_not_registered_after=%d", goneAt.UnixMilli()); query != want {
		t.Fatalf("query = %q, want the moment alone, %q", query, want)
	}
}

// A listener that kept the registration says so, and the caller is told which it was: that answer
// is the one that stops a slow stop deleting the registration a relaunched process wrote under
// the same session id (dispatch://INFRA-392).
func TestDeleteSessionReportsARegistrationTheListenerKept(t *testing.T) {
	const reason = "the registration belongs to another client instance"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"deleted":false,"reason":"`+reason+`"}`)
	}))
	defer server.Close()

	stopped, err := New(server.URL, "").DeleteSession(context.Background(), "ses_relaunched",
		DeleteConditions{ClientInstanceID: "inst_dead", NotRegisteredAfter: time.Now()})
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if stopped.Deleted || stopped.Reason != reason {
		t.Fatalf("DeleteSession = %+v, want the registration kept with the listener's reason", stopped)
	}
}

// A registry that holds no record for the session has already reached the outcome the stop asks
// for, so its 404 is no failure; a refusal that leaves the registration standing is one, and says
// what the listener answered so an operator can see why the session still holds its role.
func TestDeleteSessionTellsAnAlreadyGoneSessionFromARefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "the listener removed it", status: http.StatusOK, body: `{"deleted":true}`},
		{name: "an older listener removed it without saying so", status: http.StatusOK, body: ``},
		{name: "the registry holds no record of it", status: http.StatusNotFound, body: `{"error":"session not found"}`},
		{name: "the bus could not be reached", status: http.StatusServiceUnavailable, body: `{"error":"session registry unavailable"}`,
			wantErr: "session registry unavailable"},
		{name: "the bearer is wrong", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantErr: "401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			stopped, err := New(server.URL, "").DeleteSession(context.Background(), "ses_implementer",
				DeleteConditions{ClientInstanceID: "inst_dead", NotRegisteredAfter: time.Now()})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("DeleteSession = %v, want the session taken as stopped", err)
			case tc.wantErr == "" && !stopped.Deleted:
				t.Fatalf("DeleteSession = %+v, want the registration taken as deleted", stopped)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("DeleteSession = %v, want a failure naming %q", err, tc.wantErr)
			}
		})
	}
}

// The read before the stop is one GET of the session's registry entry, with the listener's bearer
// and the session id escaped into the path, and it asks for both selectors. view=registration is
// the registration-only answer, which a listener carrying it gives from the session bucket alone
// — no claims view and no interest record, so no read that the stop does not need can refuse it.
// include=unresponsive is what a listener predating that selector needs in order to answer a
// record past its live window at all, which is every record the stop is owed for: it ignores the
// unknown selector and answers the full row. What the read answers is the entry's own client
// instance and its last registration.
func TestReadSessionRegistrationAsksTheRegistryForTheEntrysInstance(t *testing.T) {
	var method, path, query, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"session_id":"ses/dead","client_instance_id":"inst_dead","last_seen":1762560000000}`)
	}))
	defer server.Close()

	registration, err := New(server.URL, "envoy-bearer").ReadSessionRegistration(context.Background(), "ses/dead")
	if err != nil {
		t.Fatalf("ReadSessionRegistration: %v", err)
	}
	want := SessionRegistration{Registered: true, ClientInstanceID: "inst_dead", LastSeen: 1762560000000}
	if registration != want {
		t.Fatalf("ReadSessionRegistration = %+v, want %+v", registration, want)
	}
	if method != http.MethodGet || path != "/v1/registry/ses/dead" || bearer != "Bearer envoy-bearer" {
		t.Fatalf("%s %s with %q, want a GET of the session's registry entry with the listener's bearer", method, path, bearer)
	}
	if query != "include=unresponsive&view=registration" {
		t.Fatalf("query = %q, want both the unresponsive record and the registration view asked for", query)
	}
}

// A registry that holds no record of the session answers 404, and that is the outcome the stop
// wanted rather than a failure: the caller is told there is no registration, with no error, and
// sends no delete.
func TestReadSessionRegistrationReadsA404AsNoRegistration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"no live session ses_gone"}`)
	}))
	defer server.Close()

	registration, err := New(server.URL, "").ReadSessionRegistration(context.Background(), "ses_gone")
	if err != nil {
		t.Fatalf("ReadSessionRegistration = %v, want a registry holding none taken as no failure", err)
	}
	if registration != (SessionRegistration{}) {
		t.Fatalf("ReadSessionRegistration = %+v, want no registration at all", registration)
	}
}

// A listener predating the view selector answers the whole session row — its topics, its roles,
// its capabilities — and nothing bounds how many topics a long-lived session subscribes to, so
// the entry is read within answerBodyLimit rather than refusalBodyLimit, which bounds
// an error body. A row cut off at that limit would not decode, and the stop of the busiest
// session would be reported as a listener that did not answer and send no delete at all.
func TestReadSessionRegistrationReadsAnEntryPastTheRefusalBodyLimit(t *testing.T) {
	topics := make([]string, 0, 400)
	for i := range 400 {
		topics = append(topics, fmt.Sprintf("notifications.dispatch.issue.infra-%04d", i))
	}
	entry, err := json.Marshal(map[string]any{
		"session_id": "ses_busy", "client_instance_id": "inst_busy", "last_seen": 1762560000000,
		"topics": topics,
	})
	if err != nil {
		t.Fatalf("build the entry: %v", err)
	}
	if len(entry) <= refusalBodyLimit {
		t.Fatalf("the entry is %d bytes, want one past the %d-byte refusal limit", len(entry), refusalBodyLimit)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry)
	}))
	defer server.Close()

	registration, err := New(server.URL, "").ReadSessionRegistration(context.Background(), "ses_busy")
	if err != nil {
		t.Fatalf("ReadSessionRegistration of a %d-byte entry: %v", len(entry), err)
	}
	want := SessionRegistration{Registered: true, ClientInstanceID: "inst_busy", LastSeen: 1762560000000}
	if registration != want {
		t.Fatalf("ReadSessionRegistration = %+v, want %+v", registration, want)
	}
}

// An answer this client cannot read is no registration it may name in a delete, and neither is a
// listener that refused the read: both are failures, and the refusal names the status so an
// operator can see why no stop was sent.
func TestReadSessionRegistrationTellsARefusalFromAnEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "the body is not an entry at all", status: http.StatusOK, body: `not json`, wantErr: "read the registration"},
		{name: "the bus could not be reached", status: http.StatusServiceUnavailable,
			body: `{"error":"session registry unavailable"}`, wantErr: "503"},
		{name: "the listener failed", status: http.StatusInternalServerError, body: `{"error":"boom"}`, wantErr: "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			registration, err := New(server.URL, "").ReadSessionRegistration(context.Background(), "ses_implementer")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ReadSessionRegistration = %+v, %v, want a failure naming %q", registration, err, tc.wantErr)
			}
		})
	}
}

// heldListener answers each request hold after it arrived, with a 200 and body, or ends it the
// moment the request's own context ends first: a listener that takes hold to answer the clean
// stop's read or delete. It stands in for the connection itself, so in a synctest bubble the hold
// is the bubble's clock and the run waits for nothing.
type heldListener struct {
	hold time.Duration
	body string
}

func (l heldListener) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(l.hold):
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(l.body)), Request: r}, nil
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

// The clean stop's read and delete have one bound, the context the daemon's stop gives them (its
// budget, sessionStop.send in packages/daemon/internal/daemon/daemon.go), and none of the client's
// own: requestTimeout bounds a publish and a role lookup alone. The production client has no
// timeout, and a transport of its own with no fixed limit (stopTransport), which each row checks
// (wantNoFixedLimit) before it replaces that transport with a listener that holds its answer;
// TestTheCleanStopsTransportKeepsNoLimitOfTheSharedOne shows a stalled connect and a stalled TLS
// handshake on it. The transport's idle timeout closes only pooled idle connections, so replacing
// it hides no bound on a held answer. A listener that answers past requestTimeout but inside the
// budget is waited for, and one that answers after the budget is given up at the budget. The
// budget is a day, longer than any a test of the daemon gives its stops, so a fixed client
// timeout of any length under it, the 10 s requestTimeout or a minute alike, ends the second
// row's request before the budget does, and fails it. Each row
// runs in a synctest bubble, whose clock moves only while every goroutine in it waits, so the
// holds and the budget are times on that clock, not on the run's, and the day costs the run
// nothing.
func TestTheCleanStopsReadAndDeleteEndAtTheirContextAlone(t *testing.T) {
	const budget = 24 * time.Hour
	for _, request := range []struct {
		name string
		body string
		send func(ctx context.Context, p *HTTPPublisher) error
	}{
		{name: "the registry read", body: `{"session_id":"ses_dead","client_instance_id":"inst_dead","last_seen":1762560000000}`,
			send: func(ctx context.Context, p *HTTPPublisher) error {
				_, err := p.ReadSessionRegistration(ctx, "ses_dead")
				return err
			}},
		{name: "the session delete", body: `{"deleted":true}`,
			send: func(ctx context.Context, p *HTTPPublisher) error {
				_, err := p.DeleteSession(ctx, "ses_dead", DeleteConditions{ClientInstanceID: "inst_dead", NotRegisteredAfter: time.Now()})
				return err
			}},
	} {
		for _, hold := range []time.Duration{requestTimeout + time.Second, budget + time.Minute} {
			t.Run(fmt.Sprintf("%s, answered after %s", request.name, hold), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					p := New("http://listener.invalid", "envoy-bearer")
					if p.stops.Timeout != 0 {
						t.Fatalf("the clean stop's client has a timeout of %s, want none: its context is the read's and the delete's one bound", p.stops.Timeout)
					}
					wantNoFixedLimit(t, p.stops.Transport)
					p.stops.Transport = heldListener{hold: hold, body: request.body}
					ctx, cancel := context.WithTimeout(context.Background(), budget)
					defer cancel()
					began := time.Now()
					err := request.send(ctx, p)
					took := time.Since(began)
					if hold < budget {
						if err != nil || took != hold {
							t.Fatalf("%s answered %s into a budget of %s ended after %s with %v, want the answer it waited for", request.name, hold, budget, took, err)
						}
						return
					}
					if !errors.Is(err, context.DeadlineExceeded) || took != budget {
						t.Fatalf("%s the listener would answer after %s ended after %s with %v, want it given up at its budget of %s", request.name, hold, took, err, budget)
					}
				})
			})
		}
	}
}

// wantNoFixedLimit fails t unless transport is an *http.Transport with no field through which a
// request could meet a fixed limit before its context ends (fixedLimits).
func wantNoFixedLimit(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	own, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("the clean stop's transport is %T, want an *http.Transport of its own (stopTransport)", transport)
	}
	if found := fixedLimits(own); len(found) > 0 {
		t.Fatalf("the clean stop's transport has %s, want none: each could end the stop before its context does", strings.Join(found, ", "))
	}
}

// exemptFields are the fields of http.Transport that fixedLimits leaves to other checks:
// IdleConnTimeout closes only pooled connections no request uses, DialContext is the dialer the
// stop's transport makes for itself (its own limit sits inside the function, where no field shows
// it, so TestTheCleanStopsTransportKeepsNoLimitOfTheSharedOne stalls a connect instead), and
// Proxy must be Go's own choice from the environment, which fixedLimits checks by itself.
var exemptFields = map[string]bool{"IdleConnTimeout": true, "DialContext": true, "Proxy": true}

// fixedLimits names each field of own through which a request could meet a limit other than its
// context's, read by reflection over http.Transport and http.HTTP2Config (as of Go 1.27.3), so a
// field a later Go adds is read too: every time.Duration that is not zero, and every function and
// every map of functions that is not nil, but exemptFields; no DialContext at all; a Proxy other
// than http.ProxyFromEnvironment; and the same over HTTP2 with no field exempt. The TLS
// configuration is not read: it is the trust the stop keeps (stopTransport). Each entry starts
// with the field's name, Transport.<field> or HTTP2.<field>, and a space.
func fixedLimits(own *http.Transport) []string {
	found := limitFields("Transport", reflect.ValueOf(own).Elem(), exemptFields)
	if own.DialContext == nil {
		found = append(found, "Transport.DialContext (none: no dialer of its own)")
	}
	if reflect.ValueOf(own.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		found = append(found, "Transport.Proxy (a function other than http.ProxyFromEnvironment)")
	}
	if own.HTTP2 != nil {
		found = append(found, limitFields("HTTP2", reflect.ValueOf(own.HTTP2).Elem(), nil)...)
	}
	return found
}

var durationType = reflect.TypeFor[time.Duration]()

// limitFields names each exported field of the struct v, but exempt, that is a time.Duration
// other than zero, or a function or a map of functions other than nil, as fixedLimits does.
func limitFields(name string, v reflect.Value, exempt map[string]bool) []string {
	var found []string
	for i := range v.NumField() {
		field, value := v.Type().Field(i), v.Field(i)
		if !field.IsExported() || exempt[field.Name] {
			continue
		}
		switch {
		case value.Type() == durationType && value.Int() != 0:
			found = append(found, fmt.Sprintf("%s.%s (%s)", name, field.Name, time.Duration(value.Int())))
		case value.Kind() == reflect.Func && !value.IsNil():
			found = append(found, fmt.Sprintf("%s.%s (a function)", name, field.Name))
		case value.Kind() == reflect.Map && value.Type().Elem().Kind() == reflect.Func && !value.IsNil():
			found = append(found, fmt.Sprintf("%s.%s (a map of functions)", name, field.Name))
		}
	}
	return found
}

// Whatever the shared transport holds, the clean stop's transport keeps no field through which a
// request could meet a fixed limit (stopTransport). The shared transport here has every field
// fixedLimits reads set, by reflection over Go's own types (plant): each time.Duration 2 s, each
// function, and each map of functions, one that fails at once, on http.Transport and on its
// HTTP2 settings. So a field a later Go adds is set here and must be cleared there too. The
// control: fixedLimits names every field the plant set on the shared transport but the exempt
// ones, so the check that finds none on the stop's transport can see each of them.
func TestTheCleanStopsTransportKeepsNoLimitOrFunctionOfTheSharedOne(t *testing.T) {
	shared := http.DefaultTransport.(*http.Transport).Clone()
	set := plant("Transport", reflect.ValueOf(shared).Elem())
	shared.HTTP2 = &http.HTTP2Config{}
	set = append(set, plant("HTTP2", reflect.ValueOf(shared.HTTP2).Elem())...)
	found := fixedLimits(shared)
	for _, name := range set {
		exempt := strings.HasPrefix(name, "Transport.") && exemptFields[strings.TrimPrefix(name, "Transport.")] && name != "Transport.Proxy"
		named := slices.ContainsFunc(found, func(entry string) bool { return strings.HasPrefix(entry, name+" ") })
		if named == exempt {
			t.Fatalf("fixedLimits named %q of the shared transport where the plant set %q, want each field set named but %v", found, set, exemptFields)
		}
	}
	wantNoFixedLimit(t, stopTransport(shared))
}

var errorType = reflect.TypeFor[error]()

// plant sets every exported field of the struct v that fixedLimits reads: each time.Duration to
// 2 s, each function to one that answers its zero values and an error at once, and each map of
// functions to a map of one such function. It answers the names it set, <name>.<field>.
func plant(name string, v reflect.Value) []string {
	failing := func(fn reflect.Type) reflect.Value {
		return reflect.MakeFunc(fn, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, fn.NumOut())
			for i := range out {
				out[i] = reflect.Zero(fn.Out(i))
				if fn.Out(i) == errorType {
					out[i] = reflect.ValueOf(errors.New("a planted function")).Convert(errorType)
				}
			}
			return out
		})
	}
	var set []string
	for i := range v.NumField() {
		field, value := v.Type().Field(i), v.Field(i)
		if !field.IsExported() {
			continue
		}
		switch {
		case value.Type() == durationType:
			value.SetInt(int64(2 * time.Second))
		case value.Kind() == reflect.Func:
			value.Set(failing(value.Type()))
		case value.Kind() == reflect.Map && value.Type().Elem().Kind() == reflect.Func:
			planted := reflect.MakeMap(value.Type())
			planted.SetMapIndex(reflect.ValueOf("planted").Convert(value.Type().Key()), failing(value.Type().Elem()))
			value.Set(planted)
		default:
			continue
		}
		set = append(set, name+"."+field.Name)
	}
	return set
}

// A clean stop closes no connection but its own failed request's (stopFailed). A read and a
// delete that succeed send through one connection, which the stop's transport keeps; a read that
// fails closes its transport's idle connections and nothing of Go's shared transport, which the
// publish and the role lookup send through (HTTPPublisher.client). Each request here reports the
// connection it got (httptrace): the delete gets the read's, and the publish after the failed
// read gets the one the publish before it used. A failed read here is one the listener holds
// until the read's own context ends.
func TestACleanStopsFailedReadClosesNoConnectionOfAnotherRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/registry/ses_held":
			<-r.Context().Done()
		case r.URL.Path == "/v1/registry/ses_dead":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"session_id":"ses_dead","client_instance_id":"inst_dead","last_seen":1762560000000}`)
		case r.Method == http.MethodDelete:
			_, _ = io.WriteString(w, `{"deleted":true}`)
		default:
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	p := New(server.URL, "envoy-bearer")
	got := func() (context.Context, *httptrace.GotConnInfo) {
		info := &httptrace.GotConnInfo{}
		return httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { *info = i }}), info
	}
	ctx, published := got()
	if err := p.Publish(ctx, "notifications.legion.x.controller", "first", nil, "k1"); err != nil {
		t.Fatalf("the first publish: %v", err)
	}
	ctx, read := got()
	if _, err := p.ReadSessionRegistration(ctx, "ses_dead"); err != nil {
		t.Fatalf("the clean stop's read: %v", err)
	}
	ctx, deleted := got()
	if _, err := p.DeleteSession(ctx, "ses_dead", DeleteConditions{ClientInstanceID: "inst_dead", NotRegisteredAfter: time.Now()}); err != nil {
		t.Fatalf("the clean stop's delete: %v", err)
	}
	if !deleted.Reused || deleted.Conn != read.Conn {
		t.Fatalf("the clean stop's delete got a connection reused=%t, the read's=%t, want the read's own: a stop whose read succeeded closed its connection", deleted.Reused, deleted.Conn == read.Conn)
	}
	held, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.ReadSessionRegistration(held, "ses_held"); err == nil {
		t.Fatal("the clean stop's read of a registration the listener holds past the read's context succeeded, want it given up at that context")
	}
	ctx, again := got()
	if err := p.Publish(ctx, "notifications.legion.x.controller", "second", nil, "k2"); err != nil {
		t.Fatalf("the second publish: %v", err)
	}
	if !again.Reused || again.Conn != published.Conn {
		t.Fatalf("the publish after the clean stop's failed read got a connection reused=%t, the first publish's=%t, want the first publish's: the failed stop closed a connection of Go's shared transport", again.Reused, again.Conn == published.Conn)
	}
}
