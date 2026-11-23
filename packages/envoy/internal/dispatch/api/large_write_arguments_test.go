package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// largeWrite is one write a client makes whose statements can pass more than
// store.LargeArgumentBytesForTest of arguments, made through the route the client calls on the
// issue issueKey names.
type largeWrite struct {
	name  string
	write func(t *testing.T, handler http.Handler, issueKey string)
}

// largeWrites is each write of the store that can pass a statement more than 64 KiB of
// arguments: a document's version with its stored history and its blocks' texts, an upload's
// bytes, a text whose display tree is stored beside it, a phase handoff, and the JSON fields that
// carry a client's text, an ask's options and an event's payload. Each passes 90 KB or more, past
// the bound, so the statement that carries it is one the release must count.
var largeWrites = []largeWrite{
	{"a document version", func(t *testing.T, handler http.Handler, issueKey string) {
		uploadIssueDocument(t, handler, issueKey, "large-notes.md", largeMarkdown("A paragraph of the large document."))
	}},
	{"an upload's text", func(t *testing.T, handler http.Handler, issueKey string) {
		uploadVersions(t, handler, "/api/v1/issues/"+issueKey, "large-log.txt", "text/plain",
			[]byte(largeMarkdown("A line of the uploaded log.")))
	}},
	{"a comment, its stored display tree and its event's payload", func(t *testing.T, handler http.Handler, issueKey string) {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments",
			map[string]any{"body": largeMarkdown("A paragraph of the large comment.")}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the comment: status=%d body=%s", created.Code, created.Body.String())
		}
	}},
	{"a message", func(t *testing.T, handler http.Handler, issueKey string) {
		createIssueMessage(t, handler, issueKey,
			map[string]any{"body": largeMarkdown("A paragraph of the large message.")}, "alice")
	}},
	{"a phase handoff", func(t *testing.T, handler http.Handler, issueKey string) {
		// A handoff's body is one JSON object, which the handoff stores as it was written.
		body, err := json.Marshal(map[string]string{"notes": largeMarkdown("A paragraph of the large handoff.")})
		if err != nil {
			t.Fatalf("encode the handoff: %v", err)
		}
		recordedHandoff(t, handler, issueKey, "implement", string(body))
	}},
	{"an ask's options, a JSON field", func(t *testing.T, handler http.Handler, issueKey string) {
		// The question and the options share a bound of 32,000 UTF-16 units
		// (validateAskWording). A character outside ASCII is one unit and three bytes, so the
		// options' JSON passes the bound in bytes while their wording stays inside it.
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
			"question": "Which way?",
			"options": []map[string]string{
				{"label": "This way", "description": strings.Repeat("字", 30_000)},
				{"label": "That way"},
			},
			"actor": sessionActor(),
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("open the ask: status=%d body=%s", response.Code, response.Body.String())
		}
	}},
	{"an issue's spec", func(t *testing.T, handler http.Handler, issueKey string) {
		project, _, _ := strings.Cut(issueKey, "-")
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": "A large spec", "spec": largeMarkdown("A paragraph of the large spec."),
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create the issue: status=%d body=%s", response.Code, response.Body.String())
		}
	}},
}

// largeMarkdown is paragraph repeated, a paragraph at a time, to about 100 KiB of markdown.
func largeMarkdown(paragraph string) string {
	var text strings.Builder
	for line := 0; text.Len() < 100<<10; line++ {
		fmt.Fprintf(&text, "%s It is paragraph %d of the text.\n\n", paragraph, line)
	}
	return text.String()
}

// A pooled connection gives back a statement's arguments on release only when the arguments it
// counts pass store.LargeArgumentBytesForTest, and it counts only the kinds whose size it reads
// cheaply (store.CountedArgumentBytesForTest): a value pgx encodes by marshalling it is not
// counted. So every statement a large write runs, alone or in a batch, must pass its bytes in a
// kind the release counts, or a connection keeps that write for the half hour it idles.
func TestNoLargeStoreWritePassesAnArgumentTheReleaseCannotCount(t *testing.T) {
	t.Parallel()
	reader := &largeArgumentReader{largest: map[string]int{}}
	// Registered before the server, so it runs after the server's own cleanups have shut its
	// document service down, and reads a statement a write left running too.
	t.Cleanup(func() { reader.requireCounted(t) })
	handler, _, _, srv := newRegisteredTestServer(t, testServerOptions{tracer: reader})
	issue := createInteractionIssue(t, handler, "LARGE", "Large writes", "A spec.")
	for _, large := range largeWrites {
		reader.begin(large.name)
		large.write(t, handler, issue.Key)
		awaitBackgroundWrites(t, srv)
	}
	reader.begin("a write left running")
	for _, large := range largeWrites {
		if held := reader.largestOf(large.name); held <= store.LargeArgumentBytesForTest {
			t.Errorf("%s passed at most %d bytes of arguments to one statement, want more than %d: it shows nothing of a large write",
				large.name, held, store.LargeArgumentBytesForTest)
		}
	}
}

