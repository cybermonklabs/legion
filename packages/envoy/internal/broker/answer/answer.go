// Package answer records every answer of the broker's API port. Middleware wraps the one handler
// that port serves (cmd/broker installs it outermost), so it sees the answer of every route, of
// the OAuth provider's library, of the approval page and of the mux's own 404 and 405, whichever
// writer wrote it. For each answer it:
//
//   - mints an answer id, which the answer carries in its X-Broker-Answer-Id header and, in a
//     JSON refusal body, as answer_id, so a refused caller has a thing to quote;
//   - counts the answer in broker_answers_total by outcome, code and caller class (NewAnswers);
//   - and, for a refusal, writes one line at WARN under RefusalMessage naming the answer id, the
//     status, the code, the method, the route, the caller's class and identity, the secret names
//     the request named (SecretNames: the first of them, up to 32 KiB of names, and how many it
//     named), and the caller's X-Correlation-Id when it sent one.
//
// A refusal is an answer with a status from 400 to 499. Three of them are counted and write no
// line, because a caller is told each again and again while nothing changes: a 429, and the OAuth
// device grant's slow_down and authorization_pending (RFC 8628 section 3.5), which a machine
// polls once an interval for as long as its person has not decided. A 5xx writes no line here
// either: the writer of each one already logs its cause.
//
// The line never holds a secret's value or the refusal's message text: it names the code, and the
// code is read from the answer (refusalCode), not the words around it.
//
// What a handler knows and the answer does not, it notes on the request's Recorder, which
// FromContext and FromWriter reach: the caller it authenticated (NoteCaller), the secrets the
// request named (NoteSecrets), and the code of a refusal whose body names none, such as the
// approval page's HTML (NoteCode). A request with no Recorder notes nothing, so a handler notes
// whether or not it runs behind Middleware.
//
// It is a leaf: the standard library and internal/metrics, so the packages that note on it
// (api, approval, oauth, requests) take nothing else through it.
package answer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/metrics"
)

// RefusalMessage is the line one refusal of the broker writes, at WARN. It is the one message a
// reader filters the broker's refusals on.
const RefusalMessage = "broker: answer refused"

// Header is the answer's own header naming its answer id, which every answer of the port carries.
const Header = "X-Broker-Answer-Id"

// correlationHeader is the header a caller names its own request id in, which the refusal line
// carries as correlation_id, at most fieldBytes of it.
const correlationHeader = "X-Correlation-Id"

// fieldBytes bounds each piece of caller-written text a refusal's line keeps (Clip): the
// correlation id, the method, each secret name, and on the airlock's proxy port the host and path.
const fieldBytes = 256

// secretNamesBytes bounds the secret names a refusal's line lists (SecretNames), counted as the
// bytes of the names it lists, each already cut to fieldBytes. Cloud Logging refuses an entry over
// 256 KiB, and a request may name thousands of names; 32 KiB of them leaves the rest of the line
// far below that.
const secretNamesBytes = 32 << 10

// The caller classes a line and the counter name: how the broker established who sent the
// request, which a handler notes once it knows (NoteCaller).
const (
	// CallerPublic is a request to a route that takes no credential, and a request no route
	// took at all (the mux's 404 and 405). It is the class a request has until a handler notes
	// another.
	CallerPublic = "public"
	// CallerUnauthenticated is a request to a route that takes a credential, refused before the
	// credential established a caller: a refusal raised in authentication.
	CallerUnauthenticated = "unauthenticated"
	// CallerLauncher is a machine's launcher, by the id of the launcher credential its proof
	// proved.
	CallerLauncher = "launcher"
	// CallerSession is an enrolled agent session or pod, by its enrollment id.
	CallerSession = "session"
	// CallerAccount is a caller BROKER_API_ACCOUNTS lists, by the name it gives the caller.
	CallerAccount = "account"
	// CallerService is a service logged in with its own ServiceAccount token, by the service and
	// the account it presented.
	CallerService = "service"
	// CallerSignIn is a person signed in to the broker's approval page, by their login.
	CallerSignIn = "sign-in"
	// CallerAirlock is a call through the airlock's proxy port, by the grant and the enrollment
	// its credential names.
	CallerAirlock = "airlock"
)

