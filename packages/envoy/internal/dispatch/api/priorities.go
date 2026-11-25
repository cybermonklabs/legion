// priorities.go serves the five routes of a priority: the deployment-wide node work in any
// project advances. Every row it reads and writes belongs to internal/dispatch/prioritystore,
// which is the one place in the tree that writes `priority_nodes` and `priority_tuples`; this
// file is the HTTP shape of those calls and the refusals they map to.
//
// A priority belongs to no project, so each of the three writes is classed scopeDeployment
// (writeClasses, routes_table.go): a credential scoped to one project is refused
// PROJECT_SCOPE_DEPLOYMENT, and a machine's broker token naming a service and no person is
// refused by writeNeedsASignedInPerson, as it is refused every write. No guard of this file's own
// stands between an agent and a priority: a person signed in, a personal token, the shared token
// every pod carries, a broker token naming a person and an account entry's broker token naming
// no person, recorded under its account, each write one.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// priorityIDMax is the longest priority id, in bytes, which for the ASCII the grammar admits is
// characters too. The id is a btree key of priority_nodes and of priority_tuples_inbound, and
// Postgres refuses a btree entry over about 2,700 bytes, so an id with no bound reached the
// insert inside prioritystore.Create and failed it as a 500. 200 is far under that bound with
// every other column of the inbound entry beside it, and well over any name a person reads at a
// glance. It is contracts.PriorityIDMax, so the refusal of an id the caller names
// (priorityIDRefusal) and the cut of an id a create derives from a title (priorityIDFromTitle)
// read one number.
const priorityIDMax = contracts.PriorityIDMax

// priorityIDPattern is the grammar of a priority id: lowercase letters and digits in groups
// joined by single hyphens, which is the slug spelling a document's id already takes
// (model.ArtifactSlug, and text.artifactSlugPattern, which reads one back). An id is written into
// a path and into the `advances` filter of the issue listing, so one that spells anything else
// would be stored and then address nothing. The grammar admits two words the filter reads as
// keywords, noAdvancesFilter and anyAdvancesFilter, and the length is bounded by priorityIDMax;
// priorityIDRefusal holds an id to all of them.
var priorityIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// priorityIDRefusal answers why an id the caller names can be no priority's id, or "" when it can
// be one. An id a create derives from the title is never refused: priorityIDFromTitle answers one
// the grammar spells, within priorityIDMax, and never a keyword.
//
// An id over priorityIDMax is refused before the grammar is read, so the answer names its length
// rather than quoting it. noAdvancesFilter and anyAdvancesFilter are refused although the grammar
// spells them, because the issue listing reads `advances=none` as the work under no priority and
// `advances=any` as the work under any (parseAdvancesFilter): a priority of either id would have
// its own page and row list the work the keyword reads, and no filter could ask for the work
// under it.
func priorityIDRefusal(id string) string {
	switch {
	case len(id) > priorityIDMax:
		return fmt.Sprintf("an id is at most %d characters, and this one is %d", priorityIDMax, len(id))
	case !priorityIDPattern.MatchString(id):
		return fmt.Sprintf("%q is no priority id: an id is lowercase letters and digits in groups joined by single hyphens", id)
	case id == noAdvancesFilter:
		return fmt.Sprintf("%q is no priority id: the issue listing reads advances=%s as the work under no priority, so no filter could ask for the work under a priority of that id",
			id, noAdvancesFilter)
	case id == anyAdvancesFilter:
		return fmt.Sprintf("%q is no priority id: the issue listing reads advances=%s as the work under any priority, so no filter could ask for the work under a priority of that id",
			id, anyAdvancesFilter)
	}
	return ""
}

