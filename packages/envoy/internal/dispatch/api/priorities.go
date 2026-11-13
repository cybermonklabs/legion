// priorities.go serves the five routes of a priority: the deployment-wide node work in any
// project advances. Every row it reads and writes belongs to internal/dispatch/prioritystore,
// which is the one place in the tree that writes `priority_nodes` and `priority_tuples`; this
// file is the HTTP shape of those calls and the refusals they map to.
//
// A priority belongs to no project, so each of the three writes is classed scopeDeployment
// (writeClasses, routes_table.go): a credential scoped to one project is refused
// PROJECT_SCOPE_DEPLOYMENT, and a broker token naming a service and no person is refused by
// writeNeedsASignedInPerson, as it is refused every write. No guard of this file's own stands
// between an agent and a priority: a person signed in, a personal token, the shared token every
// pod carries and a broker token naming a person each write one.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

const (
	// codePriorityInput refuses a body no priority can be written from: a title with nothing in
	// it or one longer than an issue title may be, an id that is no priority id, a title no id
	// can be derived from, and a patch that names nothing to write.
	codePriorityInput = "PRIORITY_INPUT"
	// codePriorityExists refuses a create of an id the store already holds, as a project create
	// refuses a key it holds (PROJECT_EXISTS): a second create would write its own history over
	// the first write's.
	codePriorityExists = "PRIORITY_EXISTS"
	// codePriorityNotFound answers a write addressed to an id no priority row carries.
	codePriorityNotFound = "PRIORITY_NOT_FOUND"
	// codeRankInput refuses a move to a place the order does not have, which is the code and
	// the wording the issue board's own drag is refused with (issue_rank.go).
	codeRankInput = "RANK_INPUT"
)

// priorityIDMax is the longest priority id, in bytes, which for the ASCII the grammar admits is
// characters too. The id is a btree key of priority_nodes and of priority_tuples_inbound, and
// Postgres refuses a btree entry over about 2,700 bytes, so an id with no bound reached the
// insert inside prioritystore.Create and failed it as a 500. 200 is far under that bound with
// every other column of the inbound entry beside it, and well over any name a person reads at a
// glance; a longer title derives a longer id, and the create then asks the caller to name one.
const priorityIDMax = 200

// priorityIDPattern is the grammar of a priority id: lowercase letters and digits in groups
// joined by single hyphens, which is the slug spelling a document's id already takes
// (artifactSlug, and text.artifactSlugPattern, which reads one back). An id is written into a
// path and into the `advances` filter of the issue listing, so one that spells anything else
// would be stored and then address nothing. The grammar admits one word the filter reads as a
// keyword, noAdvancesFilter, and the length is bounded by priorityIDMax; priorityIDRefusal holds
// an id to all three.
var priorityIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// priorityIDRefusal answers why id can be no priority's id, or "" when it can be one. derived
// says the create took it from the title, so the answer says so and asks for an id in id.
//
// An id over priorityIDMax is refused before the grammar is read, so the answer names its length
// rather than quoting it. noAdvancesFilter is refused although the grammar spells it, because the
// issue listing reads `advances=none` as the work under no priority (parseAdvancesFilter): a
// priority of that id would have its own page and row list the work under no priority, and no
// filter could ask for the work under it.
func priorityIDRefusal(id string, derived bool) string {
	var reason string
	switch {
	case len(id) > priorityIDMax:
		reason = fmt.Sprintf("an id is at most %d characters, and this one is %d", priorityIDMax, len(id))
	case !priorityIDPattern.MatchString(id):
		reason = fmt.Sprintf("%q is no priority id: an id is lowercase letters and digits in groups joined by single hyphens", id)
	case id == noAdvancesFilter:
		reason = fmt.Sprintf("%q is no priority id: the issue listing reads advances=%s as the work under no priority, so no filter could ask for the work under a priority of that id",
			id, noAdvancesFilter)
	default:
		return ""
	}
	if derived {
		return "the id this title derives cannot be used, because " + reason + ": name the id in id"
	}
	return reason
}