// The outcome label of broker_answers_total: what the answer was.
const (
	outcomeServed      = "served"
	outcomeRefused     = "refused"
	outcomeRateLimited = "rate_limited"
	outcomePending     = "pending"
	outcomeFailed      = "failed"
)

// The two RFC 8628 poll answers a client is told once an interval while nothing changes: the
// grant's own rate limit, and its person not having decided yet.
const (
	oauthSlowDown             = "slow_down"
	oauthAuthorizationPending = "authorization_pending"
)

// codePattern is a code the line and the counter accept as the answer's: a word the broker or
// its OAuth library chose, never free text. Anything else is recorded as HTTP_<status>, so the
// counter's code label stays a small set.
var codePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// NewAnswers registers broker_answers_total on r and answers it, for Middleware and for the
// airlock's proxy port, which count their answers on the one counter (Count, CountCarried).
func NewAnswers(r *metrics.Registry) *metrics.Counter {
	return r.NewCounter("broker_answers_total",
		"Answers of the broker's API and airlock ports, by outcome (served, refused, rate_limited, pending, failed), the code the answer named and the caller's class.")
}

// recorderKey carries the request's Recorder on its context.
type recorderKey struct{}

// Recorder is one answer's response writer and the facts its record names. It wraps the server's
// writer and carries through what a handler may take it for: Flusher, Hijacker and ReaderFrom,
// and Unwrap for http.ResponseController.
//
// It holds a refusal's body until the handler returns, so it can read the code from it and add
// answer_id to it; every other answer passes straight through.
type Recorder struct {
	http.ResponseWriter
	id string
	// correlation is the caller's X-Correlation-Id, clipped (CorrelationID); empty when it sent
	// none.
	correlation string

	// mu guards the notes and request: a handler may note from a goroutine of its own while the
	// answer is being written.
	mu sync.Mutex
	// request is the request the mux was handed, whose Pattern is the route (WithContext).
	request *http.Request
	class   string
	caller  string
	secrets SecretNames
	code    string

	// status is the answer's status, 0 until the handler has set one.
	status int
	// holding is whether body is a refusal held back from the server's writer, written on when
	// the handler returns; body keeps a refusal's bytes either way, for the code.
	holding bool
	body    bytes.Buffer
}

// Middleware records every answer next writes: it gives the answer its id, counts it on answers
// (nil counts nothing), writes a refusal's line, and adds answer_id to a JSON refusal body that
// does not hold it.
func Middleware(next http.Handler, answers *metrics.Counter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &Recorder{ResponseWriter: w, id: NewID(), correlation: CorrelationID(r), class: CallerPublic}
		r = WithContext(r, context.WithValue(r.Context(), recorderKey{}, rec))
		completed := false
		defer func() {
			// A handler that panicked answers nothing net/http will send - it closes the
			// connection or resets the stream - so the answer is counted failed, as the 500 it
			// would have been, and the panic goes on to net/http.
			if !completed {
				rec.mu.Lock()
				class := rec.class
				rec.mu.Unlock()
				Count(answers, http.StatusInternalServerError, "", class)
			}
		}()
		next.ServeHTTP(rec, r)
		completed = true
		rec.finish(answers)
	})
}

// WithContext is r.WithContext(ctx) for a handler between Middleware and the mux, which must hand
// the mux a request of its own: the request it answers is the one the recorder reads the route
// from, since the mux sets Pattern on the request it is handed and no other. With no Recorder on
// ctx it is r.WithContext(ctx) and no more.
func WithContext(r *http.Request, ctx context.Context) *http.Request {
	derived := r.WithContext(ctx)
	if rec := FromContext(ctx); rec != nil {
		rec.mu.Lock()
		rec.request = derived
		rec.mu.Unlock()
	}
	return derived
}

// FromContext is the Recorder of the request ctx belongs to, and nil for one Middleware does not
// record. Every method of Recorder takes a nil one and does nothing.
func FromContext(ctx context.Context) *Recorder {
	rec, _ := ctx.Value(recorderKey{}).(*Recorder)
	return rec
}

// FromWriter is the Recorder w is or wraps (through Unwrap, as http.ResponseController reaches a
// writer), and nil when it is none: for a writer that is handed no request, such as the approval
// page's refusals.
func FromWriter(w http.ResponseWriter) *Recorder {
	for {
		switch writer := w.(type) {
		case *Recorder:
			return writer
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return nil
		}
	}
}

