package api

import (
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
)

const (
	// sessionAnswersWindow is the window GET /api/v1/agents/{session_id}/answers reads when the
	// request names no since: the 72 hours every agent host carries on each model request.
	sessionAnswersWindow = 72 * time.Hour
	// sessionAnswersOldest bounds how far back a since may reach, so one read never walks more
	// than a week of the events index.
	sessionAnswersOldest = 7 * 24 * time.Hour
	// sessionAnswersMaxChars is the largest max_chars the route takes.
	sessionAnswersMaxChars = 100_000
)

// listSessionAnswers answers GET /api/v1/agents/{session_id}/answers: what people wrote that
// Dispatch delivered to one session since the window's start (sessionanswers.List), newest
// first, with total counting every item in the window. With max_chars the list stops after the
// item whose words take the running sum past max_chars, in UTF-16 units as an agent host counts
// its cap: a superset of what fits under the host's cap, and a body bounded at about that cap
// plus one item.
func (s *server) listSessionAnswers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	query := r.URL.Query()
	since, err := parseSessionAnswersSince(query, time.Now().UTC())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	maxChars, limited, valid := parseQueryInt(query, "max_chars", 1, sessionAnswersMaxChars)
	if limited && !valid {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"max_chars must be one integer from 1 to %d", sessionAnswersMaxChars))
		return
	}
	items, err := sessionanswers.List(r.Context(), s.deps.Store.Pool, session, since)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	response := model.SessionAnswersResponse{SessionID: session, Since: since, Total: len(items), Items: items}
	if limited {
		response.Items = sessionAnswersWithin(items, maxChars)
	}
	WriteJSON(w, http.StatusOK, response)
}

// parseSessionAnswersSince reads the window's start: the request's one since, an RFC 3339 time no
// older than sessionAnswersOldest and not after now, or sessionAnswersWindow before now.
func parseSessionAnswersSince(query url.Values, now time.Time) (time.Time, error) {
	values, present := query["since"]
	if !present {
		return now.Add(-sessionAnswersWindow), nil
	}
	if len(values) != 1 {
		return time.Time{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "since must be one RFC3339 timestamp")
	}
	since, err := time.Parse(time.RFC3339, values[0])
	if err != nil {
		return time.Time{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "since must be an RFC3339 timestamp")
	}
	since = since.UTC()
	if since.Before(now.Add(-sessionAnswersOldest)) {
		return time.Time{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "since must be no more than 7 days ago")
	}
	if since.After(now) {
		return time.Time{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "since must not be in the future")
	}
	return since, nil
}

// sessionAnswersWithin is items up to and including the one whose words take the running sum
// past maxChars, or every item when the sum never passes it. An item's words are every word it
// serves the agent: the person's own text and, on a decline, the sentences Dispatch wrote beside
// it, which are about 200 units and the whole of a decline the person wrote no note with, so a
// window of declines would otherwise answer far past what the caller asked for.
func sessionAnswersWithin(items []model.SessionAnswer, maxChars int) []model.SessionAnswer {
	sum := 0
	for index, item := range items {
		sum += itemblocks.Units16(item.Text) + itemblocks.Units16(item.DispatchWords)
		if sum > maxChars {
			return items[:index+1]
		}
	}
	return items
}