// refusePriorityTitle refuses a title longer than an issue title may be, counted as an issue
// title is (contracts.IssueTitleMax, in UTF-16 units after the trim), on a create and a rename
// alike. A title is one line a page heads itself with, and the id a create derives from it is
// cut to priorityIDMax apart (priorityIDFromTitle), so a title this allows never fails a create
// for the length of the id it derives.
func refusePriorityTitle(w http.ResponseWriter, r *http.Request, title string) bool {
	length := itemblocks.Units16(title)
	if length <= contracts.IssueTitleMax {
		return false
	}
	writeError(w, r, apicode.PriorityInput, fmt.Sprintf(
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

// priorityAnswer is a write's own answer: the priority it wrote, with the tree of its description
// and whether Parse takes that description whole for a caller that sent `Dispatch-Trees: 1`, read
// where it stands (shapeBuilt) as a comment's write answers the tree of its body. The handler
// builds it before it stores the description's tree, so the store finds that reading in the
// process's cache rather than reading the description again.
func priorityAnswer(priority model.Priority, trees bool) model.Priority {
	shapeBuilt(trees, func(reader *displayTreeReader) {
		reader.textExact(&priority.DescriptionTree, &priority.DescriptionTreeExact, priority.Description)
	})
	return priority
}

// readPriorityTrees gives each listed row the tree of its description, and whether Parse takes
// that description whole (model.Priority.DescriptionTreeExact), for a caller that sent
// `Dispatch-Trees: 1`, read as a comment list reads its bodies' (displayTreeReader), so the page
// parses no description: the listing of either version calls it on the rows it answers. The rows
// must be final, since resolve fills each field registered where it stands.
func (s *server) readPriorityTrees(r *http.Request, rows []model.PriorityRow) {
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	for index := range rows {
		row := &rows[index]
		reader.textExact(&row.DescriptionTree, &row.DescriptionTreeExact, row.Description)
	}
	reader.resolve()
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
//
// Every description carries its display tree for a caller that sent `Dispatch-Trees: 1`, and
// whether Parse takes the description whole, which says whether a writer's editor may take the
// markdown in (readPriorityTrees).
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
	body, _, err := s.readPriorities(r.Context(), projects, query.Get("summary") == "true")
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.readPriorityTrees(r, body.Priorities)
	WriteJSON(w, http.StatusOK, body)
}

// createPriorityInput is the body of POST /api/v1/priorities and of POST /api/v2/priorities.
type createPriorityInput struct {
	// ID is the priority's own id, the spelling every address of it carries. A body that names
	// none takes the title written as a slug and cut to priorityIDMax at the end of a word
	// (priorityIDFromTitle), as nothing in the store makes an id: it is the caller's, as an issue
	// key is. Given or derived, it is held to priorityIDRefusal, and only a given one can be
	// refused for its length.
	ID string `json:"id"`
	// Title is what the priority is called, one line, and is required; it is at most
	// contracts.IssueTitleMax UTF-16 units, as an issue title is (refusePriorityTitle).
	Title string `json:"title" required:"true"`
	// Description is markdown of any length: nothing bounds it, as nothing bounds the text of
	// any other node, and a reader who wants a sentence reads its first line. A reader is shown
	// the tree the server reads it into (model.Priority.DescriptionTree), which holds marks
	// nested past pmdoc's bound as text, so no reader's browser parses the markdown itself.
	Description string `json:"description"`
	// Owner is who holds the priority, written as the person who wrote it wrote it.
	Owner string       `json:"owner"`
	Actor *model.Actor `json:"actor"`
}

func (s *server) createPriority(w http.ResponseWriter, r *http.Request) {
	var input createPriorityInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.createPriorityFrom(w, r, input)
}

// createPriorityFrom writes the priority input names, the work both versions' POST does once each
// has read its body.
func (s *server) createPriorityFrom(w http.ResponseWriter, r *http.Request, input createPriorityInput) {
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		writeError(w, r, apicode.PriorityInput, "title is required")
		return
	}
	if refusePriorityTitle(w, r, input.Title) {
		return
	}
	input.Owner = strings.TrimSpace(input.Owner)
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = priorityIDFromTitle(input.Title)
	} else if refusal := priorityIDRefusal(id); refusal != "" {
		writeError(w, r, apicode.PriorityInput, refusal)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The new priority takes the last place of the one order. RankAt with neither neighbour
	// named answers the key after the last one, under the allocation lock it takes first, so two
	// creates at once write two keys rather than one (prioritystore.RankAt).
	order, err := prioritystore.RankAt(r.Context(), tx, id, "", "")
	if err != nil {
		s.writeHandlerError(w, r, err)
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
			writeError(w, r, apicode.PriorityExists,
				fmt.Sprintf("a priority with the id %q is already written", id))
			return
		}
		s.writeHandlerError(w, r, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, priorityEvent(actor, id, model.PriorityCreated))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), event)
	// The row the create wrote, built from what it wrote rather than read back: Create writes
	// the state live, the time and the writer this handler handed it, and nothing else touches
	// the row inside that transaction.
	answer := priorityAnswer(model.Priority{
		ID:          id,
		Title:       input.Title,
		Description: input.Description,
		Owner:       input.Owner,
		Rank:        order,
		State:       prioritystore.StateLive,
		MadeAt:      at,
		MadeBy:      actor,
	}, wantsDisplayTrees(r))
	s.storeDisplayTrees(r.Context(), id, input.Description)
	WriteJSON(w, http.StatusCreated, answer)
}

