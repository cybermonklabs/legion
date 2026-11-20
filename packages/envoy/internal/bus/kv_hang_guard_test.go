package bus

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A start whose lookup the server never answers ends at the start's hang guard, with an error that
// names the lookup, the sends that drew no answer and the guard, rather than hanging the program's
// start for as long as the server stays silent. The guard is shortened here (ensureKeyValue), to
// two seconds: each lookup is a read, waited for up to its read limit (firstReadLimit, then twice
// that) and sent again, so the guard passes during the third send and cuts it (Bounded); a send
// waiting past the first limit is copied every first limit besides (limitedRead). Each lookup but
// the first goes out behind a WARN line, one that says the lookup is sent again or one that says
// a copy of it goes out, and the lookup the guard cut is followed by neither, so the lines are one
// fewer than the lookups, and the error names the limit of a lookup that reached it, not of the
// one the guard cut.
func TestABucketLookupTheServerNeverAnswersEndsAtTheHangGuard(t *testing.T) {
	logs := captureLogs(t)
	const bucket = "envoy_unanswered_never"
	relay := startUnansweredRelayOn(t, testnats.URL(t), openSubject(bucket))
	conn, js := relayedJetStream(t, relay)
	relay.arm(1 << 30)

	_, err := ensureKeyValue(context.Background(), 2*time.Second, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if !errors.Is(err, errHangGuard) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a start whose lookup the server never answers = %v, want the start's hang guard, with context.DeadlineExceeded", err)
	}
	for _, want := range []string{"KV_" + bucket + " STREAM.INFO", "sends drew no answer when the wait ended", "no answer within ", "hang guard passed after 2s"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("a start whose lookup the server never answers = %v, want it to name %q", err, want)
		}
	}
	warns := append(logs.recordsWithMsg(t, "envoy nats stream request unanswered; sending it again"),
		logs.recordsWithMsg(t, "envoy nats stream request unanswered past its first read limit; sending a copy")...)
	// The lookup the guard cut was sent before the guard passed; wait for it to reach the relay.
	testwait.Holds(time.Second, testwait.Poll, func() bool { return relay.sent() > len(warns) })
	sends := relay.sent()
	if sends < 2 {
		t.Fatalf("lookups sent = %d, want the lookup sent again after its read limit", sends)
	}
	if len(warns) != sends-1 {
		t.Fatalf("the start logged %d lines that a lookup is sent again or copied for %d lookups sent, want %d: each line is followed by a lookup, and the lookup the hang guard cut by none", len(warns), sends, sends-1)
	}
	match := regexp.MustCompile(`no answer within ([0-9.]+[a-zµ]+)`).FindStringSubmatch(err.Error())
	if match == nil {
		t.Fatalf("a start whose lookup the server never answers = %v, want it to name the read limit of the last lookup that reached it", err)
	}
	if limit, parseErr := time.ParseDuration(match[1]); parseErr != nil || limit > 2*time.Second {
		t.Fatalf("a start whose lookup the server never answers = %v, names a read limit of %s, past the 2s guard: the limit of the lookup the guard cut, which never reached it, and not of one that did", err, match[1])
	}
}

// cancelOnUnansweredWarning ends a read run as the warning for its next send is emitted. It makes
// the race between the warning and Bounded's context check deterministic.
type cancelOnUnansweredWarning struct {
	cancel  context.CancelFunc
	request string
	warns   atomic.Int32
}

func (h *cancelOnUnansweredWarning) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelOnUnansweredWarning) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "envoy nats stream request unanswered; sending it again" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key != "request" || attr.Value.String() != h.request {
				return true
			}
			h.warns.Add(1)
			h.cancel()
			return false
		})
	}
	return nil
}

func (h *cancelOnUnansweredWarning) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *cancelOnUnansweredWarning) WithGroup(string) slog.Handler      { return h }