// ID is the answer's id, which its X-Broker-Answer-Id header carries; empty on a nil Recorder.
func (rec *Recorder) ID() string {
	if rec == nil {
		return ""
	}
	return rec.id
}

// NoteCaller names who the broker established sent the request: class, one of the Caller
// constants, and id, that class's identity. The last note wins, so a handler that authenticates
// its own caller notes over what the route's authentication noted.
func (rec *Recorder) NoteCaller(class, id string) {
	if rec == nil {
		return
	}
	rec.mu.Lock()
	rec.class, rec.caller = class, id
	rec.mu.Unlock()
}

// NoteSecrets adds names to the secret names the request named, which a refusal's line lists
// (SecretNames).
func (rec *Recorder) NoteSecrets(names ...string) {
	if rec == nil {
		return
	}
	rec.mu.Lock()
	rec.secrets.Note(names...)
	rec.mu.Unlock()
}

// SecretNames is the secret names a request named as a refusal's line lists them: the first of
// them, in the request's order and each cut by Clip, up to secretNamesBytes of names, and a count
// of the names after them. The first name that would pass the bound ends the list, so the list is
// always a prefix of what the request named, and the names it leaves out are counted, never kept.
// Middleware's Recorder notes the API port's names on one, and the airlock's proxy port the name
// its call was handled for, so both ports' lines list names by this one rule. The zero value has
// noted none.
type SecretNames struct {
	// listed is the names the line lists, and bytes their length together.
	listed []string
	bytes  int
	// omitted counts the names noted once the list had ended.
	omitted int
}

// Note adds names to those the request named, in its order.
func (n *SecretNames) Note(names ...string) {
	for _, name := range names {
		if n.omitted == 0 {
			if name = Clip(name); n.bytes+len(name) <= secretNamesBytes {
				n.listed = append(n.listed, name)
				n.bytes += len(name)
				continue
			}
		}
		n.omitted++
	}
}

// AppendFields appends to fields, a line's attributes as slog takes them, the attributes that
// name the noted names: secrets, the names listed (an empty list for none); secrets_count, how many
// were noted; and secrets_omitted, how many the list left out, only when it left any out.
func (n *SecretNames) AppendFields(fields []any) []any {
	listed := n.listed
	if listed == nil {
		listed = []string{}
	}
	fields = append(fields, "secrets", listed, "secrets_count", len(n.listed)+n.omitted)
	if n.omitted > 0 {
		fields = append(fields, "secrets_omitted", n.omitted)
	}
	return fields
}

// NoteCode names the code of the refusal about to be written, for a writer whose body names none
// (the approval page's HTML). A code the body names wins over it (refusalCode), and an answer
// outside 400 to 499 is counted under HTTP_<status> whatever was noted: a 500 its writer answered
// in place of the refusal it noted is the 500 it is.
func (rec *Recorder) NoteCode(code string) {
	if rec == nil {
		return
	}
	rec.mu.Lock()
	rec.code = code
	rec.mu.Unlock()
}

