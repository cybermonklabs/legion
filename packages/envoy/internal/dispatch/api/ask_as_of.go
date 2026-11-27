package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The ask routes, the inbox, open asks and an agent's answers at a cut. Each runs its present
// statements under the cut, which the store binds to the shadows. What the shadows do not give of
// an ask, each answer leaves out: the fields of leaveOutPastAskFields, the reach of an ask's
// route, which the route roster gives now and which keeps no past state (loadAskReaches reads no
// roster at a cut), of each reply of an ask's thread what commentsAtTheCut names, and of an
// agent's answers the question of each ask whose wording no record gives (sessionAnswersAtTheCut).
// A request at no cut leaves out nothing.

// asksChangedAfterTheCut reads, of the asks ids names, the ids of the asks that changed after cut
// and the ids of those of them whose wording at cut no record gives (asof.AsksChangedAfterStatement).
func (s *server) asksChangedAfterTheCut(ctx context.Context, ids []string, cut asof.Cut) (changed, wordingUnknown map[string]bool, err error) {
	rows, err := s.deps.Store.Pool.Query(ctx, asof.AsksChangedAfterStatement, ids, cut.Time())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	changed, wordingUnknown = map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var id string
		var unknown bool
		if err := rows.Scan(&id, &unknown); err != nil {
			return nil, nil, err
		}
		changed[id] = true
		if unknown {
			wordingUnknown[id] = true
		}
	}
	return changed, wordingUnknown, rows.Err()
}

// askPartsThen is the part state that the newest state event of an ask at a cut stored, as
// public.events holds it (asof.AskPartsAtTheCutStatement): its revision, a positive whole number,
// and its options and its answer with the part id that the event's write stored in each, never
// one that a read adds from the present tables (attachAskEventFields).
type askPartsThen struct {
	revision int
	options  []model.AskOption
	answer   *model.AskAnswer
}

// confirms reports whether then, the part state that the ask's newest state event at the cut
// stored, is the part state of shown, the ask as a read shows it, with the revision and the part
// ids that the block index holds now (refs.AskPartIDs): the same positive revision, as many
// options with the same part id each, and an answer where shown holds one, with the same part id
// (reason P). An option or an answer to which the index gives no part id confirms nothing, and
// neither does one whose id the event does not store. An ask with no option and no answer stands
// on its revision alone, which counts its question's part too.
func (then askPartsThen) confirms(shown model.Ask) bool {
	if then.revision <= 0 || then.revision != shown.Revision || len(then.options) != len(shown.Options) ||
		(then.answer == nil) != (shown.Answer == nil) {
		return false
	}
	for index, option := range shown.Options {
		if option.ID == "" || then.options[index].ID != option.ID {
			return false
		}
	}
	return shown.Answer == nil || shown.Answer.ID != "" && then.answer.ID == shown.Answer.ID
}

// askPartsAtTheCut reads, of the asks ids names, the part state that the newest state event of
// each at cut stored (asof.AskPartsAtTheCutStatement), by ask id, in one statement over every id.
// An ask with no state event at the cut is not in it, and neither is one whose newest such event
// stored no positive whole revision, or whose payload holds options or an answer that this build
// cannot read: no record gives such an ask's parts.
func (s *server) askPartsAtTheCut(ctx context.Context, ids []string, cut asof.Cut) (map[string]askPartsThen, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, asof.AskPartsAtTheCutStatement, ids, cut.Time())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stored map[string]askPartsThen
	for rows.Next() {
		var id string
		var revision, options, answer []byte
		if err := rows.Scan(&id, &revision, &options, &answer); err != nil {
			return nil, err
		}
		number, err := strconv.Atoi(string(revision))
		if err != nil || number <= 0 {
			continue
		}
		then := askPartsThen{revision: number}
		if options != nil && json.Unmarshal(options, &then.options) != nil ||
			answer != nil && json.Unmarshal(answer, &then.answer) != nil {
			continue
		}
		if stored == nil {
			stored = map[string]askPartsThen{}
		}
		stored[id] = then
	}
	return stored, rows.Err()
}

