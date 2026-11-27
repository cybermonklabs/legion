// priority_id_fixture_test.go holds the id a priority create derives from its title to
// packages/contracts/fixtures/priority-ids.json, the one list of the cases of that rule, which the
// dashboard's browser row reads too. TestPriorityIDFromTitle reads
// priorityIDFromTitle itself and opens no store; TestPriorityIDFromTitleThroughTheRoute reads
// what POST /api/v1/priorities writes for each title, against the database storetest.Open clones
// (newTestHandlerWithStore). Every failure names the title of its case.
package api

//ci:reads packages/contracts/fixtures/priority-ids.json readPriorityIDCases reads the shared list of the rule's cases through priorityIDFixturePath (../../../../contracts/fixtures)

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

// priorityIDFixturePath is the shared list of the rule's cases, read by relative path as the other
// shared fixtures of this repository are (text/whitespace_fixture_test.go).
const priorityIDFixturePath = "../../../../contracts/fixtures/priority-ids.json"

// priorityIDCase is one case of the shared list: a title and the id a create derives from it.
// Every title gives an id.
type priorityIDCase struct {
	Title string `json:"title"`
	ID    string `json:"id"`
}

// readPriorityIDCases reads the shared list and holds it to its own shape: each case carries an
// id, and no two cases carry one id, because the route test writes every id into one store.
func readPriorityIDCases(t *testing.T) []priorityIDCase {
	t.Helper()
	raw, err := os.ReadFile(priorityIDFixturePath)
	if err != nil {
		t.Fatalf("read the shared list of priority ids: %v", err)
	}
	var fixture struct {
		Description string           `json:"description"`
		Cases       []priorityIDCase `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("parse %s: %v", priorityIDFixturePath, err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatalf("%s holds no case, so it says nothing about the id a title derives", priorityIDFixturePath)
	}
	titleOf := make(map[string]string, len(fixture.Cases))
	for _, priorityCase := range fixture.Cases {
		if priorityCase.ID == "" {
			t.Fatalf("the case of the title %q names no id", priorityCase.Title)
		}
		if other, taken := titleOf[priorityCase.ID]; taken {
			t.Fatalf("the titles %q and %q both give the id %q, and one store holds one priority of an id",
				other, priorityCase.Title, priorityCase.ID)
		}
		titleOf[priorityCase.ID] = priorityCase.Title
	}
	return fixture.Cases
}

// hashedPriorityIDOf is the id the rule gives a title with no slug, or one whose cut is a keyword
// of the issue listing: priority- and the first 12 hex characters of the SHA-256 of the title's
// UTF-8 bytes, written here apart from priorityIDHash so the shared list is held to the rule as
// the rule reads, not as the server computes it.
func hashedPriorityIDOf(title string) string {
	sum := sha256.Sum256([]byte(title))
	return "priority-" + hex.EncodeToString(sum[:])[:12]
}

// priorityIDIsHashed says the case's id is the hashed one rather than a cut of the title's slug:
// it is no start of the slug. A slug that itself begins with the word priority is a cut like any
// other, since it starts with its own id.
func priorityIDIsHashed(priorityCase priorityIDCase) bool {
	return !strings.HasPrefix(artifactSlug(priorityCase.Title), priorityCase.ID)
}

// priorityIDCutProblem says what is wrong with id as the cut of slug, or "" when nothing is. The
// id has at most priorityIDMax characters and ends at the end of a word: it is the slug whole, or
// a start of it that the slug continues past with a hyphen. The one exception is a slug whose
// first word is longer than the bound, where no start ends a word and the id is the first
// priorityIDMax characters.
func priorityIDCutProblem(slug, id string) string {
	switch {
	case len(id) > priorityIDMax:
		return fmt.Sprintf("the id %q has %d characters, past the bound of %d", id, len(id), priorityIDMax)
	case !strings.HasPrefix(slug, id):
		return fmt.Sprintf("the id %q is no start of the title's slug %q", id, slug)
	case len(id) == len(slug) || slug[len(id)] == '-':
		return ""
	}
	if firstWord, _, _ := strings.Cut(slug, "-"); len(firstWord) > priorityIDMax && len(id) == priorityIDMax {
		return ""
	}
	return fmt.Sprintf("the id %q stops inside a word of the title's slug %q", id, slug)
}

// TestPriorityIDFromTitle holds priorityIDFromTitle to every case of the shared list: each title
// derives its id, which priorityIDRefusal lets through. An id cut from the slug ends at the end of
// a word within priorityIDMax; a hashed id is the rule's hash of the title, for a title with no
// ASCII letter or digit or one whose cut is none or any.
func TestPriorityIDFromTitle(t *testing.T) {
	t.Parallel()
	for _, priorityCase := range readPriorityIDCases(t) {
		title := priorityCase.Title
		id := priorityIDFromTitle(title)
		if id != priorityCase.ID {
			t.Errorf("the title %q derives the id %q, want %q", title, id, priorityCase.ID)
		}
		if refusal := priorityIDRefusal(id); refusal != "" {
			t.Errorf("the title %q derives the id %q, which priorityIDRefusal refuses: %s", title, id, refusal)
		}
		if !priorityIDIsHashed(priorityCase) {
			if problem := priorityIDCutProblem(artifactSlug(title), id); problem != "" {
				t.Errorf("the title %q: %s", title, problem)
			}
			continue
		}
		if want := hashedPriorityIDOf(title); priorityCase.ID != want {
			t.Errorf("the title %q gives the id %q in the shared list, want the hash of the title %q", title, priorityCase.ID, want)
		}
	}
}

// TestPriorityIDFromTitleThroughTheRoute sends each title of the shared list to
// POST /api/v1/priorities with no id. Each is 201 under its id, with the title written whole; an
// id cut from the slug ends at the end of a word within priorityIDMax. A create that names an id
// one past the bound is 400 PRIORITY_INPUT naming the bound, since only a derived id is cut, and
// one that names none or any is 400 PRIORITY_INPUT, since the issue listing reads each as a
// keyword. The refused writes leave the store as it was: it holds the cases' priorities and
// nothing else.
func TestPriorityIDFromTitleThroughTheRoute(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	var accepted []string
	for _, priorityCase := range readPriorityIDCases(t) {
		title := priorityCase.Title
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
			map[string]any{"title": title}, "alice")
		if response.Code != http.StatusCreated {
			t.Errorf("the title %q: status=%d body=%.300s, want 201 under the id %q",
				title, response.Code, response.Body.String(), priorityCase.ID)
			continue
		}
		made := decodeBody[model.Priority](t, response)
		if made.ID != priorityCase.ID {
			t.Errorf("the title %q is written under the id %q, want %q", title, made.ID, priorityCase.ID)
		}
		if made.Title != strings.TrimSpace(title) {
			t.Errorf("the title %q is written as %q, want it whole", title, made.Title)
		}
		if !priorityIDIsHashed(priorityCase) {
			if problem := priorityIDCutProblem(artifactSlug(title), made.ID); problem != "" {
				t.Errorf("the title %q: %s", title, problem)
			}
		}
		accepted = append(accepted, made.ID)
	}

	named := strings.Repeat("n", priorityIDMax+1)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": named, "title": "A named id one past the bound"}, "alice")
	code, text := refusalOf(t, response)
	if response.Code != http.StatusBadRequest || code != codePriorityInput {
		t.Errorf("a create naming an id of %d characters: status=%d code=%s error=%.300q, want 400 %s",
			len(named), response.Code, code, text, codePriorityInput)
	}
	if !strings.Contains(text, fmt.Sprint(priorityIDMax)) {
		t.Errorf("a create naming an id of %d characters is refused with %.300q, want it to name the bound %d",
			len(named), text, priorityIDMax)
	}
	for _, keyword := range []string{noAdvancesFilter, anyAdvancesFilter} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
			map[string]any{"id": keyword, "title": "A named keyword"}, "alice")
		if code, text := refusalOf(t, response); response.Code != http.StatusBadRequest || code != codePriorityInput {
			t.Errorf("a create naming the id %q: status=%d code=%s error=%.300q, want 400 %s",
				keyword, response.Code, code, text, codePriorityInput)
		}
	}

	var written int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from priority_nodes where kind = $1`,
		prioritystore.KindPriority).Scan(&written); err != nil {
		t.Fatalf("count the priorities: %v", err)
	}
	if written != len(accepted) {
		t.Errorf("the store holds %d priorities, want the %d the accepted titles wrote", written, len(accepted))
	}
	listed := listedPriorityIDs(t, handler)
	slices.Sort(listed)
	slices.Sort(accepted)
	if !slices.Equal(listed, accepted) {
		t.Errorf("the live priorities are %q, want only the ids the accepted titles wrote, %q", listed, accepted)
	}
}
