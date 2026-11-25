package api

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The ask routes, the inbox and open asks at a cut. Each runs its present statements under the
// cut, which the store binds to the shadows. What the shadows do not give of an ask, each answer
// leaves out: the fields of leaveOutPastAskFields, the reach of an ask's route, which the route
// roster gives now and which keeps no past state (loadAskReaches reads no roster at a cut), and
// of each reply of an ask's thread what commentsAtTheCut names. A request at no cut leaves out
// nothing.

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

// leaveOutAskWording names the wording of the ask at place, which no record gives at the cut
// where a write with no event changed it at a time no record gives (reason E,
// asof.AsksChangedAfterStatement): its question, its options, whether it takes several, and its
// urgency.
func leaveOutAskWording(ctx context.Context, place string) {
	leaveOut(ctx, place+"/question", place+"/options", place+"/multiple", place+"/urgency")
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
// places[index]: the reach of the ask's route, and of each reply what commentsAtTheCut names. One
// call of commentsAtTheCut reads the replies of every thread, as messagesAtTheCut reads every
// message of its threads, so a list of threads costs the statements of one.
func (s *server) threadsAtTheCut(ctx context.Context, threads []*askThread, places []string) error {
	var replies []model.Comment
	var at []string
	for index, thread := range threads {
		leaveOutRouteReach(ctx, thread.Route, places[index])
		reply := elementsOf(places[index] + "/replies")
		for number := range thread.Replies {
			replies, at = append(replies, thread.Replies[number].Comment), append(at, reply(number))
		}
	}
	return s.commentsAtTheCut(ctx, replies, func(index int) string { return at[index] })
}

// issueAsksAtTheCut names what an issue's ask list at a cut leaves out: of each row, the fields of
// leaveOutPastAskFields, and of a row's thread (?thread=1) what threadsAtTheCut names. asks are the
// rows' asks, in the rows' order.
func (s *server) issueAsksAtTheCut(ctx context.Context, asks []model.Ask, rows []issueAskRow) error {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return nil
	}
	at := elementsOf("")
	if err := s.leaveOutPastAskFields(ctx, asks, at); err != nil {
		return err
	}
	var threads []*askThread
	var places []string
	for index := range rows {
		if rows[index].Thread != nil {
			threads, places = append(threads, rows[index].Thread), append(places, at(index)+"/thread")
		}
	}
	return s.threadsAtTheCut(ctx, threads, places)
}

// openAsksAtTheCut names what open asks at a cut leave out of each of asks: the person an ask
// waits on, where it rests on a project default that the records at the cut do not give (reason
// D). The answer's as_of and each ask's age count from the cut in the read itself (listOpenAsks).
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
// the ask's anchored block is left out where the read skips it (anchorAtTheCut).
func (s *server) askAtTheCut(ctx context.Context, read askRead) error {
	if _, atCut := asof.CutFrom(ctx); !atCut {
		return nil
	}
	if err := s.leaveOutPastAskFields(ctx, []model.Ask{read.Ask.Ask}, func(int) string { return "/ask" }); err != nil {
		return err
	}
	return s.threadsAtTheCut(ctx, []*askThread{&read.askThread}, []string{""})
}

// inboxAtTheCut names what the inbox at a cut leaves out of each of rows (decision 10): the
// caller's snooze, which keeps no history, the fields of leaveOutPastAskFields, and of the row's
// thread what threadsAtTheCut names.
func (s *server) inboxAtTheCut(ctx context.Context, rows []inboxAsk) error {
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
	if err := s.threadsAtTheCut(ctx, threads, places); err != nil {
		return err
	}
	return s.leaveOutPastAskFields(ctx, asks, at)
}
