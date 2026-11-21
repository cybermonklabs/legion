// Package fleetalert writes the daemon's error lines for the cases no agent can hear (R6): the
// project's controller is the one out of reach, Envoy lists nobody, the daemon cannot read Envoy's
// session list at all, or a claim's newer process is still locked out by an older one that will not
// stop (R3).
//
// Every notice of R5 goes to a session over Envoy, so none of these cases can reach a person that
// way. The line goes outside Envoy instead: the deployment repository's `legion/` program holds a
// log-match alert policy over the daemons' own containers which matches Prefix in the entry's
// `jsonPayload.msg` and opens an incident in Slack's #fleet-alerts and under the infrastructure
// lead's Envoy role, as `legion/sessions_readonly_alert.go` does for a statement refused in a
// read-only transaction. Prefix is therefore a contract between the two repositories: this
// package's test holds it, and the policy's filter names it.
//
// A line is written once per case and claim until that case ends, so a case that holds for hours
// opens one incident rather than one a minute, and the daemon's store keeps which lines are open
// (Load), so a case that holds through a restart opens one incident rather than one a boot. The
// caller says when a case ends (Close), which is the moment the next Open writes again.
package fleetalert

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/timewords"
)

// Prefix is the fixed text every line starts with, and the whole of what the alert policy
// matches. It changes in neither repository alone.
const Prefix = "LEGION FLEET ALERT"

// ListSilentFor is how long the daemon's reads of Envoy's session list fail, or find no live
// session, before the line of NoLiveSession or SessionListUnreadable is written (R6).
const ListSilentFor = 20 * time.Minute

// Case is one of the cases a person is told of: Word is what the line's `case` field holds and
// the policy's documentation names, and Sentence is what the line says after Prefix.
type Case struct {
	Word     string
	Sentence string
}

var (
	// ControllerUnreachable is the controller still out of reach after the daemon's one relaunch
	// of it, a relaunched controller that has not reached ready with its Envoy role 20 minutes
	// after that relaunch included.
	ControllerUnreachable = Case{
		Word:     "controller_unreachable",
		Sentence: "the project's controller is out of reach of Envoy and the daemon's relaunch of it did not bring it back; nobody else holds that role, so a person must start it",
	}
	// ControllerRelaunchRefused is the controller out of reach with the daemon's relaunch of it
	// refused by the guard, or ended by a process that outlived its stop.
	ControllerRelaunchRefused = Case{
		Word:     "controller_relaunch_refused",
		Sentence: "the project's controller is out of reach of Envoy and the daemon cannot relaunch it; a person must look at its process",
	}
	// NoLiveSession is Envoy's whole session list, Legion's sessions and every other, holding no
	// live session for ListSilentFor.
	NoLiveSession = Case{
		Word:     "envoy_no_live_session",
		Sentence: "Envoy has listed no live session for " + timewords.Words(ListSilentFor) + ", so no agent of the fleet can hear anything",
	}
	// SessionListUnreadable is the daemon's reads of Envoy's session list failing for
	// ListSilentFor.
	SessionListUnreadable = Case{
		Word:     "envoy_session_list_unreadable",
		Sentence: "the daemon has not read Envoy's session list for " + timewords.Words(ListSilentFor) + ", so it cannot tell which roles are out of reach",
	}
	// NewerProcessRefused is a claim's newer process still refused by the worker stream 5 minutes
	// after its first hello, because an older connection will not close (R3).
	NewerProcessRefused = Case{
		Word:     "newer_process_refused",
		Sentence: "a claim's current process is still locked out of its worker stream by an older process that will not stop; a person must end that process on its node",
	}
)

// Writer writes one line per open case of one project.
type Writer struct {
	log     *slog.Logger
	project string

	mu   sync.Mutex
	open map[Line]struct{}
	// lines keeps open outlive the daemon (Load), nil while nothing has been loaded; undeleted is
	// each line closed here whose delete the store refused, deleted again at its next close.
	lines     Lines
	undeleted map[Line]struct{}
	// pending is each record of the store not written yet, in the order its lines opened and
	// closed. One goroutine at a time writes them, outside mu (drain), so a store that does not
	// answer holds back records alone, never a line; draining is whether it runs, and settled is
	// signalled when it ends.
	pending  []lineRecord
	draining bool
	settled  *sync.Cond
}

// lineRecord is one write of the store: line kept open, or forgotten.
type lineRecord struct {
	line Line
	keep bool
}

// Line keys a case that has been written and not ended, as its store keeps it: one case may be
// open for several claims at once, each with a line of its own. Claim is "" for a case of the
// whole project.
type Line struct {
	Word  string
	Claim string
}

// Lines is where a Writer keeps which lines are open, so that a daemon's boot, which ends no
// case, writes no line of a case the daemon before it opened (R6 writes a line once until its
// case ends, and restarts are what the alert must survive).
type Lines interface {
	// OpenLines is every line the store holds open.
	OpenLines(ctx context.Context) ([]Line, error)
	// PutLine keeps l open, and DeleteLine forgets it.
	PutLine(ctx context.Context, l Line) error
	DeleteLine(ctx context.Context, l Line) error
}

