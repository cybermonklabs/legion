package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A request whose person the identity header names, but whom the people store cannot record, is
// answered 500 INTERNAL, and the operator's line says the handler failed without naming the
// person: the error the store returns says what failed and not whose row it was, because a log
// line is read by whoever reads the server's logs (CodeQL go/clear-text-logging).
func TestAPersonTheStoreCannotRecordIsAnsweredAndLoggedWithoutTheirAddress(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	const address = "person-7@example.com"
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	database := storetest.Open(t)
	// The people store writes through a pool of its own, closed before the request, so the
	// person's record fails in the store's own database call, as it does when the database is
	// out of reach.
	unreachable := storetest.SecondProcess(t, database)
	unreachable.Pool.Close()
	handler, _, _ := newTestServer(t, testServerOptions{
		store:    database,
		identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(unreachable.Pool, testSigningKey, nil)},
	})

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, address)
	answer := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusInternalServerError || answer.Code != codeInternal {
		t.Fatalf("whoami for a person the store cannot record: status=%d body=%s, want 500 %s", response.Code, response.Body.String(), codeInternal)
	}
	failed := 0
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, address) {
			t.Errorf("a log line's message names the person: %q", record.Message)
		}
		record.Attrs(func(attribute slog.Attr) bool {
			if strings.Contains(attribute.Value.String(), address) {
				t.Errorf("the log line %q names the person in %s: %q", record.Message, attribute.Key, attribute.Value.String())
			}
			return true
		})
		if record.Message == "dispatch: API handler failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("the log holds %d lines saying the handler failed, want the one for this request", failed)
	}
}

// A refusal of a document quotes the document's text, a person's address among it, and a handler
// that formats the refusal into its own error with %v leaves no chain errors.Is can read, so no
// refusal class names it. The 500 such an error reaches is still logged without that text: the
// line names the error by the Go types of its chain, the handler that met it, at its call, and
// the route it answered, and no log record of the request holds the address. The route is a row
// of the table, served through the chain register mounts it behind (routeHandler) with its
// handler replaced: once as a read, and once as a keyed write, whose layer answers on a writer of
// its own (keyedAnswer). The handler hands its error on through writeIDPrefixError, one of the
// helpers the line's site reads past to the handler.
func TestAFaultHoldingADocumentsTextIsLoggedByItsTypesSiteAndRoute(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault in its subtests).
	const address = "person-7@example.com"
	refused, refusal := pmdoc.Parse(":::callout{#c1 kind=\"" + address + "\" title=\"T\"}\nBody.\n:::\n")
	if refusal == nil || !errors.Is(refusal, pmdoc.ErrSchema) || !strings.Contains(refusal.Error(), address) {
		t.Fatalf("parse of a callout whose kind is %q: tree=%v err=%v, want a schema refusal quoting the kind", address, refused, refusal)
	}
	failure := fmt.Errorf("register the eval: %w", fmt.Errorf("write the eval's method: %v", refusal))
	if class := docs.RefusalClass(failure); class != "" || !strings.Contains(failure.Error(), address) {
		t.Fatalf("the formatted refusal reads as class %q with message %q, want no class and the address in the message", class, failure.Error())
	}

	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	// TEST-1 is the issue the write's probe path names.
	createArtifactIssue(t, handler)
	replays := served.keyedReplays()
	for _, sent := range []struct {
		method, pattern, key string
	}{
		{http.MethodGet, "/api/v1/whoami", ""},
		{http.MethodPost, "/api/v1/issues/{key}/comments", "fault-1"},
	} {
		name := sent.method + " " + sent.pattern
		t.Run(name, func(t *testing.T) {
			logs := &logtest.Recorder{}
			logtest.SetDefault(t, logs)
			table := readOnlyTable()
			index := slices.IndexFunc(table, func(route apiRoute) bool {
				return route.Method == sent.method && route.Pattern == sent.pattern
			})
			if index < 0 {
				t.Fatalf("the table has no row %s", name)
			}
			route := table[index]
			// site is this handler's call of writeIDPrefixError, the line after the one that reads it.
			var site string
			route.Handler = func(w http.ResponseWriter, r *http.Request) {
				pc, _, line, _ := runtime.Caller(0)
				served.writeIDPrefixError(w, r, failure)
				site = path.Base(runtime.FuncForPC(pc).Name()) + ":" + strconv.Itoa(line+1)
			}
			// On a mux of its own, as register mounts it, so the path's values reach every layer.
			alone := http.NewServeMux()
			alone.HandleFunc(name, served.routeHandler(route, replays[name]))
			request := httptest.NewRequest(sent.method, routeProbePath(sent.pattern), strings.NewReader(routeProbeBody(route)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Dispatch-User", "alice")
			if sent.key != "" {
				request.Header.Set(contracts.IdempotencyKeyHeader, sent.key)
			}
			response := httptest.NewRecorder()
			alone.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError || site == "" {
				t.Fatalf("status=%d body=%s handler ran=%v, want the handler's 500", response.Code, response.Body.String(), site != "")
			}

			failed := 0
			for _, record := range logs.Records() {
				if strings.Contains(record.Message, address) {
					t.Errorf("a log line's message holds the address: %q", record.Message)
				}
				attributes := map[string]slog.Value{}
				record.Attrs(func(attribute slog.Attr) bool {
					if strings.Contains(attribute.Value.String(), address) {
						t.Errorf("the log line %q holds the address in %s: %q", record.Message, attribute.Key, attribute.Value.String())
					}
					attributes[attribute.Key] = attribute.Value
					return true
				})
				if record.Message != "dispatch: API handler failed" {
					continue
				}
				failed++
				var types []string
				if described := attributes["error"]; described.Kind() == slog.KindGroup {
					for _, attribute := range described.Group() {
						if attribute.Key == "types" {
							types, _ = attribute.Value.Any().([]string)
						}
					}
				}
				if want := []string{"*fmt.wrapError", "*errors.errorString"}; !slices.Equal(types, want) {
					t.Errorf("the line's error is %s, want a group whose types are %q", attributes["error"], want)
				}
				if got := attributes["site"].String(); got != site {
					t.Errorf("the line's site is %q, want the handler's call %q", got, site)
				}
				if got := attributes["route"].String(); got != sent.pattern {
					t.Errorf("the line's route is %q, want %q", got, sent.pattern)
				}
			}
			if failed != 1 {
				t.Errorf("the log holds %d lines saying the handler failed, want the one for this request", failed)
			}
		})
	}
}