// askPartsGivenAtTheCut reads, of the asks ids names, the ones whose parts a past-time answer that
// holds no read of the ask itself gives from the present tables: an event page, whose payloads a
// read fills with the ask's present parts (attachAskEventFields), and the block of a reply that
// names a part of an ask. An ask's parts are given where its newest state event at cut stored a
// revision and the part ids that the block index holds now for that event's options and answer,
// matched as a read matches an event payload's (askEventShell, attachAskPartIDs), and where the
// ask did not change after the cut (asof.AsksChangedAfterStatement, which counts an ask whose
// wording at the cut no record gives). Each statement is sent for the asks that the one before it
// leaves, so a cut whose events store no revision sends one statement.
func (s *server) askPartsGivenAtTheCut(ctx context.Context, ids []string, cut asof.Cut) (map[string]bool, error) {
	stored, err := s.askPartsAtTheCut(ctx, ids, cut)
	if err != nil {
		return nil, err
	}
	shown := make([]*model.Ask, 0, len(stored))
	for id, then := range stored {
		// The read below writes the present ids into the ask it reads, so it reads copies: then
		// keeps the ids that the event stored.
		ask := &model.Ask{ID: id, Options: slices.Clone(then.options)}
		if then.answer != nil {
			answer := *then.answer
			ask.Answer = &answer
		}
		shown = append(shown, ask)
	}
	if len(shown) == 0 {
		return nil, nil
	}
	if err := attachAskPartIDs(ctx, s.deps.Store.Pool, shown); err != nil {
		return nil, err
	}
	var confirmed []string
	for _, ask := range shown {
		if stored[ask.ID].confirms(*ask) {
			confirmed = append(confirmed, ask.ID)
		}
	}
	if len(confirmed) == 0 {
		return nil, nil
	}
	changed, _, err := s.asksChangedAfterTheCut(ctx, confirmed, cut)
	if err != nil {
		return nil, err
	}
	given := make(map[string]bool, len(confirmed))
	for _, id := range confirmed {
		if !changed[id] {
			given[id] = true
		}
	}
	return given, nil
}

// leaveOutAskWording names the wording of the ask at place, which no record gives at the cut
// where a write with no event changed it at a time no record gives (reason E,
// asof.AsksChangedAfterStatement): its question, its options, whether it takes several, and its
// urgency, and the tree of its question where the answer carries display trees (trees). The
// trees of the options are members of the options, so they go out with them.
func leaveOutAskWording(ctx context.Context, place string, trees bool) {
	leaveOut(ctx, place+"/question", place+"/options", place+"/multiple", place+"/urgency")
	if trees {
		leaveOut(ctx, place+"/question_tree")
	}
}

// leaveOutRouteReach names, for an item whose route is route, the route's status, its holder and
// the reason for its status at place (an issue's, the ask read's own place, or a thread's): the
// roster that gives them now keeps no past state. An item with no route holds none of them at any
// time, so nothing is left out.
func leaveOutRouteReach(ctx context.Context, route *string, place string) {
	if route != nil {
		leaveOut(ctx, place+"/route_status", place+"/route_holder", place+"/route_status_reason")
	}
}

// threadsAtTheCut names what a past-time answer leaves out of each of threads, the thread at
// places[index]: the reach of the ask's route, and of each reply what commentsAtTheCut names.
// trees is whether the answer carries display trees. One call of commentsAtTheCut reads the
// replies of every thread, as messagesAtTheCut reads every message of its threads, so a list of
// threads costs the statements of one.
func (s *server) threadsAtTheCut(ctx context.Context, threads []*askThread, places []string, trees bool) error {
	var replies []model.Comment
	var at []string
	for index, thread := range threads {
		leaveOutRouteReach(ctx, thread.Route, places[index])
		reply := elementsOf(places[index] + "/replies")
		for number := range thread.Replies {
			replies, at = append(replies, thread.Replies[number].Comment), append(at, reply(number))
		}
	}
	_, err := s.commentsAtTheCut(ctx, replies, func(index int) string { return at[index] }, trees)
	return err
}

// issueAsksAtTheCut names what an issue's ask list at a cut leaves out: of each row, the fields of
// leaveOutPastAskFields, and of a row's thread (?thread=1) what threadsAtTheCut names. asks are the
// rows' asks, in the rows' order; trees is whether the rows carry display trees.
func (s *server) issueAsksAtTheCut(ctx context.Context, asks []model.Ask, rows []issueAskRow, trees bool) error {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return nil
	}
	at := elementsOf("")
	if err := s.leaveOutPastAskFields(ctx, asks, at, trees); err != nil {
		return err
	}
	var threads []*askThread
	var places []string
	for index := range rows {
		if rows[index].Thread != nil {
			threads, places = append(threads, rows[index].Thread), append(places, at(index)+"/thread")
		}
	}
	return s.threadsAtTheCut(ctx, threads, places, trees)
}