// patchPriorityInput is the body of PATCH /api/v1/priorities/{id} and of PATCH
// /api/v2/priorities/{id}. Each field is a pointer, so a write names the attributes it changes and
// leaves every other as it is: one write states one fact, which is what the tuple log then holds.
type patchPriorityInput struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Owner       *string `json:"owner"`
	// Rank moves the priority in the one order: before names the priority it lands above and
	// after the one it lands below, which is the shape the issue board's own drag takes
	// (rankInput, issue_rank.go). A rank that names neither is a move to nowhere and is refused,
	// because a priority already stands somewhere.
	Rank *rankInput `json:"rank"`
	// Expected is what the writer's edit started from, for the title and the description this
	// same write writes: where the priority no longer stores it, somebody else wrote the
	// attribute after the edit began, and the write is refused 409 PRIORITY_CHANGED rather than
	// written over that change (priorityChangedRefusal). A write that leaves it out writes over
	// whatever is stored. Owner and rank take none.
	Expected *patchPriorityExpected `json:"expected"`
	Actor    *model.Actor           `json:"actor"`
}

// patchPriorityExpected is a patch's precondition: the value each attribute held when the
// writer's edit of it began, as the writer read it. Each names an attribute the same patch
// writes, or the patch is refused 400 PRIORITY_INPUT, since a value nothing is written over
// guards nothing.
type patchPriorityExpected struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// patchPriority is PATCH /api/v1/priorities/{id}: the attributes the body names, and the move
// its rank names, in one transaction. A priority whose state is `gone` takes neither: the store
// writes the node row of a live priority alone (prioritystore's liveRow), so both calls answer
// ErrNoPriority and this route answers 404 PRIORITY_NOT_FOUND, in the wording its page and its
// history already answer. The transaction is rolled back, so a refused write appends no tuple
// and no `priority.updated` (priorityEvent).
func (s *server) patchPriority(w http.ResponseWriter, r *http.Request) {
	var input patchPriorityInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.patchPriorityFrom(w, r, input)
}

