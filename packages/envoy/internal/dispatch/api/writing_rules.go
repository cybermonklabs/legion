package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/plainwords"
)

// writing_rules.go serves the written rules of INFRA-3231's R6. The `dispatch` command refuses an
// agent's first write of a session of an ask, an issue, a comment or a message, and its first
// after each compaction, once, and shows the rules for that write's reader. The refusal is set
// here, on the server, so the deployment's choice holds in one place: the command reads it from
// GET /api/v1/settings/writing-rules, and the rules it shows from POST /api/v1/writing-rules.

// WritingRulesConfig is the refusal as the deployment set it (cmd/dispatch:
// DISPATCH_WRITING_RULES_REFUSAL and DISPATCH_WRITING_RULES_EXTRA), which cmd/dispatch checked.
type WritingRulesConfig struct {
	// Refusal is whether the command refuses the write the rules come before.
	Refusal bool
	// Extra are the rule sets shown beside the written rules, in the order the deployment named
	// them: two of the reviewer's rule sets, written for the writer, reviewer.RuleReaderWords (the
	// words the write's reader asked about before, reviewer.WrittenReaderWords) and
	// reviewer.RuleAskParts (the parts of an ask, for a write of an ask, reviewer.WrittenAskParts).
	Extra []reviewer.RuleSet
}

// writingRulesSettings is GET /api/v1/settings/writing-rules: the refusal's state, "on" or "off",
// and the rule sets it shows beside the written rules, [] for none.
type writingRulesSettings struct {
	Refusal string   `json:"refusal"`
	Extra   []string `json:"extra"`
}

// getWritingRulesSettings is GET /api/v1/settings/writing-rules.
func (s *server) getWritingRulesSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	settings := writingRulesSettings{Refusal: "off", Extra: make([]string, len(s.deps.WritingRules.Extra))}
	for i, set := range s.deps.WritingRules.Extra {
		settings.Extra[i] = string(set)
	}
	if s.deps.WritingRules.Refusal {
		settings.Refusal = "on"
	}
	WriteJSON(w, http.StatusOK, settings)
}

// writingRulesPattern is the route's pattern, and writingRulesRoute that route: one of the JSON
// routes whose body limit is its own rather than maxJSONRequestBytes (routeBody).
const (
	writingRulesPattern = "/api/v1/writing-rules"
	writingRulesRoute   = http.MethodPost + " " + writingRulesPattern
)

// writingRulesLimit is the most this route's request may hold: the largest body a JSON write
// sends (maxJSONRequestBytes), and a kilobyte for the object around it, its route and its two
// keys. The write's body is carried as a JSON value and not as a string, so no escaping makes it
// longer here than on its own route, and every write this route can name fits.
const writingRulesLimit = maxJSONRequestBytes + 1<<10

// errWritingRulesTooLarge is this route's answer to a body past its limit (routeBody).
var errWritingRulesTooLarge = errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE",
	"request body exceeds %d bytes", writingRulesLimit)

// writingRulesInput is POST /api/v1/writing-rules's body: a write the caller has not sent yet, as
// "<method> <path>" and the JSON body it would send.
type writingRulesInput struct {
	Route string          `json:"route" required:"true"`
	Body  json.RawMessage `json:"body" required:"true"`
}

// writingRulesResponse is the rules for one write: the login of the person who reads it, "" for
// none, and the rules text.
type writingRulesResponse struct {
	Reader string `json:"reader"`
	Text   string `json:"text"`
}

// writingRulesAskRoutes are the routes whose write is an ask's question, which the parts of an ask
// (reviewer.RuleAskParts) are for.
var writingRulesAskRoutes = []string{
	"POST /api/v1/issues/{key}/asks",
	"POST /api/v1/artifacts/{id}/asks",
	"PATCH /api/v1/asks/{id}",
}

// postWritingRules is POST /api/v1/writing-rules: the rules text for the reader of the write the
// body names. It writes nothing. It decides the reader as the writing check decides the reader
// of that write when it arrives (writingCheckReader), from the record and the body's fields that
// name a person or a text it answers, and with the actor the write would carry. The reader's
// words are shown to a caller readsWordsOf lets read them, as GET /api/v1/readers/{login}/words
// serves them, so the rules a person at a browser gets for another person's write leave that
// person's words out; an agent's token gets them for any reader.
func (s *server) postWritingRules(w http.ResponseWriter, r *http.Request) {
	caller, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input writingRulesInput
	limit, tooLarge := routeBody(http.MethodPost, writingRulesPattern)
	if err := decodeJSONWithin(r, &input, limit, tooLarge); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(input.Body) == 0 {
		writeError(w, "INVALID_BODY", http.StatusBadRequest, "body is required: the JSON body the write would send")
		return
	}
	pending := pendingWriteRequest(r.Context(), input.Route)
	if pending == nil {
		writeError(w, "INVALID_BODY", http.StatusBadRequest, fmt.Sprintf(
			"route %q is not \"<method> <path>\" of a write whose text a person may read", input.Route))
		return
	}
	var carried struct {
		Actor *model.Actor `json:"actor"`
	}
	// A body that is not an object carries no actor, and the caller's credential decides it.
	_ = json.Unmarshal(input.Body, &carried)
	actor, ok := s.requireActor(w, r, carried.Actor)
	if !ok {
		return
	}
	fields, err := pendingWriteInput(pending.Pattern, input.Body)
	if err != nil {
		writeError(w, "INVALID_BODY", http.StatusBadRequest, fmt.Sprintf(
			"body is not the JSON a write to %s sends: %v", pending.Pattern, err))
		return
	}
	reader, err := s.writingCheckReader(r.Context(), pending, pending.Pattern, actor, fields)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("decide the reader of %s: %w", pending.Pattern, err))
		return
	}
	text, err := s.writingRulesText(r.Context(), pending.Pattern, reader, readsWordsOf(caller, human, reader))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, writingRulesResponse{Reader: reader, Text: text})
}

