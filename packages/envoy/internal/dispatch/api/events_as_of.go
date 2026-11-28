package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The three event logs at a cut (decision 13): the log of an issue (listIssueEvents), of a
// document (listArtifactEvents) and the whole log (listEventLog). Each
// runs its present statement under the cut, so it reads the events shadow: the events whose
// transaction began at or before the cut. An event carries its payload as its write stored it.
// The read adds to a payload what it adds at the present (readEventRows), from the shadows: the
// fields of an ask (attachAskEventFields), and the marks of a post (attachPostMarks), which give a
// post its edit at the cut and its present delete (decision 14). So a post that its author deleted
// after the cut gives its own events with no body, and a reply's event keeps the preview that it
// froze of the post. Each page leaves out what the records at the cut do not give
// (eventPageAtTheCut).

// newestEventIDAtTheCut is the largest id of the events at the cut ctx carries, or 0 when no event
// is at or before the cut (asof.NewestEventIDStatement). A page of the whole log at a cut ends its
// id range at this id (decision 13). The events shadow already leaves out each event written after
// the cut. The bound stops the page that reaches the newest event at the cut before it reads each
// event that was written after the cut.
func (s *server) newestEventIDAtTheCut(ctx context.Context) (int64, error) {
	var newest *int64
	if err := s.deps.Store.Pool.QueryRow(ctx, asof.NewestEventIDStatement).Scan(&newest); err != nil {
		return 0, fmt.Errorf("read the newest event id at the cut: %w", err)
	}
	if newest == nil {
		return 0, nil
	}
	return *newest, nil
}

// eventPageAtTheCut names what a page of an event log at the cut ctx carries leaves out of
// events, the events of the answer at /0, /1 and on (decision 5):
//   - of each ask event, referenced_by_count: the reference graph gives it now, and keeps no
//     history (reason G). A read at a cut reads no count (queueAskDetails);
//   - of each event of an ask, the revision of its parts, the id of each option and the id of the
//     answer (reason P): at no cut the read adds them to the payload from the present ask_blocks
//     (attachAskEventFields), which a read at a cut does not read (queueAskDetails). No writer
//     stores them in an ask's events, so no record gives them at the cut. An event's own wording
//     is the payload its write stored, so the answer gives it;
//   - of each event of a comment or a message that its author deleted after the cut, the body,
//     which attachPostMarks took out with the present delete of the post (reason T, decision 14).
//     An event of a post deleted at or before the cut gave no body at the cut either, so nothing
//     is left out of it.
//
// It reads each such payload again for what it names, at a cut only. A request at no cut leaves
// out nothing.
func eventPageAtTheCut(ctx context.Context, events []model.Event) error {
	cut, atCut := asof.CutFrom(ctx)
	if !atCut {
		return nil
	}
	at := elementsOf("")
	for index := range events {
		kind := events[index].Type
		_, carriesBody := postBodyEventKinds[kind]
		if !carriesBody && !askEventKinds[kind] {
			continue
		}
		raw, ok := events[index].Payload.(json.RawMessage)
		if !ok {
			return fmt.Errorf("decode %s payload: expected raw JSON", kind)
		}
		place := at(index) + "/payload"
		if carriesBody {
			var mark struct {
				DeletedAt *time.Time `json:"deleted_at"`
			}
			if err := json.Unmarshal(raw, &mark); err != nil {
				return fmt.Errorf("decode %s payload: %w", kind, err)
			}
			if deletedSinceTheCut(mark.DeletedAt, cut) {
				leaveOut(ctx, place+"/body")
			}
			continue
		}
		var ask struct {
			Options []struct{} `json:"options"`
			Answer  *struct{}  `json:"answer"`
		}
		if err := json.Unmarshal(raw, &ask); err != nil {
			return fmt.Errorf("decode %s payload: %w", kind, err)
		}
		leaveOut(ctx, place+"/"+model.ReferencedByCountKey)
		leaveOutAskParts(ctx, place, len(ask.Options), ask.Answer != nil)
	}
	return nil
}

// leaveOutAskParts names the parts of the ask at place, which the present ask_blocks give and no
// record gives at a cut, since no writer stores them in an ask's events (reason P): its revision,
// the id of each of its options options, and the id of its answer when it is answered. Every
// answer that gives an ask at a cut names them through it.
func leaveOutAskParts(ctx context.Context, place string, options int, answered bool) {
	leaveOut(ctx, place+"/revision")
	option := elementsOf(place + "/options")
	for index := range options {
		leaveOut(ctx, option(index)+"/id")
	}
	if answered {
		leaveOut(ctx, place+"/answer/id")
	}
}