// WriteHeader records the answer's status and sets its id header. A refusal's status is held back
// with its body until the handler returns; any other goes on at once. A 1xx informational answer
// goes on as it is, and the final status still follows it.
func (rec *Recorder) WriteHeader(status int) {
	switch {
	case rec.status != 0:
		// A second status: the server's writer reports it as superfluous. One for a held refusal
		// is dropped here, since the server's writer has been told no status yet.
		if !rec.holding {
			rec.ResponseWriter.WriteHeader(status)
		}
		return
	case status >= 100 && status < 200 && status != http.StatusSwitchingProtocols:
		rec.ResponseWriter.WriteHeader(status)
		return
	}
	rec.status = status
	rec.Header().Set(Header, rec.id)
	if refusal(status) {
		rec.holding = true
		return
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *Recorder) Write(body []byte) (int, error) {
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	if rec.holding {
		return rec.body.Write(body)
	}
	return rec.ResponseWriter.Write(body)
}

// ReadFrom keeps the server's own copy for a handler that streams a body with io.Copy, and copies
// a held refusal into what it holds.
func (rec *Recorder) ReadFrom(body io.Reader) (int64, error) {
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	from, ok := rec.ResponseWriter.(io.ReaderFrom)
	if rec.holding || !ok {
		// Through Write, which holds or passes on, without handing io.Copy this ReadFrom again.
		return io.Copy(writerOnly{rec}, body)
	}
	return from.ReadFrom(body)
}

// writerOnly hides every method of its writer but Write.
type writerOnly struct{ io.Writer }

// Flush sends on what the handler has written, a held refusal included: a handler that flushes is
// streaming, so the rest of its refusal streams through and the line reads its code from what had
// been held.
func (rec *Recorder) Flush() {
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	rec.release()
	if flusher, ok := rec.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack hands the handler the connection, sending a held refusal on first. A connection taken
// before any status is recorded as the 101 an upgrade is; what the handler writes on it is not
// this answer's.
func (rec *Recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	rec.release()
	conn, buffered, err := hijacker.Hijack()
	if err == nil && rec.status == 0 {
		rec.status = http.StatusSwitchingProtocols
	}
	return conn, buffered, err
}

// Unwrap gives http.ResponseController the server's own writer.
func (rec *Recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// release sends a held refusal on as it stands, its status and what the handler has written of
// its body, and holds nothing more.
func (rec *Recorder) release() {
	if !rec.holding {
		return
	}
	rec.holding = false
	rec.ResponseWriter.WriteHeader(rec.status)
	_, _ = rec.ResponseWriter.Write(rec.body.Bytes())
}

// finish records the answer once the handler has returned: its line when it is a refusal, its
// count, and then a held refusal's status and body, answer_id added to a JSON object that names
// none. The line is written before the body, so whoever reads the answer can find its line.
func (rec *Recorder) finish(answers *metrics.Counter) {
	if rec.status == 0 {
		// A handler that wrote nothing answers 200 with no body, which net/http sends once it
		// returns.
		rec.status = http.StatusOK
		rec.Header().Set(Header, rec.id)
	}
	rec.mu.Lock()
	request, class, caller, secrets, noted := rec.request, rec.class, rec.caller, rec.secrets, rec.code
	rec.mu.Unlock()

	body := rec.body.Bytes()
	answerCode := httpCode(rec.status)
	var fields map[string]json.RawMessage
	if refusal(rec.status) {
		fields = jsonObject(rec.Header(), body)
		answerCode = refusalCode(rec.status, fields, noted)
	}
	if Refused(rec.status, answerCode) {
		attrs := secrets.AppendFields([]any{
			"answer_id", rec.id,
			"status", rec.status,
			"code", answerCode,
			"method", Clip(request.Method),
			"route", request.Pattern,
			"caller_class", class,
			"caller", caller,
		})
		if rec.correlation != "" {
			attrs = append(attrs, "correlation_id", rec.correlation)
		}
		slog.Warn(RefusalMessage, attrs...)
	}
	Count(answers, rec.status, answerCode, class)

	if !rec.holding {
		return
	}
	rec.holding = false
	if fields != nil {
		if _, named := fields["answer_id"]; !named {
			body = withAnswerID(body, rec.id)
		}
	}
	// The handler's length is the body it wrote, which this may have lengthened.
	rec.Header().Del("Content-Length")
	rec.ResponseWriter.WriteHeader(rec.status)
	_, _ = rec.ResponseWriter.Write(body)
}

// Count counts one answer the broker wrote on answers (nil counts nothing): an answer of status
// naming answerCode, which is HTTP_<status> when it names none codePattern accepts, for a caller
// of class. Its outcome is read from the two (outcomeOf). It is the one rule of
// broker_answers_total's labels: Middleware counts every answer of the API port by it, and the
// airlock's proxy port every answer it writes itself.
func Count(answers *metrics.Counter, status int, answerCode, class string) {
	if !codePattern.MatchString(answerCode) {
		answerCode = httpCode(status)
	}
	answers.Inc(outcome(outcomeOf(status, answerCode)), code(answerCode), callerClass(class))
}

// CountCarried counts one answer the broker carried rather than wrote: a call the airlock's
// proxy port forwarded to a service, whose status is the service's own. It is served whatever
// that status is, because the service chose it and the broker refused nothing, and its code is
// HTTP_<status>.
func CountCarried(answers *metrics.Counter, status int, class string) {
	answers.Inc(outcome(outcomeServed), code(httpCode(status)), callerClass(class))
}

// The three labels of broker_answers_total, in the order every sample names them.
func outcome(value string) [2]string     { return [2]string{"outcome", value} }
func code(value string) [2]string        { return [2]string{"code", value} }
func callerClass(value string) [2]string { return [2]string{"caller_class", value} }

// refusal reports whether status is a refusal's: 400 to 499.
func refusal(status int) bool { return status >= 400 && status < 500 }

// Refused reports whether an answer of status naming answerCode is a refusal that writes its
// line under RefusalMessage: a status from 400 to 499 but a 429 and the device grant's slow_down
// and authorization_pending, which are counted and not logged (outcomeOf). The airlock's proxy
// port writes its refusal line by it, and the approval page prints the answer id by it.
func Refused(status int, answerCode string) bool {
	return outcomeOf(status, answerCode) == outcomeRefused
}

// outcomeOf is the outcome label of an answer of status naming answerCode.
func outcomeOf(status int, answerCode string) string {
	switch {
	case status == http.StatusTooManyRequests, refusal(status) && answerCode == oauthSlowDown:
		return outcomeRateLimited
	case refusal(status) && answerCode == oauthAuthorizationPending:
		return outcomePending
	case refusal(status):
		return outcomeRefused
	case status >= 500:
		return outcomeFailed
	}
	return outcomeServed
}

// refusalCode is the code a refusal of status names: its JSON body's `code` member (every API
// refusal, and oauth.WriteRefusal's), else its RFC 6749 `error` member (the OAuth library's
// own), else the code its writer noted (the approval page), else HTTP_<status> (the mux's 404 and
// 405). The first of those the answer has is its code, and one codePattern does not accept is
// HTTP_<status>.
func refusalCode(status int, fields map[string]json.RawMessage, noted string) string {
	answerCode := noted
	if named := stringMember(fields, "code"); named != "" {
		answerCode = named
	} else if named := stringMember(fields, "error"); named != "" {
		answerCode = named
	}
	if !codePattern.MatchString(answerCode) {
		return httpCode(status)
	}
	return answerCode
}

// httpCode is the code of an answer that names none.
func httpCode(status int) string { return "HTTP_" + strconv.Itoa(status) }

// jsonObject is body's members when header says it is JSON and it is one JSON object, and nil
// otherwise.
func jsonObject(header http.Header, body []byte) map[string]json.RawMessage {
	media, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || (media != "application/json" && !strings.HasSuffix(media, "+json")) {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	return fields
}

// stringMember is the string fields holds under name, and "" for none or for another JSON value.
func stringMember(fields map[string]json.RawMessage, name string) string {
	var value string
	if raw, ok := fields[name]; !ok || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// withAnswerID is body, one JSON object, with the member answer_id naming id added after its
// last member: before the closing brace, which is the last one in the body since only white
// space may follow an object.
func withAnswerID(body []byte, id string) []byte {
	end := bytes.LastIndexByte(body, '}')
	// A string always marshals.
	quoted, _ := json.Marshal(id)
	var out []byte
	out = append(out, body[:end]...)
	if !bytes.HasSuffix(bytes.TrimRight(body[:end], " \t\r\n"), []byte("{")) {
		out = append(out, ',')
	}
	out = append(out, `"answer_id":`...)
	out = append(out, quoted...)
	return append(out, body[end:]...)
}

// NewID is a fresh answer id: 16 random bytes, in hex. Middleware mints one for each answer of
// the API port, and the airlock's proxy port one for each of its own.
func NewID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// CorrelationID is the caller's X-Correlation-Id as a refusal's line keeps it: its printable
// ASCII, at most fieldBytes of it. Middleware reads it for the API port, and the airlock's proxy
// port for its own refusals.
func CorrelationID(r *http.Request) string {
	sent := r.Header.Get(correlationHeader)
	kept := make([]byte, 0, min(len(sent), fieldBytes))
	for i := 0; i < len(sent) && len(kept) < fieldBytes; i++ {
		if sent[i] >= ' ' && sent[i] <= '~' {
			kept = append(kept, sent[i])
		}
	}
	return string(kept)
}

// Clip cuts caller-written text to fieldBytes, dropping a rune the cut splits: what a refusal's
// line keeps of the method, a secret name, and on the airlock's proxy port the host and the path.
func Clip(text string) string {
	if len(text) <= fieldBytes {
		return text
	}
	return strings.ToValidUTF8(text[:fieldBytes], "")
}