// patchPriorityFrom writes what input names of the path's priority, the work both versions'
// PATCH does once each has read its body.
//
// A body whose expected names a value first reads the node row under its lock
// (prioritystore.ReadForWrite) and compares, in the transaction that then writes, so a second
// write naming the same value waits for this one and finds the value it wrote. One that differs
// is 409 PRIORITY_CHANGED, refused before any statement writes. A body naming no expected takes
// no lock of its own and writes as it always has.
func (s *server) patchPriorityFrom(w http.ResponseWriter, r *http.Request, input patchPriorityInput) {
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	values := make(map[string]string, 3)
	if input.Title != nil {
		title := strings.TrimSpace(*input.Title)
		if title == "" {
			writeError(w, r, apicode.PriorityInput,
				"title is required: a write renames a priority or leaves its name alone")
			return
		}
		if refusePriorityTitle(w, r, title) {
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
	if refusal := patchExpectedUnwritten(input); refusal != "" {
		writeError(w, r, apicode.PriorityInput, refusal)
		return
	}
	if len(values) == 0 && input.Rank == nil {
		writeError(w, r, apicode.PriorityInput,
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
			writeError(w, r, apicode.RankInput,
				"rank names the priority this one lands above in before, the one it lands below in after, or both")
			return
		}
	}
	id := r.PathValue("id")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	at := time.Now().UTC()
	if expected := input.Expected; expected != nil && (expected.Title != nil || expected.Description != nil) {
		stored, err := prioritystore.ReadForWrite(r.Context(), tx, id)
		if err != nil {
			s.refusePriorityWrite(w, r, id, err)
			return
		}
		if refusal := priorityChangedRefusal(id, stored, *expected); refusal != "" {
			writeError(w, r, apicode.PriorityChanged, refusal)
			return
		}
	}
	if err := prioritystore.SetAttributes(r.Context(), tx, id, values, actor, at); err != nil {
		s.refusePriorityWrite(w, r, id, err)
		return
	}
	if input.Rank != nil {
		if _, err := prioritystore.Move(r.Context(), tx, id, before, after, actor, at); err != nil {
			s.refusePriorityWrite(w, r, id, err)
			return
		}
	}
	node, held, err := prioritystore.Read(r.Context(), tx, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !held {
		writeError(w, r, apicode.PriorityNotFound, fmt.Sprintf("no priority of the id %q", id))
		return
	}
	event, err := s.appendEvent(r.Context(), tx, priorityEvent(actor, id, model.PriorityChanged))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), event)
	answer := priorityAnswer(priorityFrom(node), wantsDisplayTrees(r))
	if input.Description != nil {
		s.storeDisplayTrees(r.Context(), id, *input.Description)
	}
	WriteJSON(w, http.StatusOK, answer)
}

// patchExpectedUnwritten answers why a patch's expected names an attribute the same patch does not
// write, or "" when each value it names stands beside the attribute written over it.
func patchExpectedUnwritten(input patchPriorityInput) string {
	if input.Expected == nil {
		return ""
	}
	var refusals []string
	if input.Expected.Title != nil && input.Title == nil {
		refusals = append(refusals, "expected.title is the title an edit started from, and this write writes no title")
	}
	if input.Expected.Description != nil && input.Description == nil {
		refusals = append(refusals,
			"expected.description is the description an edit started from, and this write writes no description")
	}
	return strings.Join(refusals, "; ")
}

// priorityChangedQuoteMax is the most characters of a stored value the 409 PRIORITY_CHANGED quotes:
// a description is markdown of any length, and the refusal names what is stored now rather than
// carrying it whole.
const priorityChangedQuoteMax = 200

// priorityChangedRefusal answers why a patch's expected refuses it, or "" when each value expected
// names is the one stored now. Each attribute that differs is named with its value now, quoted and
// cut past priorityChangedQuoteMax characters with a trailing "…", so the writer reads what the
// write would have gone over. The title is compared with the stored title exactly: the route
// stores a title trimmed, and every read answers it as stored, so a writer sends the title it
// read. The description is stored as written and compared exactly too.
func priorityChangedRefusal(id string, stored prioritystore.Node, expected patchPriorityExpected) string {
	var changed []string
	for _, attribute := range []struct {
		name     string
		expected *string
	}{
		{prioritystore.AttributeTitle, expected.Title},
		{prioritystore.AttributeDescription, expected.Description},
	} {
		if attribute.expected == nil {
			continue
		}
		if now, _ := stored.Attribute(attribute.name); now != *attribute.expected {
			changed = append(changed, fmt.Sprintf("its %s is now %s", attribute.name, priorityChangedQuote(now)))
		}
	}
	if len(changed) == 0 {
		return ""
	}
	return fmt.Sprintf("priority %q was written after this edit began, so nothing was written: %s",
		id, strings.Join(changed, " and "))
}

// priorityChangedQuote is one stored value as the 409 PRIORITY_CHANGED quotes it: whole up to
// priorityChangedQuoteMax characters, and past that its first priorityChangedQuoteMax characters
// and a trailing "…". A character is a rune, so the cut never splits one.
func priorityChangedQuote(value string) string {
	head := text.HeadRunes(value, priorityChangedQuoteMax)
	if head != value {
		head += "…"
	}
	return fmt.Sprintf("%q", head)
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
	s.deletePriorityAs(w, r, actor)
}

// deletePriorityAs writes the path's priority gone as actor, the work both versions' DELETE does
// once each has settled who sends it.
func (s *server) deletePriorityAs(w http.ResponseWriter, r *http.Request, actor model.Actor) {
	id := r.PathValue("id")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	if err := prioritystore.SetState(r.Context(), tx, id, prioritystore.StateGone, actor, time.Now().UTC()); err != nil {
		s.refusePriorityWrite(w, r, id, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, priorityEvent(actor, id, model.PriorityDeleted))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), event)
	w.WriteHeader(http.StatusNoContent)
}