// lineBudget bounds one write of the store a line's open or close makes. Neither takes a
// context of its caller's: the worker stream opens its line from a timer of its own.
const lineBudget = 30 * time.Second

// New is a Writer that logs through log for project, and keeps its open lines in its own memory
// until Load gives it a store.
func New(log *slog.Logger, project string) *Writer {
	w := &Writer{log: log, project: project, open: map[Line]struct{}{}, undeleted: map[Line]struct{}{}}
	w.settled = sync.NewCond(&w.mu)
	return w
}

// Load reads the lines lines holds open, which the daemon before this one wrote and no case's
// end has closed, and keeps each line this writer opens or closes there from now on. The daemon
// loads its writer at its boot, before anything can open a line.
func (w *Writer) Load(ctx context.Context, lines Lines) error {
	held, err := lines.OpenLines(ctx)
	if err != nil {
		return fmt.Errorf("read the fleet alert lines the last daemon left open: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = lines
	for _, l := range held {
		w.open[l] = struct{}{}
	}
	return nil
}

// Open writes c's line, naming claim where there is one and whatever else the caller gives as
// slog key-value pairs, and returns whether it wrote: a case already open, in this daemon or in
// the one before it, writes nothing until Close ends it. The line is written before its record
// is kept, and the record is kept outside the writer's lock, so a store that refuses the record
// costs at most a second line from the next daemon, and one that does not answer delays no line.
func (w *Writer) Open(c Case, claim string, attrs ...any) bool {
	key := Line{Word: c.Word, Claim: claim}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, already := w.open[key]; already {
		return false
	}
	w.open[key] = struct{}{}
	delete(w.undeleted, key)

	args := []any{"case", c.Word, "project", w.project}
	if claim != "" {
		args = append(args, "claim", claim)
	}
	w.log.Error(Prefix+": "+c.Sentence, append(args, attrs...)...)
	w.record(lineRecord{line: key, keep: true})
	return true
}

// Close ends c for claim, so the next Open of it writes again. Closing a case that is not open
// changes nothing, but for a close whose delete the store refused, which is made again.
func (w *Writer) Close(c Case, claim string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := Line{Word: c.Word, Claim: claim}
	_, open := w.open[key]
	_, undeleted := w.undeleted[key]
	if !open && !undeleted {
		return
	}
	delete(w.open, key)
	w.record(lineRecord{line: key})
}

// CloseEnded ends each line of c whose claim holds says the case no longer holds for: a case
// whose end only the caller can read, such as a lock-out the last daemon's worker stream held,
// which no connection of this daemon's holds. holds is read outside the writer's lock.
func (w *Writer) CloseEnded(c Case, holds func(claim string) bool) {
	w.mu.Lock()
	var open []string
	for l := range w.open {
		if l.Word == c.Word {
			open = append(open, l.Claim)
		}
	}
	w.mu.Unlock()
	for _, claim := range open {
		if !holds(claim) {
			w.Close(c, claim)
		}
	}
}

// Wait waits until every record an open or a close asked of the store has been written, for the
// daemon's stop and for a test.
func (w *Writer) Wait() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.draining {
		w.settled.Wait()
	}
}

// record asks the store for rec, where a store is loaded, and starts the goroutine that writes
// the records if none runs. The caller holds mu.
func (w *Writer) record(rec lineRecord) {
	if w.lines == nil {
		return
	}
	w.pending = append(w.pending, rec)
	if !w.draining {
		w.draining = true
		go w.drain()
	}
}

// drain writes the pending records in their order, each under lineBudget and outside mu, until
// none is left. A delete the store refuses marks its line undeleted, unless the line has opened
// again since, so the line's next close deletes it again.
func (w *Writer) drain() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.pending) > 0 {
		rec := w.pending[0]
		w.pending = w.pending[1:]
		w.mu.Unlock()
		err := w.write(rec)
		w.mu.Lock()
		switch {
		case err != nil && rec.keep:
			w.log.Warn("fleet alert: the line is written, but the store did not keep it open; a daemon that boots before its case ends writes it again",
				"case", rec.line.Word, "claim", rec.line.Claim, "error", err)
		case err != nil:
			if _, reopened := w.open[rec.line]; !reopened {
				w.undeleted[rec.line] = struct{}{}
			}
			w.log.Warn("fleet alert: the store did not forget a line whose case ended; it is forgotten again at the next close",
				"case", rec.line.Word, "claim", rec.line.Claim, "error", err)
		case !rec.keep:
			delete(w.undeleted, rec.line)
		}
	}
	w.draining = false
	w.settled.Broadcast()
}

// write is rec written to the store, under lineBudget.
func (w *Writer) write(rec lineRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), lineBudget)
	defer cancel()
	if rec.keep {
		return w.lines.PutLine(ctx, rec.line)
	}
	return w.lines.DeleteLine(ctx, rec.line)
}
