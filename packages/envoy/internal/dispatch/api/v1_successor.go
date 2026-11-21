package api

// v1_successor.go is how a /api/v1 answer names the /api/v2 route that answers for it (R3.1 of
// dispatch://INFRA-4071/spec): a `Link` header with rel="successor-version", which linkSuccessor,
// mounted around every /api/v1 route (routeHandler), sets when the answer's status is written.
// The route's Successor names the /api/v2 route, and its path is filled from the request's own
// path values, with the successor's query where the /api/v1 route became a filter of it
// (successor, routes_versions.go).
//
// Rows 6, 17, 89, 91, 92, 161, 164 and 165 of the route map name a /api/v2 route whose path holds
// a value the v1 request does not carry - the artifact a slug names, the issue a document belongs
// to - and row 178 one route per project its list holds (handlerResolvedSuccessors). Each of
// their handlers records the value with successorValue once it has resolved it, into the holder
// the wrapper put on the request, and the wrapper fills the Link from it. A keyed write keeps
// those values with its stored answer, and a replay of that answer, which runs no handler, records
// them again (withSuccessorValues), so it links what the first answer linked. Where no holder is
// there, a recording is nothing, so a handler records whichever route it answers and its answer
// does not change. The Deprecation and Sunset headers and the end of /api/v1 are the router's,
// which holds every path under /api/v1 to them, a route's or not (routes/router.go).