// refusePriorityTitle refuses a title longer than an issue title may be, counted as an issue
// title is (contracts.IssueTitleMax, in UTF-16 units after the trim), on a create and a rename
// alike. A title is one line a page heads itself with, and the id a create derives from it is
// bounded apart (priorityIDMax).
func refusePriorityTitle(w http.ResponseWriter, title string) bool {
	length := itemblocks.Units16(title)
	if length <= contracts.IssueTitleMax {
		return false
	}
	writeError(w, codePriorityInput, http.StatusBadRequest, fmt.Sprintf(
		"a title is at most %d characters, as an issue title is, and this one is %d", contracts.IssueTitleMax, length))
	return true
}

// priorityFrom is one node row as a route answers it. The four attributes a person writes are
// read off the row's own JSON (prioritystore.Node.Attribute) rather than walked out of the
// tuple log, which answers the same value at the cost of a read per attribute, and an attribute
// never written reads as the empty string.
func priorityFrom(node prioritystore.Node) model.Priority {
	title, _ := node.Attribute(prioritystore.AttributeTitle)
	description, _ := node.Attribute(prioritystore.AttributeDescription)
	owner, _ := node.Attribute(prioritystore.AttributeOwner)
	order, _ := node.Attribute(prioritystore.AttributeRank)
	return model.Priority{
		ID:          node.ID,
		Title:       title,
		Description: description,
		Owner:       owner,
		Rank:        order,
		State:       node.State,
		MadeAt:      node.MadeAt,
		MadeBy:      node.MadeBy,
	}
}

// listPriorities is GET /api/v1/priorities: every live priority in rank order, the work that
// advances each, and the two faults — work in flight under no priority, and priority work parked
// in `backlog` or `icebox`. The reads are priority_queries.go's, which says what each one costs.
//
// The route's two parameters are the ones declared on it (prioritiesQuery, routes_shapes.go).
// ?project=, repeatable, narrows the counts and the two fault lists to those projects and leaves
// the list of priorities whole, since a priority belongs to no project; a blank value names no
// project and is dropped rather than refused, as it narrows nothing. ?summary=true answers the
// two fault counts alone, which is what the navigation rail reads.
//
// A store holding no live priority is answered 200 with the empty list and the sentence saying
// why (priorityNoneWritten): a deployment that has written no priority yet has nothing to list
// and nothing has gone wrong.
func (s *server) listPriorities(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	projects := make([]string, 0, len(query["project"]))
	for _, project := range query["project"] {
		if named := strings.TrimSpace(project); named != "" {
			projects = append(projects, named)
		}
	}
	body, err := s.readPriorities(r.Context(), projects, query.Get("summary") == "true")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, body)
}

// createPriorityInput is the body of POST /api/v1/priorities.
type createPriorityInput struct {
	// ID is the priority's own id, the spelling every address of it carries. A body that names
	// none takes the title written as a slug (priorityIDFromTitle), as nothing in the store
	// makes an id: it is the caller's, as an issue key is. Given or derived, it is held to
	// priorityIDRefusal.
	ID string `json:"id"`
	// Title is what the priority is called, one line, and is required; it is at most
	// contracts.IssueTitleMax UTF-16 units, as an issue title is (refusePriorityTitle).
	Title string `json:"title" required:"true"`
	// Description is markdown of any length: nothing bounds it, as nothing bounds the text of
	// any other node, and a reader who wants a sentence reads its first line.
	Description string `json:"description"`
	// Owner is who holds the priority, written as the person who wrote it wrote it.
	Owner string       `json:"owner"`
	Actor *model.Actor `json:"actor"`
}