// writingRulesText is the written rules (plainwords.Rules), then each rule set of
// WritingRulesConfig.Extra that applies to a write on route read by reader: the reader's words
// where readerWords allows the caller to read them (readsWordsOf), and the parts of an ask for an
// ask's write.
func (s *server) writingRulesText(ctx context.Context, route, reader string, readerWords bool) (string, error) {
	var text strings.Builder
	text.WriteString(plainwords.Rules)
	for _, set := range s.deps.WritingRules.Extra {
		var section string
		var err error
		switch set {
		case reviewer.RuleReaderWords:
			if reader == "" || !readerWords {
				continue
			}
			var words []string
			if words, err = s.readerWords(ctx, reader); err != nil || len(words) == 0 {
				break
			}
			section, err = reviewer.WrittenReaderWords(reader, words)
		case reviewer.RuleAskParts:
			if !slices.Contains(writingRulesAskRoutes, route) {
				continue
			}
			section, err = reviewer.WrittenAskParts()
		}
		if err != nil {
			return "", fmt.Errorf("the written rules of a write on %s: %w", route, err)
		}
		if section != "" {
			text.WriteString("\n" + section)
		}
	}
	return text.String(), nil
}

// readerWords answers the words the person login asked about before, the most recently asked
// first: R8's records (store.ReaderWords), the words of each text in which the records job found
// the person asking what an agent's word means, saying they could not understand a text or
// complaining about its words, and of each Define use.
func (s *server) readerWords(ctx context.Context, login string) ([]string, error) {
	words, err := s.deps.Store.ReaderWords(ctx, model.CanonicalLogin(login), time.Time{})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(words))
	for i, word := range words {
		out[i] = word.Word
	}
	return out, nil
}

// pendingWriteInput decodes body as the request of the route pattern into the type the reader
// rule of that route reads (writingCheckReader): a new issue's, a comment's or a message's. Every
// other rule reads the record alone, and answers nil.
func pendingWriteInput(pattern string, body json.RawMessage) (any, error) {
	switch writingCheckReaderRules[pattern] {
	case readerNewIssue:
		return decodedAs[createIssueInput](body)
	case readerComment:
		return decodedAs[commentInput](body)
	case readerMessage:
		return decodedAs[createMessageInput](body)
	}
	return nil, nil
}

// decodedAs is body decoded as a T, the value and not a pointer, as a route's handler hands its
// decoded request to writingCheckReader.
func decodedAs[T any](body json.RawMessage) (any, error) {
	var input T
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, err
	}
	return input, nil
}

// pendingWriteKey is the context key under which pendingWriteRequest hands its request to the
// handler that receives it.
type pendingWriteKey struct{}

// pendingWriteRoutes routes a write the caller has not sent as the server routes one: each route
// of writingCheckReaderRules, whose handler hands back the request it received, its Pattern and
// path values set.
var pendingWriteRoutes = sync.OnceValue(func() *http.ServeMux {
	mux := http.NewServeMux()
	for route := range writingCheckReaderRules {
		mux.HandleFunc(route, func(_ http.ResponseWriter, r *http.Request) {
			if received, ok := r.Context().Value(pendingWriteKey{}).(**http.Request); ok {
				*received = r
			}
		})
	}
	return mux
})

// pendingWriteRequest is the request route, "<method> <path>", would be as the server hands it to
// its handler: its Pattern one of writingCheckReaderRules and its path values set. A query on
// the path is left out. It is nil where route is no such write.
func pendingWriteRequest(ctx context.Context, route string) *http.Request {
	method, target, found := strings.Cut(strings.TrimSpace(route), " ")
	if !found {
		return nil
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(target))
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return nil
	}
	parsed.RawQuery = ""
	var received *http.Request
	request, err := http.NewRequestWithContext(context.WithValue(ctx, pendingWriteKey{}, &received), method, parsed.String(), nil)
	if err != nil {
		return nil
	}
	pendingWriteRoutes().ServeHTTP(discardedAnswer{}, request)
	return received
}

// discardedAnswer is the answer pendingWriteRoutes writes to a route it does not serve, which
// nobody reads.
type discardedAnswer struct{}

func (discardedAnswer) Header() http.Header         { return http.Header{} }
func (discardedAnswer) Write(b []byte) (int, error) { return len(b), nil }
func (discardedAnswer) WriteHeader(int)             {}
