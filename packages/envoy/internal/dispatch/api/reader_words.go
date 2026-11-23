package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// reader_words.go serves the words a reader asked about (INFRA-3231's R8): the words of their
// recorded word questions and Define uses (store.ReaderWords), which the records job
// (internal/dispatch/wordrecords) and Define record. The reviewer, Rewrite and Define read them,
// and the written rules show them (readerWords, writing_rules.go).

// codeReaderWordsForbidden refuses a person who reads another person's words.
const codeReaderWordsForbidden = "READER_WORDS_FORBIDDEN"

// readerWordsResponse is GET /api/v1/readers/{login}/words: one page of the reader's words, the
// most recently asked first, and how many words they asked about.
type readerWordsResponse struct {
	Items  []store.ReaderWord `json:"items"`
	Total  int                `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

// readsWordsOf reports whether a caller reads the recorded words of the person login (Decision 14
// of INFRA-3231's plan): any agent's token reads any reader's, because the reviewer and the
// written rules read them for the person an agent writes to, and a person signed in at a browser
// (human) reads their own alone. A person in person is a browser's caller; every bearer, a
// personal token included, is the credential an agent writes with. Every route that shows a
// reader's words holds to it: this one, the written rules and the draft route's review.
func readsWordsOf(caller model.Actor, human bool, login string) bool {
	return !human || model.CanonicalLogin(caller.ID) == model.CanonicalLogin(login)
}

// getReaderWords is GET /api/v1/readers/{login}/words. It serves the caller readsWordsOf lets read
// the reader's words, and refuses every other person, 403 READER_WORDS_FORBIDDEN.
func (s *server) getReaderWords(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	login := model.CanonicalLogin(r.PathValue("login"))
	if !readsWordsOf(actor, human, login) {
		writeError(w, codeReaderWordsForbidden, http.StatusForbidden, "a person reads their own recorded words alone; an agent's token reads any reader's")
		return
	}
	query := r.URL.Query()
	var before time.Time
	if raw := strings.TrimSpace(query.Get("before")); raw != "" {
		if before, err = time.Parse(time.RFC3339, raw); err != nil {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, "before must be an RFC 3339 time")
			return
		}
	}
	page, err := parseListPage(query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	words, total, err := s.deps.Store.ReaderWordsPage(r.Context(), login, before, page.limit, page.offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, readerWordsResponse{Items: words, Total: total, Limit: page.limit, Offset: page.offset})
}