func (s *server) createPriority(w http.ResponseWriter, r *http.Request) {
	var input createPriorityInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		writeError(w, codePriorityInput, http.StatusBadRequest, "title is required")
		return
	}
	if refusePriorityTitle(w, input.Title) {
		return
	}
	input.Owner = strings.TrimSpace(input.Owner)
	id := strings.TrimSpace(input.ID)
	derived := id == ""
	if derived {
		if id = priorityIDFromTitle(input.Title); id == "" {
			writeError(w, codePriorityInput, http.StatusBadRequest,
				"this title holds no letter or digit an id can be written from: name the id in id")
			return
		}
	}
	if refusal := priorityIDRefusal(id, derived); refusal != "" {
		writeError(w, codePriorityInput, http.StatusBadRequest, refusal)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The new priority takes the last place of the one order. RankAt with neither neighbour
	// named answers the key after the last one, under the allocation lock it takes first, so two
	// creates at once write two keys rather than one (prioritystore.RankAt).
	order, err := prioritystore.RankAt(r.Context(), tx, id, "", "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	at := time.Now().UTC()
	values := map[string]string{
		prioritystore.AttributeTitle:       input.Title,
		prioritystore.AttributeDescription: input.Description,
		prioritystore.AttributeOwner:       input.Owner,
		prioritystore.AttributeRank:        order,
	}
	if err := prioritystore.Create(r.Context(), tx, id, values, actor, at); err != nil {
		if errors.Is(err, prioritystore.ErrPriorityExists) {
			writeError(w, codePriorityExists, http.StatusConflict,
				fmt.Sprintf("a priority with the id %q is already written", id))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The row the create wrote, built from what it wrote rather than read back: Create writes
	// the state live, the time and the writer this handler handed it, and nothing else touches
	// the row inside that transaction.
	WriteJSON(w, http.StatusCreated, model.Priority{
		ID:          id,
		Title:       input.Title,
		Description: input.Description,
		Owner:       input.Owner,
		Rank:        order,
		State:       prioritystore.StateLive,
		MadeAt:      at,
		MadeBy:      actor,
	})
}

// patchPriorityInput is the body of PATCH /api/v1/priorities/{id}. Each field is a pointer, so a
// write names the attributes it changes and leaves every other as it is: one write states one
// fact, which is what the tuple log then holds.
type patchPriorityInput struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Owner       *string `json:"owner"`
	// Rank moves the priority in the one order: before names the priority it lands above and
	// after the one it lands below, which is the shape the issue board's own drag takes
	// (rankInput, issue_rank.go). A rank that names neither is a move to nowhere and is refused,
	// because a priority already stands somewhere.
	Rank  *rankInput   `json:"rank"`
	Actor *model.Actor `json:"actor"`
}

// patchPriority is PATCH /api/v1/priorities/{id}: the attributes the body names, and the move
// its rank names, in one transaction. A priority whose state is `gone` takes neither: the store
// writes the node row of a live priority alone (prioritystore's liveRow), so both calls answer
// ErrNoPriority and this route answers 404 PRIORITY_NOT_FOUND, in the wording its page and its
// history already answer. The transaction is rolled back, so a refused write appends no tuple.
func (s *server) patchPriority(w http.ResponseWriter, r *http.Request) {
	var input patchPriorityInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	values := make(map[string]string, 3)
	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" {
			writeError(w, codePriorityInput, http.StatusBadRequest,
				"title is required: a write renames a priority or leaves its name alone")
			return
		}
		if refusePriorityTitle(w, title) {
			return
		}
		values[prioritystore.AttributeTitle] = title
	}
	if input.Description != nil {
		values[prioritystore.AttributeDescription] = *input.Description
	}
	if input.Owner != nil {
		values[prioritystore.AttributeOwner] = strings.TrimSpace(*input.Owner)
	}
	if len(values) == 0 && input.Rank == nil {
		writeError(w, codePriorityInput, http.StatusBadRequest,
			"a write names at least one of title, description, owner and rank")
		return
	}
	var before, after string
	if input.Rank != nil {
		if input.Rank.Before != nil {
			before = strings.TrimSpace(*input.Rank.Before)
		}
		if input.Rank.After != nil {
			after = strings.TrimSpace(*input.Rank.After)
		}
		if before == "" && after == "" {
			writeError(w, codeRankInput, http.StatusBadRequest,
				"rank names the priority this one lands above in before, the one it lands below in after, or both")
			return
		}
	}
	id := r.PathValue("id")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	at := time.Now().UTC()
	if err := prioritystore.SetAttributes(r.Context(), tx, id, values, actor, at); err != nil {
		s.refusePriorityWrite(w, id, err)
		return
	}
	if input.Rank != nil {
		if _, err := prioritystore.Move(r.Context(), tx, id, before, after, actor, at); err != nil {
			s.refusePriorityWrite(w, id, err)
			return
		}
	}
	node, held, err := prioritystore.Read(r.Context(), tx, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !held {
		writeError(w, codePriorityNotFound, http.StatusNotFound, fmt.Sprintf("no priority of the id %q", id))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, priorityFrom(node))
}

// deletePriority is DELETE /api/v1/priorities/{id}: the state `gone`, and no row removed. The
// listing then answers without it, and a read of the row still answers who ended it and when. No
// route writes `live` again, so a delete is final to a caller; what the row and its log keep is
// the record of it.
//
// A second delete is 404 PRIORITY_NOT_FOUND rather than a second `gone` tuple: the store takes a
// write of a live priority alone (prioritystore's liveRow), and a priority already gone is no
// priority to write.
func (s *server) deletePriority(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	id := r.PathValue("id")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := prioritystore.SetState(r.Context(), tx, id, prioritystore.StateGone, actor, time.Now().UTC()); err != nil {
		s.refusePriorityWrite(w, id, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// priorityHistory is GET /api/v1/priorities/{id}/history: every act stated about one priority,
// newest first, which is what the priority page's margin lists. It is prioritystore.Timeline —
// each attribute the priority was written with and each `advances` relation an issue asserted or
// retracted on it, with its writer and its moment — and no act of any other node.
//
// It is a read, so no write class stands on it (writeClasses, routes_table.go) and every
// authenticated caller makes it, as the listing is read: a priority belongs to no project, so a
// credential held to one reads this as it reads the listing.
//
// An id no priority row carries is 404 PRIORITY_NOT_FOUND, in the wording the two writes of an
// id answer (refusePriorityWrite). A priority somebody deleted still answers its history: the
// row is there, and the delete is the last act of it.
func (s *server) priorityHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	_, held, err := prioritystore.Read(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !held {
		writeError(w, codePriorityNotFound, http.StatusNotFound, fmt.Sprintf("no priority of the id %q", id))
		return
	}
	tuples, err := prioritystore.Timeline(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	acts := make([]model.PriorityAct, 0, len(tuples))
	for _, tuple := range tuples {
		acts = append(acts, priorityActFrom(tuple, id))
	}
	WriteJSON(w, http.StatusOK, acts)
}

// priorityActFrom is one tuple of the log as the history answers it. An attribute tuple holds its
// value in the far end itself (ToKind KindValue), and a relation names the node at the end that
// is not this priority, which for `advances` is the issue that states it.
func priorityActFrom(tuple prioritystore.Tuple, id string) model.PriorityAct {
	act := model.PriorityAct{
		Field:    tuple.Kind,
		Asserted: tuple.Asserted,
		Actor:    tuple.Who,
		At:       tuple.At,
	}
	switch {
	case tuple.ToKind == prioritystore.KindValue:
		act.Value = tuple.ToID
	case tuple.ToKind == prioritystore.KindPriority && tuple.ToID == id:
		act.Issue = tuple.FromID
	default:
		act.Issue = tuple.ToID
	}
	return act
}

// refusePriorityWrite is the answer a store refusal gives a caller: an id no row carries and an
// id whose priority is gone are both 404 ErrNoPriority, since a priority whose state is gone is
// no priority; a move to a place the order does not have is 400; and anything else is the
// handler error every route answers a failed statement with.
func (s *server) refusePriorityWrite(w http.ResponseWriter, id string, err error) {
	switch {
	case errors.Is(err, prioritystore.ErrNoPriority):
		writeError(w, codePriorityNotFound, http.StatusNotFound, fmt.Sprintf("no priority of the id %q", id))
	case errors.Is(err, prioritystore.ErrRankNeighbor):
		writeError(w, codeRankInput, http.StatusBadRequest, err.Error())
	default:
		s.writeHandlerError(w, err)
	}
}

// priorityIDFromTitle is the id a create takes when its body names none: the title written as a
// slug, by the rule artifactSlug writes one, so one spelling names every slug this deployment
// stores. A title holding no ASCII letter or digit derives nothing — artifactSlug would answer
// its own fallback word, which is no name of that title — and the create then asks for an id.
func priorityIDFromTitle(title string) string {
	if !strings.ContainsFunc(title, func(character rune) bool {
		return ('a' <= character && character <= 'z') ||
			('A' <= character && character <= 'Z') ||
			('0' <= character && character <= '9')
	}) {
		return ""
	}
	return artifactSlug(title)
}