// A warning about another send is written from the function Bounded starts. When cancellation
// lands at that line, the send has already started; it is never a warning for a send Bounded
// declines to start.
func TestAReadAgainWarningOnlyOccursWhenTheSendStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &cancelOnUnansweredWarning{cancel: cancel, request: "warning-order STREAM.INFO"}
	logtest.SetDefault(t, handler)
	var sends atomic.Int32
	limit := readRun(20 * time.Millisecond)
	_, err := readAgain(ctx, "warning-order STREAM.INFO", limit, func() (int, error) {
		sends.Add(1)
		return 0, nats.ErrTimeout
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the canceled retry = %v, want context.Canceled", err)
	}
	// Bounded can return on the cancellation before the warned callback reaches read.
	testwait.Eventually(t, "the send whose warning canceled its wait to start", func() bool { return sends.Load() == 2 })
	if got, want := handler.warns.Load(), sends.Load()-1; got != want {
		t.Fatalf("warnings for another send = %d, sends = %d, want one fewer: a warning must follow the send it names", got, sends.Load())
	}
}

// heldUnansweredWarningError holds formatting of the first lost send's error past the next
// send's read limit, while readAgain replaces its last error and continues on another send.
type heldUnansweredWarningError struct {
	ctx     context.Context
	release <-chan struct{}
}

func (e heldUnansweredWarningError) Error() string {
	select {
	case <-e.release:
	case <-e.ctx.Done():
	}
	return "the first read drew no answer"
}

func (heldUnansweredWarningError) Unwrap() error { return nats.ErrTimeout }

// A warning can outlive its own read limit. Its error belongs to that send, not to the
// mutable last error of the caller that has already continued, which the send cannot reach:
// readAgain hands limitedRead the warning, the previous send's error in it, by value in the call
// that starts the send (sendAgain).
//
// The second send's line is held until readAgain has returned, and a copy of that send, made one
// first limit after it while it waits (limitedRead), is what answers. The run's first limit is
// 2 s here, so the first send, answered at once, makes no copy, and the second, waiting up to 4 s,
// has that long to start (Bounded starts a send only while its limit lasts). The row waits for the
// line it reads, written once its error is released, and not for a count of sends: how many
// copies went out before the answer was taken depends on how the goroutines are scheduled.
func TestAReadAgainWarningKeepsTheErrorOfItsPreviousSend(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Bounded(t, time.Minute, time.Second))
	defer cancel()
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	defer release()
	var sends atomic.Int32
	limit := readRun(2 * time.Second)
	value, err := readAgain(ctx, "held-warning STREAM.INFO", limit, func() (int, error) {
		if sends.Add(1) == 1 {
			return 0, heldUnansweredWarningError{ctx: ctx, release: released}
		}
		return 42, nil
	})
	release()
	if err != nil || value != 42 {
		t.Fatalf("the read after the held warning = %d, %v, want 42 and no error", value, err)
	}
	var held map[string]any
	testwait.Eventually(t, "the line of the send whose warning outlived its read limit", func() bool {
		for _, warn := range logs.recordsWithMsg(t, "envoy nats stream request unanswered; sending it again") {
			if warn["request"] == "held-warning STREAM.INFO" && warn["sent"] == float64(1) {
				held = warn
				return true
			}
		}
		return false
	})
	if held["error"] != "the first read drew no answer" {
		t.Fatalf("the first retry's error = %v, want the first read's error", held["error"])
	}
}