// largeArgumentReader reads the arguments of every statement its pool runs, alone or in a batch,
// and keeps each one whose arguments hold more than store.LargeArgumentBytesForTest while the
// bytes the release counts in them do not: a statement whose connection goes back to its pool
// still holding them.
type largeArgumentReader struct {
	mu sync.Mutex
	// write is the write running now, which the statements read are put down to.
	write string
	// largest is the most bytes one statement's arguments held, by write.
	largest map[string]int
	// uncounted is each statement whose large arguments the release cannot count.
	uncounted []string
}

func (r *largeArgumentReader) begin(write string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write = write
}

func (r *largeArgumentReader) largestOf(write string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.largest[write]
}

func (r *largeArgumentReader) requireCounted(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, statement := range r.uncounted {
		t.Errorf("a statement's arguments pass %d bytes in a kind the release does not count: %s",
			store.LargeArgumentBytesForTest, statement)
	}
}

func (r *largeArgumentReader) read(sql string, args []any) {
	held, counted := 0, 0
	var uncounted []string
	for index, arg := range args {
		bytes := heldBytes(reflect.ValueOf(arg), map[uintptr]struct{}{})
		counts := store.CountedArgumentBytesForTest(arg)
		held += bytes
		counted += counts
		if counts < bytes {
			uncounted = append(uncounted, fmt.Sprintf("argument %d, a %T holding %d bytes", index+1, arg, bytes))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.largest[r.write] = max(r.largest[r.write], held)
	if held > store.LargeArgumentBytesForTest && counted <= store.LargeArgumentBytesForTest {
		r.uncounted = append(r.uncounted, fmt.Sprintf("%s ran %q with %s",
			r.write, statementStart(sql), strings.Join(uncounted, ", ")))
	}
}

func (r *largeArgumentReader) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.read(data.SQL, data.Args)
	return ctx
}

func (r *largeArgumentReader) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart reads each query of the batch on its own, as the release judges them.
func (r *largeArgumentReader) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if data.Batch != nil {
		for _, queued := range data.Batch.QueuedQueries {
			r.read(queued.SQL, queued.Arguments)
		}
	}
	return ctx
}

func (r *largeArgumentReader) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (r *largeArgumentReader) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// statementStart is the first 120 bytes of sql with its runs of white space made one space,
// enough to find the statement by.
func statementStart(sql string) string {
	start := strings.Join(strings.Fields(sql), " ")
	if len(start) > 120 {
		return start[:120] + "…"
	}
	return start
}

// heldBytes is the bytes of text and binary data value holds, whatever its kind: every string and
// byte slice or array it reaches through pointers, interfaces, slices, arrays, maps and struct
// fields, each pointer read once.
func heldBytes(value reflect.Value, seen map[uintptr]struct{}) int {
	switch value.Kind() {
	case reflect.String:
		return value.Len()
	case reflect.Pointer:
		if value.IsNil() {
			return 0
		}
		if _, read := seen[value.Pointer()]; read {
			return 0
		}
		seen[value.Pointer()] = struct{}{}
		return heldBytes(value.Elem(), seen)
	case reflect.Interface:
		if value.IsNil() {
			return 0
		}
		return heldBytes(value.Elem(), seen)
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value.Len()
		}
		bytes := 0
		for index := range value.Len() {
			bytes += heldBytes(value.Index(index), seen)
		}
		return bytes
	case reflect.Map:
		bytes := 0
		for entry := value.MapRange(); entry.Next(); {
			bytes += heldBytes(entry.Key(), seen) + heldBytes(entry.Value(), seen)
		}
		return bytes
	case reflect.Struct:
		bytes := 0
		for index := range value.NumField() {
			bytes += heldBytes(value.Field(index), seen)
		}
		return bytes
	}
	return 0
}