// openAsksAtTheCut names what open asks at a cut leave out of each of asks: the person an ask
// waits on, where it rests on a project default that the records at the cut do not give (reason
// D). The answer's as_of and each ask's age count from the cut in the read itself (listOpenAsks).
// Each ask is open at the cut, where the record gives its question and its urgency (the comment of
// the route's entry in asof.AbsentFields says why), so neither is left out.
func openAsksAtTheCut(ctx context.Context, asks []openAsk) {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return
	}
	at := elementsOf("/asks")
	for index := range asks {
		if asks[index].WaitingOnPerson == "" {
			leaveOut(ctx, at(index)+"/waiting_on_person")
		}
	}
}

// askAtTheCut names what the read of one ask at a cut leaves out: the fields of
// leaveOutPastAskFields of the ask, and of its thread what threadsAtTheCut names. The place of
// the ask's anchored block is left out where the read skips it (anchorAtTheCut), and with the
// anchor's block id where no record gives that id at the cut. trees is whether the read carries
// display trees.
func (s *server) askAtTheCut(ctx context.Context, read askRead, trees bool) error {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return nil
	}
	undatedBlocks, err := s.askFieldsAtTheCut(ctx, []model.Ask{read.Ask.Ask}, func(int) string { return "/ask" }, trees)
	if err != nil {
		return err
	}
	if undatedBlocks[read.Ask.ID] {
		leaveOut(ctx, "/ask/anchor_block")
	}
	return s.threadsAtTheCut(ctx, []*askThread{&read.askThread}, []string{""}, trees)
}

// inboxAtTheCut names what the inbox at a cut leaves out of each of rows (decision 10): the
// caller's snooze, which keeps no history, the fields of leaveOutPastAskFields, and of the row's
// thread what threadsAtTheCut names. trees is whether the rows carry display trees.
func (s *server) inboxAtTheCut(ctx context.Context, rows []inboxAsk, trees bool) error {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return nil
	}
	asks := make([]model.Ask, len(rows))
	threads := make([]*askThread, len(rows))
	places := make([]string, len(rows))
	at := elementsOf("")
	for index := range rows {
		asks[index] = rows[index].Ask
		leaveOut(ctx, at(index)+"/snoozed_until")
		threads[index], places[index] = &rows[index].Thread, at(index)+"/thread"
	}
	if err := s.threadsAtTheCut(ctx, threads, places, trees); err != nil {
		return err
	}
	return s.leaveOutPastAskFields(ctx, asks, at, trees)
}

// sessionAnswersAtTheCut names what an agent's answers at a cut leave out of each of items, the
// answer's items in its order: the question of the ask that an item is on (an answer, a decline,
// or a reply in the ask's thread), where the ask's wording at the cut no record gives (reason E,
// asof.AsksChangedAfterStatement). The asks shadow gives the wording of the ask's newest event at
// the cut, which a restore changed with no event at a time no record gives. One statement reads
// every ask of the items, and none is sent where no item is on an ask. A request at no cut leaves
// out nothing and sends no statement.
func (s *server) sessionAnswersAtTheCut(ctx context.Context, items []model.SessionAnswer) error {
	cut, atCut := asof.CutFrom(ctx)
	if !atCut {
		return nil
	}
	var ids []string
	for index := range items {
		if ask := items[index].Ask; ask != nil {
			ids = append(ids, ask.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// An ask can be on several items (its answer, its decline, the replies in its thread); the
	// statement reads each ask once.
	slices.Sort(ids)
	_, wordingUnknown, err := s.asksChangedAfterTheCut(ctx, slices.Compact(ids), cut)
	if err != nil {
		return fmt.Errorf("read which asks of an agent's answers have no wording at the cut: %w", err)
	}
	at := elementsOf("/items")
	for index := range items {
		if ask := items[index].Ask; ask != nil && wordingUnknown[ask.ID] {
			leaveOut(ctx, at(index)+"/ask/question")
		}
	}
	return nil
}