import (
	"bufio"
	"context"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// successorValues is the holder a /api/v1 request carries for the values its handler resolves: a
// pointer on the request's context, so a layer that copies the request still writes into the one
// holder the wrapper reads.
type successorValues struct {
	values map[string][]string
}

// successorValuesKey is the request-context key of the *successorValues.
type successorValuesKey struct{}

// successorValue records value as a value of the successor's path value name - `id` for the
// artifact a slug names, `key` for the issue a document belongs to or for each project a list
// holds - in the holder r carries, once however often it is recorded, and does nothing where r
// carries none.
func successorValue(r *http.Request, name, value string) {
	holder, held := r.Context().Value(successorValuesKey{}).(*successorValues)
	if !held {
		return
	}
	if holder.values == nil {
		holder.values = map[string][]string{}
	}
	if !slices.Contains(holder.values[name], value) {
		holder.values[name] = append(holder.values[name], value)
	}
}

// storedSuccessorValue is the prefix of the names a keyed write's stored answer keeps the values
// its handler recorded under (withSuccessorValues), one name per path value: a replay of that
// answer runs no handler, so it records them again from there (recordStoredSuccessorValues), and
// links what the first answer linked. It holds a space, so no header a handler sets is one of them,
// and the read of the row takes them out of the stored headers (splitSuccessorValues), so no
// reader of those headers meets one.
const storedSuccessorValue = "successor value "

// withSuccessorValues is headers, the answer a keyed write is about to store, with the values its
// handler recorded on r (successorValue), each under storedSuccessorValue and its name: headers
// itself where it recorded none, else a copy, since headers is also the answer the client is
// written.
func withSuccessorValues(r *http.Request, headers http.Header) http.Header {
	holder, held := r.Context().Value(successorValuesKey{}).(*successorValues)
	if !held || len(holder.values) == 0 {
		return headers
	}
	stored := make(http.Header, len(headers)+len(holder.values))
	maps.Copy(stored, headers)
	for name, values := range holder.values {
		stored[storedSuccessorValue+name] = values
	}
	return stored
}

// splitSuccessorValues splits stored, a keyed write's stored answer headers as its row holds them
// (withSuccessorValues), into the headers that answer carried and the values its handler recorded,
// by path value name without the prefix; values is nil where it recorded none. The names it moves
// are taken out of stored itself, which the row's scan made for this read alone.
func splitSuccessorValues(stored http.Header) (headers http.Header, values map[string][]string) {
	for key, recorded := range stored {
		name, kept := strings.CutPrefix(key, storedSuccessorValue)
		if !kept {
			continue
		}
		if values == nil {
			values = map[string][]string{}
		}
		values[name] = recorded
		delete(stored, key)
	}
	return stored, values
}

// recordStoredSuccessorValues records on r, a replay of a keyed write, each value the first
// answer's handler recorded, as the read of its row split them out (splitSuccessorValues).
func recordStoredSuccessorValues(r *http.Request, values map[string][]string) {
	for name, recorded := range values {
		for _, value := range recorded {
			successorValue(r, name, value)
		}
	}
}

// successorRel is the relation every Link value of a /api/v1 answer names (RFC 5829).
const successorRel = `; rel="successor-version"`

// linkSource is where one value of a successor's link is read from.
type linkSource int

const (
	// fromText is text of the successor's pattern or query, written as it stands.
	fromText linkSource = iota
	// fromPath is a path value of the /api/v1 request.
	fromPath
	// fromQuery is a query value of the /api/v1 request, for a name its pattern does not hold.
	fromQuery
	// fromHandler is the value the route's handler recorded (successorValue).
	fromHandler
)

// linkPart is one piece of a successor's link: text, or the value name read from its source,
// escaped for the part of the link it stands in.
type linkPart struct {
	source linkSource
	// text is the text a fromText part writes, and the name every other part reads.
	text string
	// inQuery escapes the value as a query value rather than a path segment.
	inQuery bool
}

// successorLink is a /api/v1 route's Link header, made once when the route is mounted from the
// route's Successor, so an answer only fills in the values its request holds.
type successorLink struct {
	// path and query are the successor's /api/v2 path, after /api/v2, and its query, empty for
	// none.
	path, query []linkPart
	// resolves is the path value the route's handler records, empty where the request carries
	// every value the link needs.
	resolves string
	// unfilled is the Link of an answer that holds no value for a name the path needs: a refusal
	// before the handler resolved one (a slug that names no document, a missing issue), or a
	// request that named none. It links /api/v2, where every route is listed, and names the
	// successor in its title, since a URI reference cannot carry the pattern's braces (RFC 8288
	// allows the parameter).
	unfilled string
	// none is the Link of a successful answer whose handler resolved no value, for a list that
	// may hold none (successor.None); empty for every other route.
	none string
}

// newSuccessorLink is route's Link header as its Successor names it: a `{name}` of the pattern
// that a binding names is that binding, one the /api/v1 pattern holds is its path value, and any
// other is the value the handler records; a `{name}` of the query or of a binding is the /api/v1
// path value of that name, or its query value where the /api/v1 pattern holds no such wildcard.
func newSuccessorLink(route apiRoute) *successorLink {
	next := route.Successor
	carried := patternWildcards(route.Pattern)
	link := &successorLink{unfilled: "</api/v2>" + successorRel + `; title="` + next.Method + " /api/v2" + next.Pattern + `"`}
	if next.None != "" {
		link.none = "</api/v2" + next.None + ">" + successorRel
	}
	fromRequest := func(inQuery bool) func(string) []linkPart {
		return func(name string) []linkPart {
			if slices.Contains(carried, name) {
				return []linkPart{{source: fromPath, text: name, inQuery: inQuery}}
			}
			return []linkPart{{source: fromQuery, text: name, inQuery: inQuery}}
		}
	}
	link.path = templateParts(next.Pattern, func(name string) []linkPart {
		if binding, bound := next.Path[name]; bound {
			return templateParts(binding, fromRequest(false))
		}
		if slices.Contains(carried, name) {
			return []linkPart{{source: fromPath, text: name}}
		}
		link.resolves = name
		return []linkPart{{source: fromHandler, text: name}}
	})
	link.query = templateParts(next.Query, fromRequest(true))
	return link
}

// templateParts is template's text and its `{name}` values, each read as value says.
func templateParts(template string, value func(name string) []linkPart) []linkPart {
	var parts []linkPart
	for template != "" {
		text, rest, found := strings.Cut(template, "{")
		if text != "" {
			parts = append(parts, linkPart{text: text})
		}
		if !found {
			break
		}
		name, after, _ := strings.Cut(rest, "}")
		parts = append(parts, value(name)...)
		template = after
	}
	return parts
}

// values is the Link header of an answer of status to r, whose handler recorded recorded: one
// value per value the handler resolved, or the one the request's own values fill.
func (link *successorLink) values(r *http.Request, recorded map[string][]string, status int) []string {
	if link.resolves == "" {
		if filled, ok := link.fill(r, ""); ok {
			return []string{filled}
		}
		return link.unfilledFor(status)
	}
	resolved := recorded[link.resolves]
	if len(resolved) == 0 {
		return link.unfilledFor(status)
	}
	links := make([]string, 0, len(resolved))
	for _, value := range resolved {
		filled, ok := link.fill(r, value)
		if !ok {
			return link.unfilledFor(status)
		}
		links = append(links, filled)
	}
	return links
}

// unfilledFor is the Link of an answer of status that holds no value for a name the path needs:
// the list's own where a successful answer listed none, else the one naming the route.
func (link *successorLink) unfilledFor(status int) []string {
	if link.none != "" && status < http.StatusBadRequest {
		return []string{link.none}
	}
	return []string{link.unfilled}
}

// fill is the link with r's values and resolved, the handler's value, in place; false when one of
// them is empty, since a link with an empty segment names no route.
func (link *successorLink) fill(r *http.Request, resolved string) (string, bool) {
	var text strings.Builder
	text.WriteString("</api/v2")
	if !writeParts(&text, link.path, r, resolved) {
		return "", false
	}
	if len(link.query) > 0 {
		text.WriteByte('?')
		if !writeParts(&text, link.query, r, resolved) {
			return "", false
		}
	}
	text.WriteString(">" + successorRel)
	return text.String(), true
}

// writeParts writes parts to text, each value escaped; false at the first value that is empty.
func writeParts(text *strings.Builder, parts []linkPart, r *http.Request, resolved string) bool {
	for _, part := range parts {
		var value string
		switch part.source {
		case fromText:
			text.WriteString(part.text)
			continue
		case fromPath:
			value = r.PathValue(part.text)
		case fromQuery:
			value = r.URL.Query().Get(part.text)
		case fromHandler:
			value = resolved
		}
		if value == "" {
			return false
		}
		if part.inQuery {
			text.WriteString(url.QueryEscape(value))
		} else {
			text.WriteString(url.PathEscape(value))
		}
	}
	return true
}

// linkSuccessor serves handler, the chain of a /api/v1 route, with the Link header of route's
// Successor on every answer it writes. A route whose successor needs a value its handler resolves
// is served with a holder for it on the request.
func linkSuccessor(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	link := newSuccessorLink(route)
	return func(w http.ResponseWriter, r *http.Request) {
		answer := &successorWriter{ResponseWriter: w, link: link, request: r}
		if link.resolves != "" {
			r = r.WithContext(context.WithValue(r.Context(), successorValuesKey{}, &answer.recorded))
		}
		handler(answer, r)
	}
}

// successorWriter is a /api/v1 answer's writer, outermost of the route's writers, which sets the
// Link header when the answer's status is written: the moment the handler has resolved whatever
// it resolves, and the last one at which a header can still go out.
type successorWriter struct {
	http.ResponseWriter
	link    *successorLink
	request *http.Request
	// recorded is the holder the handler records into (successorValue).
	recorded successorValues
	// linked is true once the Link is set, or the connection was taken (Hijack).
	linked bool
}

// setLink sets the Link of an answer of status, once.
func (w *successorWriter) setLink(status int) {
	if w.linked {
		return
	}
	w.linked = true
	w.Header()["Link"] = w.link.values(w.request, w.recorded.values, status)
}

// WriteHeader sets the Link on the answer's final status; an informational one (100 Continue)
// carries none.
func (w *successorWriter) WriteHeader(status int) {
	if status >= http.StatusOK {
		w.setLink(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write sets the Link for a handler that writes a body without a status of its own, which
// net/http answers 200 for.
func (w *successorWriter) Write(body []byte) (int, error) {
	w.setLink(http.StatusOK)
	return w.ResponseWriter.Write(body)
}

// ReadFrom, Flush and Hijack are forwarded as typedWriter forwards them (response_compression.go),
// so the wrapper costs a handler none of net/http's own interfaces: the v1 event stream
// (GET /api/v1/going/requests/{id}/stream) flushes, and a file's bytes are copied with ReadFrom.
// Each sets the Link first, since each can be the moment the answer's head goes out.
func (w *successorWriter) ReadFrom(body io.Reader) (int64, error) {
	w.setLink(http.StatusOK)
	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return from.ReadFrom(body)
	}
	return io.Copy(w.ResponseWriter, body)
}

func (w *successorWriter) Flush() {
	w.setLink(http.StatusOK)
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *successorWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	// Past the upgrade the connection's bytes are the handler's frames, not an answer.
	w.linked = true
	return hijacker.Hijack()
}

// Unwrap gives http.ResponseController the writer beneath, so a handler that moves its
// connection's deadlines (the drain's read deadline) reaches it through this wrapper.
func (w *successorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
