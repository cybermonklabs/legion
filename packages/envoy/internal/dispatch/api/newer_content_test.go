package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// A document holding an element this build does not know was written by a newer build while two
// builds serve one database. Every route that starts from it - the reads, an edit (what `dispatch
// doc-edit` calls) and a named version - answers 409 DOC_NEWER_CONTENT with one message, whether a
// room holds the document or only the store does. The message says to try again in one minute and
// not to replace or rebuild the document, and an upload over the document is refused, so the
// element stays. Each refusal writes one WARN line, which names the elements the document holds
// and carries nothing of the error (requireNewerContentWarnings).
func TestEveryRouteAnswersADocumentHoldingNewerContentAlike(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	for _, resident := range []bool{true, false} {
		name := "stored"
		if resident {
			name = "resident"
		}
		t.Run(name, func(t *testing.T) {
			logs := &logtest.Recorder{}
			logtest.SetDefault(t, logs)
			handler, _, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
				"name": "newer.md", "content": "before\n",
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
			}
			artifact := decodeBody[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, created).Artifact
			service := deps.Docs.(*docs.Service)
			if err := service.InjectUnknownElementForTest(context.Background(), artifact.ID); err != nil {
				t.Fatalf("write an element this build does not know: %v", err)
			}
			if !resident {
				if err := service.Evict(context.Background(), artifact.ID); err != nil {
					t.Fatalf("evict the document: %v", err)
				}
			}
			base := "/api/v1/artifacts/" + artifact.ID
			messages := map[string]string{}
			for _, route := range []struct {
				name, method, path string
				body               any
			}{
				{"text", http.MethodGet, base + "/text", nil},
				{"blocks", http.MethodGet, base + "/blocks", nil},
				{"edits", http.MethodPost, base + "/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "More.\n"}},
				}},
				{"versions", http.MethodPost, base + "/versions", map[string]string{"summary": "checkpoint"}},
			} {
				response := dispatchRequest(t, handler, route.method, route.path, route.body, "alice")
				answer := decodeBody[struct {
					Code  string `json:"code"`
					Error string `json:"error"`
				}](t, response)
				if response.Code != http.StatusConflict || answer.Code != "DOC_NEWER_CONTENT" || !strings.Contains(answer.Error, "Try again in one minute. Do not replace or rebuild the document") || strings.Contains(answer.Error, "If you replace") || !strings.Contains(answer.Error, docs.UnknownElementForTest) {
					t.Fatalf("%s %s: status=%d body=%s, want 409 DOC_NEWER_CONTENT naming the element, the retry and that no replacement or rebuild may follow", route.method, route.name, response.Code, response.Body.String())
				}
				messages[route.name] = answer.Error
			}
			for name, message := range messages {
				if message != messages["text"] {
					t.Fatalf("%s answered %q, /text %q: want one message on every route", name, message, messages["text"])
				}
			}

			upload := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "newer.md"}, "newer.md", "text/markdown", []byte("replaced\n"), "alice")
			if upload.Code != http.StatusConflict || !strings.Contains(upload.Body.String(), `"code":"DOC_NEWER_CONTENT"`) {
				t.Fatalf("upload over newer content: status=%d body=%s, want 409 DOC_NEWER_CONTENT", upload.Code, upload.Body.String())
			}
			text := dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
			if text.Code != http.StatusConflict || !strings.Contains(text.Body.String(), `"code":"DOC_NEWER_CONTENT"`) {
				t.Fatalf("read after the refused upload: status=%d body=%s, want the element still there (409 DOC_NEWER_CONTENT)", text.Code, text.Body.String())
			}
			requireNewerContentWarnings(t, logs, len(messages))
		})
	}
}

// newerContentWarning is the WARN line writeHandlerError writes for a document holding an element
// this build cannot read, written out here rather than taken from the code under test.
const newerContentWarning = "dispatch: API document holds an element this build cannot read"

// requireNewerContentWarnings fails t unless the log holds at least least of those lines and
// every one of them names the elements the document holds and nothing else.
//
// The line carries no error: the chain that reaches writeHandlerError carries what each route
// wrapped the refusal in, the caller's own identity among it, which the operator's log is not
// the place for (CodeQL go/clear-text-logging). The elements come from the refusal's own field
// (docs.NewerContentError.Elements), so the line still says what the document holds.
func requireNewerContentWarnings(t *testing.T, logs *logtest.Recorder, least int) {
	t.Helper()
	warned := 0
	for _, record := range logs.Records() {
		if record.Message != newerContentWarning {
			continue
		}
		warned++
		attributes := map[string]string{}
		record.Attrs(func(attr slog.Attr) bool {
			attributes[attr.Key] = attr.Value.String()
			return true
		})
		if !strings.Contains(attributes["elements"], docs.UnknownElementForTest) {
			t.Errorf("the warn line's elements are %q, want the element %q among them", attributes["elements"], docs.UnknownElementForTest)
		}
		if len(attributes) != 1 {
			t.Errorf("the warn line carries %v, want the elements alone: every other reading of the refusal carries what the route wrapped it in", attributes)
		}
		for key, value := range attributes {
			if strings.Contains(value, docs.ErrDocNewerContent.Error()) {
				t.Errorf("the warn line's %s carries the refusal's own message, which no log of ours writes: %q", key, value)
			}
		}
	}
	if warned < least {
		t.Errorf("warn lines %q = %d, want at least one per route the document refused (%d)", newerContentWarning, warned, least)
	}
}