// readAgain's sends run on goroutines of their own (Bounded) and can outlive the call that started
// them, while readAgain sets its own last error again for the next send. So no function literal of
// readAgain may read a variable readAgain sets again, and readAgain takes no such variable's
// address, which a send could read through later: the warning goes to the send by value, in the
// call that starts it (sendAgain). A closure that built the warning inside the send,
// `func() (T, error) { ...lastErr...; return read() }`, compiles and passes both warning rows above
// in every plain run, since the send starts long before readAgain sets the error again; only
// -race saw it, once in three runs; and a pointer to the error handed to the send, read inside it
// (`errp := &lastErr` and then `*errp`), passed both rows under -race as well. This row refuses
// each by name in every run, and holds its reading to planted readAgains that share their error.
func TestReadAgainHandsItsSendsNothingItSetsAgain(t *testing.T) {
	for _, planted := range []struct {
		name, source string
		want         []string
	}{
		{"a closure", `package bus

func readAgain(read func() (int, error)) (int, error) {
	var lastErr error
	for sends := 1; ; sends++ {
		value, err := limited(func() (int, error) { warn(sends, lastErr); return read() })
		if err == nil {
			return value, nil
		}
		lastErr = err
	}
}
`, []string{"lastErr at planted.go:6", "sends at planted.go:6"}},
		{"a pointer", `package bus

func readAgain(read func() (int, error)) (int, error) {
	var lastErr error
	for sends := 1; ; sends++ {
		errp, n := &lastErr, sends-1
		value, err := limited(warning{sent: n, previous: errp}, read)
		if err == nil {
			return value, nil
		}
		lastErr = err
	}
}
`, []string{"&lastErr at planted.go:6"}},
	} {
		if found, err := sharedWithItsSends("planted.go", planted.source); err != nil || !slices.Equal(found, planted.want) {
			t.Fatalf("the planted readAgain with %s shares %q (%v), want %q: the reading cannot see the sharing it refuses", planted.name, found, err, planted.want)
		}
	}
	found, err := sharedWithItsSends("start_wait.go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("readAgain shares %q with its sends, variables it sets again: a send that outlives its read limit then reads the caller's later value; hand it a copy in the call that starts it (sendAgain)", found)
	}
}

// sharedWithItsSends parses the Go file at path (src, when not nil, is its text) and returns each
// variable of its readAgain that readAgain sets again after declaring it (an assignment, an
// increment, or a := that reuses it) and that a function literal of readAgain declared outside it
// reads, as "<name> at <file>:<line of the read>", and each such variable whose address readAgain
// takes anywhere, as "&<name> at <file>:<line>", sorted and once each. The parser's own scopes
// decide which declaration a name is, so a literal's own variable of the same name is not one.
func sharedWithItsSends(path string, src any) ([]string, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, src, 0)
	if err != nil {
		return nil, err
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "readAgain" {
			body = fn.Body
		}
	}
	if body == nil {
		return nil, fmt.Errorf("%s declares no readAgain", path)
	}
	setAgain := map[*ast.Object]bool{}
	mark := func(expr ast.Expr, at ast.Node) {
		if ident, ok := expr.(*ast.Ident); ok && ident.Obj != nil && ident.Obj.Kind == ast.Var && ident.Obj.Decl != at {
			setAgain[ident.Obj] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			for _, left := range node.Lhs {
				mark(left, node)
			}
		case *ast.IncDecStmt:
			mark(node.X, node)
		}
		return true
	})
	var found []string
	ast.Inspect(body, func(node ast.Node) bool {
		if address, ok := node.(*ast.UnaryExpr); ok && address.Op == token.AND {
			if ident, ok := ast.Unparen(address.X).(*ast.Ident); ok && ident.Obj != nil && setAgain[ident.Obj] {
				found = append(found, fmt.Sprintf("&%s at %s:%d", ident.Name, path, fileSet.Position(address.Pos()).Line))
			}
		}
		return true
	})
	ast.Inspect(body, func(node ast.Node) bool {
		literal, ok := node.(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(literal.Body, func(inner ast.Node) bool {
			ident, ok := inner.(*ast.Ident)
			if !ok || ident.Obj == nil || !setAgain[ident.Obj] {
				return true
			}
			if decl, ok := ident.Obj.Decl.(ast.Node); ok && decl.Pos() >= literal.Pos() && decl.End() <= literal.End() {
				return true
			}
			found = append(found, fmt.Sprintf("%s at %s:%d", ident.Name, path, fileSet.Position(ident.Pos()).Line))
			return true
		})
		return false
	})
	slices.Sort(found)
	return slices.Compact(found), nil
}