// priorityEvent is the `priority.updated` each of the three writes appends in its own
// transaction, after the store wrote the priority_nodes row, so a write the store refused rolls
// back with no event. It names no owner: a priority belongs to no project, issue or document, and
// the broker keys it by the priority (events.eventOwnerKey). So it wakes nobody and the outbox
// publishes it nowhere (events.Quiet); what it is for is every other reader's open priorities
// page, priority page and navigation rail, which the live socket carries it to and which refetch
// on it (packages/dispatch/web/src/api/event-refresh.ts).
func priorityEvent(actor model.Actor, id, change string) model.Event {
	return model.Event{
		Type:    "priority.updated",
		Actor:   actor,
		Payload: model.PriorityEventPayload{ID: id, Change: change},
	}
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
		s.writeHandlerError(w, r, err)
		return
	}
	if !held {
		writeError(w, r, apicode.PriorityNotFound, fmt.Sprintf("no priority of the id %q", id))
		return
	}
	acts, err := priorityActsOf(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, acts)
}

// priorityActsOf is every act stated about the priority id, newest first (prioritystore.Timeline),
// as both versions' history of one priority answers it.
func priorityActsOf(ctx context.Context, q prioritystore.Queryer, id string) ([]model.PriorityAct, error) {
	tuples, err := prioritystore.Timeline(ctx, q, id)
	if err != nil {
		return nil, err
	}
	acts := make([]model.PriorityAct, 0, len(tuples))
	for _, tuple := range tuples {
		acts = append(acts, priorityActFrom(tuple, id))
	}
	return acts, nil
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
func (s *server) refusePriorityWrite(w http.ResponseWriter, r *http.Request, id string, err error) {
	switch {
	case errors.Is(err, prioritystore.ErrNoPriority):
		writeError(w, r, apicode.PriorityNotFound, fmt.Sprintf("no priority of the id %q", id))
	case errors.Is(err, prioritystore.ErrRankNeighbor):
		writeError(w, r, apicode.RankInput, err.Error())
	default:
		s.writeHandlerError(w, r, err)
	}
}

// priorityIDFromTitle is the id a create takes when its body names none: the title written as a
// slug, by the rule model.ArtifactSlug writes one, so one spelling names every slug this
// deployment stores, and then cut to priorityIDMax (priorityIDCut). A title the slug cannot name
// takes its hash instead (priorityIDHash): one holding no ASCII letter or digit, for which
// model.ArtifactSlug would answer its own fallback word, and one the cut leaves as
// noAdvancesFilter or anyAdvancesFilter, which no priority may take (priorityIDRefusal). So every
// title refusePriorityTitle allows derives an id, and the title itself is written whole. The
// cases of the rule, at the bound, for U+0130 and U+212A and for the hash, are
// packages/contracts/fixtures/priority-ids.json.
func priorityIDFromTitle(title string) string {
	if !strings.ContainsFunc(title, func(character rune) bool {
		return ('a' <= character && character <= 'z') ||
			('A' <= character && character <= 'Z') ||
			('0' <= character && character <= '9')
	}) {
		return priorityIDHash(title)
	}
	id := priorityIDCut(model.ArtifactSlug(title))
	if id == noAdvancesFilter || id == anyAdvancesFilter {
		return priorityIDHash(title)
	}
	return id
}

// priorityIDCut cuts a slug to priorityIDMax. A slug of at most priorityIDMax characters is the id
// whole. A longer one is cut to its longest start of at most priorityIDMax characters that ends at
// the end of a word, which is where the next character of the slug is a hyphen; when the first
// word is longer than priorityIDMax, no start ends a word and the id is the first priorityIDMax
// characters. The slug is ASCII, so a byte is a character. model.ArtifactSlug is not cut, because
// a document's slug is written by it too.
func priorityIDCut(slug string) string {
	if len(slug) <= priorityIDMax {
		return slug
	}
	// slug[priorityIDMax] is the character after the longest start that fits, so the last hyphen
	// at it or before it ends the last word within the bound. The slug never begins with one.
	if end := strings.LastIndexByte(slug[:priorityIDMax+1], '-'); end > 0 {
		return slug[:end]
	}
	return slug[:priorityIDMax]
}

// priorityIDHash is the id of a title the slug cannot name: priority- and the first 12 lower-case
// hex characters of the SHA-256 of the title's UTF-8 bytes. The same title always gives the same
// id, so a second create of it is 409 PRIORITY_EXISTS as a second create of a slug is.
func priorityIDHash(title string) string {
	sum := sha256.Sum256([]byte(title))
	return "priority-" + hex.EncodeToString(sum[:6])
}