// A document whose stored callout holds a kind this build's manifest does not list - a kind a
// newer build added, or any text a peer wrote into the attribute, a person's address among it -
// answers 409 DOC_NEWER_CONTENT quoting the value, so the caller is told what the document holds.
// The WARN line names the value by its typed block and attribute alone, and no log record carries
// the value: a log line is read by whoever reads the server's logs, which no text of a document
// reaches.
func TestAnEnumValueThisBuildDoesNotListIsLoggedByItsAttributeAlone(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	const address = "person-7@example.com"
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createArtifactIssue(t, handler)
	artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "kind.md", func(ctx context.Context, database *store.Store, artifactID string) error {
		return docs.AppendStoredChangeForTest(ctx, database, artifactID, func(transaction *crdt.Transaction, fragment *crdt.YXmlFragment) error {
			callout := crdt.NewYXmlElement("callout")
			fragment.InsertElement(transaction, len(fragment.Children()), callout)
			callout.SetAttributeValue(transaction, "kind", address)
			paragraph := crdt.NewYXmlElement("paragraph")
			callout.InsertElement(transaction, 0, paragraph)
			body := crdt.NewYXmlText()
			paragraph.InsertText(transaction, 0, body)
			body.Insert(transaction, 0, "Registered.", nil)
			return nil
		})
	})
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
	answer := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusConflict || answer.Code != "DOC_NEWER_CONTENT" || !strings.Contains(answer.Error, address) {
		t.Fatalf("/text: status=%d body=%s, want 409 DOC_NEWER_CONTENT quoting the callout's kind", response.Code, response.Body.String())
	}
	warned := 0
	for _, record := range logs.Records() {
		if strings.Contains(record.Message, address) {
			t.Errorf("a log line's message holds the callout's kind: %q", record.Message)
		}
		record.Attrs(func(attribute slog.Attr) bool {
			if strings.Contains(attribute.Value.String(), address) {
				t.Errorf("the log line %q carries the callout's kind in %s: %q", record.Message, attribute.Key, attribute.Value.String())
			}
			if record.Message == newerContentWarning && attribute.Key == "elements" {
				warned++
				elements, _ := attribute.Value.Any().([]string)
				if want := []string{"callout kind value"}; !slices.Equal(elements, want) {
					t.Errorf("the warn line's elements are %q, want %q", attribute.Value.String(), want)
				}
			}
			return true
		})
	}
	if warned == 0 {
		t.Errorf("the log holds no line %q naming the elements", newerContentWarning)
	}
}

// The test hook POST /api/v1/artifacts/{id}/_test/newer-content gives a browser test a document
// holding newer content: after it, the document answers 409 DOC_NEWER_CONTENT naming the element.
// Without DISPATCH_TEST_HOOKS the route is not mounted, and the document is unchanged.
func TestTheNewerContentHookWritesAnElementThisBuildDoesNotKnow(t *testing.T) {
	t.Parallel()
	for _, hooks := range []bool{true, false} {
		handler, _, _ := newTestServer(t, testServerOptions{testHooks: hooks})
		issue := createArtifactIssue(t, handler)
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": "newer.md", "content": "before\n",
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
		}
		base := "/api/v1/artifacts/" + decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, created).Artifact.ID
		hook := dispatchRequest(t, handler, http.MethodPost, base+"/_test/newer-content", nil, "alice")
		text := dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
		if !hooks {
			if hook.Code != http.StatusNotFound || text.Code != http.StatusOK {
				t.Fatalf("without the test hooks: the hook answered %d %s and /text %d %s, want 404 and the document unchanged", hook.Code, hook.Body.String(), text.Code, text.Body.String())
			}
			continue
		}
		if hook.Code != http.StatusNoContent {
			t.Fatalf("the hook: status=%d body=%s, want 204", hook.Code, hook.Body.String())
		}
		if text.Code != http.StatusConflict || !strings.Contains(text.Body.String(), `"code":"DOC_NEWER_CONTENT"`) || !strings.Contains(text.Body.String(), docs.UnknownElementForTest) {
			t.Fatalf("/text after the hook: status=%d body=%s, want 409 DOC_NEWER_CONTENT naming %s", text.Code, text.Body.String(), docs.UnknownElementForTest)
		}
	}
}
