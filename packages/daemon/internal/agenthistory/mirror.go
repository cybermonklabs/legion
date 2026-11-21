package agenthistory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/registrywrite"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

const (
	// batchEntries and batchBytes are the batch route's bounds: at most 256 entries and a 1 MiB
	// body per request.
	batchEntries = 256
	batchBytes   = 1 << 20
	// heartbeatEvery is how often the project's answering key is written. The listener hands a
	// project's messages off while that key last said answering, whether this daemon runs now or
	// not, so the minute's write is how an operator sees the daemon is up, not what keeps the
	// hand-off alive (spec v21, section 2.12 of dispatch://INFRA-204's plan).
	heartbeatEvery = time.Minute
	// standingsEvery is how often the standing of the history's issues and the project's released
	// claims are read again (Store.FinishedIssues, Store.ReleasedSessions), so an issue that
	// closed, was parked or whose tree was retired - and one a person moved back into the workflow
	// - and a claim resumed reach the registry within the minute. A claim the daemon's idle
	// release suspends reaches it at once: the release says so (Mirror.StandingsChanged).
	standingsEvery = time.Minute
	// notFoundRetry is how soon a route an older listener answers 404 is tried again.
	notFoundRetry = time.Minute
	// failureBackoff is the first wait after any other failure (backoff.Wait), and failureMax the
	// longest.
	failureBackoff = 2 * time.Second
	failureMax     = time.Minute
	// registryTime is RFC 3339 in UTC at a fixed millisecond width, so two times order as their
	// strings do.
	registryTime = "2006-01-02T15:04:05.000Z07:00"
)

// keySafe is a session id the registry takes (legionagents.sessionIDPattern on the listener):
// one NATS key token, so no `.`, and nothing a URL path would need to escape.
var keySafe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Entry is one registry entry as the listener's write routes take it (dispatch://INFRA-448, "The
// registry contract"), the listener's wire type (listenerwire.LegionAgentEntry): LegionProject is
// the Legion project token role tokens carry (claim.ProjectToken), never the Dispatch project;
// StartedAt is RFC 3339 UTC. The mirror leaves RecordedAt out, which the listener sets. Finished is
// whether the conversation's issue has left the workflow (Store.FinishedIssues), and Released
// whether its claim is suspended by idle release (supervise.Claim.Released,
// Store.ReleasedSessions); each is omitted while false, which is what a reader takes from an entry
// that carries none.
type Entry = listenerwire.LegionAgentEntry

// entryOf is the registry entry of row, with finished as its issue's standing and released as
// its claim's, or why it has none: a session id the registry cannot key, or a row whose token is
// not the one its project, issue and role build.
func entryOf(row store.AgentConversation, finished, released bool) (Entry, string) {
	if !keySafe.MatchString(row.Session) {
		return Entry{}, "its session id is not one the registry can key"
	}
	if token, err := claim.NewToken(row.Project, row.Issue, row.Role); err != nil || token != row.Token {
		return Entry{}, fmt.Sprintf("its token %s is not the one its project, issue and role build", row.Token)
	}
	if row.Harness != runtime.BackendOmp && row.Harness != runtime.BackendClaude {
		return Entry{}, fmt.Sprintf("its harness %q is neither omp nor claude", row.Harness)
	}
	return Entry{
		SessionID: row.Session, LegionProject: row.Project, Issue: row.Issue, Role: string(row.Role), RoleToken: string(row.Token),
		Title: Title(row.Role, row.Issue), Harness: string(row.Harness), StartedAt: row.StartedAt.UTC().Format(registryTime),
		Finished: finished, Released: released,
	}, ""
}

// standingRead is what one refresh read: whether each issue the history names has left the
// workflow (finished, Store.FinishedIssues), and the session of every released claim with the
// issue it works (released, Store.ReleasedSessions). Every entry written since carries both.
type standingRead struct {
	finished map[string]bool
	released map[string]string
}

// of is row's standing: its issue's finished, and whether its session is a released claim's.
func (s standingRead) of(row store.AgentConversation) (finished, released bool) {
	_, released = s.released[row.Session]
	return s.finished[row.Issue], released
}

// Clock is time as the mirror sees it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// MirrorConfig is what a Mirror is built from.
type MirrorConfig struct {
	// BaseURL and Token are the Envoy listener's address and its `/v1` bearer, the daemon's own
	// (envoy_url, envoy_token_file, as notify.HTTPPublisher holds them); Token "" sends none.
	BaseURL, Token string
	// Project is the Legion project token: the heartbeat's key and every entry's legion_project.
	Project string
	// Answering is what the heartbeat says: whether the daemon's answer loop runs, so that the
	// listener hands the project's messages off to it.
	Answering bool
	// Store is the history the entries are read from; nil writes no entry, only the heartbeat.
	Store Store
	Clock Clock
	Log   *slog.Logger
	// Client is the HTTP client the registry writes go through (registrywrite.New); nil is one
	// with a ten-second timeout.
	Client *http.Client
}

// Mirror writes the project's `answering` key to the listener's Legion agent registry every
// minute, and, with a Store, the history: once Run starts, every recorded conversation of the
// project in batches (each at most 256 entries and 1 MiB), then one PUT for each registration Put
// hands it, in the order handed. It reads the standing of the history's issues and the project's
// released claims each minute too (refresh), and at once when the daemon says a release suspended
// a claim (StandingsChanged), and writes an issue's conversations again when either changes, so
// each entry's `finished` and `released` follow the record in both directions. Nothing it does
// blocks the boot, a registration or a release. Its writes go through a registrywrite.Writer: a
// route an older listener answers 404 is logged once and tried again each minute; any other
// failure is tried again on a backoff. A write the listener refuses (400 or 409: an entry it will
// never take) is logged and dropped; a refused batch is sent again an entry at a time, so one bad
// entry costs only itself.
type Mirror struct {
	cfg  MirrorConfig
	wake chan struct{}

	mu sync.Mutex
	// puts are the registrations Put handed and Run has not written, one per session, in order;
	// a later registration of a session replaces its entry in place.
	puts []Entry
	// read is what the last standings read said of each issue the history names and of the
	// project's released claims, which every entry written since carries. An issue it does not
	// name is not finished and a session it does not name is not released, so a mirror that has
	// read no standing yet writes the keep every entry had before these fields.
	read standingRead
	// nudged is whether StandingsChanged asked for a standings read that no read has answered yet
	// (refresh clears it just before it reads the released claims).
	nudged bool
	// registrations counts the registrations Put has queued, and registeredAt is that count at
	// each session's latest one. A standings read notes the count just before it reads the
	// released claims, and writes a session registered after that without `released` (queueRead).
	registrations uint64
	registeredAt  map[string]uint64

	// The rest is Run's own: the writer, whose log of each route's first 404 is Run's alone too.
	writer    *registrywrite.Writer
	booted    bool
	batches   [][]Entry
	writes    lane
	heartbeat lane
	standings lane
}

// lane is one kind of write's schedule: when it is next tried, and how many failures in a row it
// has had.
type lane struct {
	next     time.Time
	failures int
}

// NewMirror builds a mirror that writes nothing until Run.
func NewMirror(cfg MirrorConfig) *Mirror {
	return &Mirror{cfg: cfg, wake: make(chan struct{}, 1), read: standingRead{finished: map[string]bool{}, released: map[string]string{}},
		registeredAt: map[string]uint64{}, writer: registrywrite.New(cfg.BaseURL, cfg.Token, cfg.Client, cfg.Log)}
}

// Put queues the registration row for one PUT of its session, never waiting on the listener. A
// registration is made while its issue runs, so the entry carries the standing the last refresh
// read of that issue, which is unfinished for every issue no refresh has named. It is never
// released: a registration is its claim's process running, whatever the last refresh read of
// the claim before it resumed, and whatever a refresh under way read of it (queueRead).
func (m *Mirror) Put(row store.AgentConversation) {
	finished, _ := m.standing().of(row)
	entry, why := entryOf(row, finished, false)
	if why != "" {
		m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: "+why, "session", row.Session, "claim", row.Token)
		return
	}
	m.mu.Lock()
	m.registrations++
	m.registeredAt[entry.SessionID] = m.registrations
	m.hold(entry)
	m.mu.Unlock()
	m.wakeRun()
}

// queueRead holds the entries a standings read made for one PUT each, and wakes Run. since is the
// registration count the read noted before it read the released claims: a session registered
// after that is its claim running again, which the read could not know, so its entry is written
// without `released`. The check and the hold are one step under m.mu, as Put's count and hold
// are, so whichever of a registration and the read is queued last, the session's entry ends
// without the flag.
func (m *Mirror) queueRead(entries []Entry, since uint64) {
	m.mu.Lock()
	for _, entry := range entries {
		if entry.Released && m.registeredAt[entry.SessionID] > since {
			entry.Released = false
		}
		m.hold(entry)
	}
	m.mu.Unlock()
	m.wakeRun()
}

// hold holds entry for one PUT of its session, replacing a queued write of the same session in
// place. The caller holds m.mu.
func (m *Mirror) hold(entry Entry) {
	for i := range m.puts {
		if m.puts[i].SessionID == entry.SessionID {
			m.puts[i] = entry
			return
		}
	}
	m.puts = append(m.puts, entry)
}

// StandingsChanged tells the mirror that the standing it reads has changed: the daemon's idle
// release has suspended a claim and recorded it (store.ReleasedSessions names its session now).
// Run reads the standings at once rather than at its next minute, with the same refresh, so the
// released claims stay the one source of each entry's `released` and the writes go out on the
// Put lane; until they do, the listener hands a message for that session to the answer loop
// rather than leaving it to the wake. It never waits on Run or the listener. A nudge while a read
// is due, or while one runs and has not yet read the released claims, costs no read of its own;
// a lane on a failure's backoff is still not read before its time, and its retry reads the claim
// all the same. A mirror without a Store reads no standing, so a nudge does nothing there.
func (m *Mirror) StandingsChanged() {
	if m.cfg.Store == nil {
		return
	}
	m.mu.Lock()
	m.nudged = true
	m.mu.Unlock()
	m.wakeRun()
}

// wakeRun ends Run's wait, or has its next wait end at once, without waiting for it.
func (m *Mirror) wakeRun() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// standing is the last standings read, which no caller changes: refresh puts new maps in its
// place rather than writing into these.
func (m *Mirror) standing() standingRead {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.read
}

// Run writes until ctx ends: the heartbeat at once and then each minute, the standing of the
// history's issues at once, then each minute and whenever StandingsChanged asks, the recorded
// history in batches, and every Put as it comes, each kind on its own schedule (lane), so a route
// an older listener lacks never holds up one it has. The standing is read before the boot's own
// write, so the first batch already carries each entry's `finished` and `released`. Without a
// Store it writes the heartbeat alone.
func (m *Mirror) Run(ctx context.Context) {
	for {
		now := m.cfg.Clock.Now()
		if !now.Before(m.heartbeat.next) {
			m.beat(ctx, now)
		}
		if m.cfg.Store != nil && (!now.Before(m.standings.next) || m.standingsAsked()) {
			m.refresh(ctx, now)
		}
		if !now.Before(m.writes.next) && m.pendingWrites() {
			m.write(ctx, now)
		}
		if ctx.Err() != nil {
			return
		}
		next := m.heartbeat.next
		if m.cfg.Store != nil && m.standings.next.Before(next) {
			next = m.standings.next
		}
		if m.pendingWrites() && m.writes.next.Before(next) {
			next = m.writes.next
		}
		// A registration and a nudge wake the loop at once; a lane on a failure's schedule is
		// still not tried before its time.
		select {
		case <-ctx.Done():
			return
		case <-m.cfg.Clock.After(max(next.Sub(m.cfg.Clock.Now()), 0)):
		case <-m.wake:
		}
	}
}

// standingsAsked is whether StandingsChanged asked for a standings read no read has answered, and
// the lane may read now: one on a failure's backoff waits for its retry, which reads what the
// nudge asked for.
func (m *Mirror) standingsAsked() bool {
	if m.standings.failures > 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nudged
}

func (m *Mirror) pendingWrites() bool {
	if m.cfg.Store == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.booted || len(m.batches) > 0 || len(m.puts) > 0
}

// beat writes the project's answering key.
func (m *Mirror) beat(ctx context.Context, now time.Time) {
	answering := m.cfg.Answering
	body := listenerwire.LegionProjectRequest{Answering: &answering, UpdatedAt: now.UTC().Format(registryTime)}
	result := m.writer.Answering(ctx, m.cfg.Project, body)
	// The key is the listener's to refuse, and the next minute's is a new write all the same:
	// schedule puts a refusal back on the heartbeat's own interval, as it does a send.
	m.heartbeat.schedule(now, result, heartbeatEvery)
}

// refresh reads the standing of every issue the history names and the project's released claims,
// and queues one PUT of each conversation of each issue whose standing changed or one of whose
// sessions turned released or stopped being so, so a role's entries say `finished` from the
// minute its issue leaves the workflow - a close, a park, a tree retired - and say it no longer
// once a person moves that issue back in, and a session's entry says `released` from the moment
// the daemon's idle release suspends its claim (StandingsChanged) and no longer once a wake
// resumes it. The queued writes go out on the Put lane, which is the one write path and the one
// 409 handling: a session the listener holds bound to another issue is refused and dropped there
// as any other write of it is.
//
// Nothing is queued before the boot's write, which writes every entry anyway with the standing
// this read just recorded. An issue whose conversations could not be read keeps its last recorded
// standing and its sessions their last recorded release, so the next pass finds it changed again
// and writes it then; that pass is the failure's own backoff rather than the minute.
func (m *Mirror) refresh(ctx context.Context, now time.Time) {
	finished, err := m.cfg.Store.FinishedIssues(ctx, m.cfg.Project, record.OutOfWorkflow)
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Log.Warn("legion daemon could not read the standing of its issues for the agent registry; trying again", "error", err)
		}
		m.standings.schedule(now, registrywrite.Failed, standingsEvery)
		return
	}
	// A nudge is answered from here: one that came before this point told of a suspension the
	// store had already committed, which the read below sees, and one that comes after it may
	// tell of one this read misses, so it asks for a read of its own. A registration from here on
	// may be a resume this read misses too, so the count noted here lets it win (queueRead).
	m.mu.Lock()
	m.nudged = false
	since := m.registrations
	m.mu.Unlock()
	released, err := m.cfg.Store.ReleasedSessions(ctx, m.cfg.Project)
	if err != nil {
		if ctx.Err() == nil {
			m.cfg.Log.Warn("legion daemon could not read its released claims for the agent registry; trying again", "error", err)
		}
		m.standings.schedule(now, registrywrite.Failed, standingsEvery)
		return
	}
	m.mu.Lock()
	known, booted := m.read, m.booted
	m.mu.Unlock()
	changed := map[string]bool{}
	for issue, left := range finished {
		if known.finished[issue] != left {
			changed[issue] = true
		}
	}
	for session, issue := range released {
		if _, was := known.released[session]; !was {
			changed[issue] = true
		}
	}
	for session, issue := range known.released {
		if _, is := released[session]; !is {
			changed[issue] = true
		}
	}
	result := registrywrite.Sent
	if booted {
		for _, issue := range slices.Sorted(maps.Keys(changed)) {
			rows, err := m.cfg.Store.IssueConversations(ctx, m.cfg.Project, issue)
			if err != nil {
				if ctx.Err() == nil {
					m.cfg.Log.Warn("legion daemon could not read the conversations of an issue whose standing changed; trying again", "issue", issue, "error", err)
				}
				finished[issue] = known.finished[issue]
				for session, of := range released {
					if of == issue {
						delete(released, session)
					}
				}
				for session, of := range known.released {
					if of == issue {
						released[session] = of
					}
				}
				result = registrywrite.Failed
				continue
			}
			m.queueRead(m.entriesOf(rows, standingRead{finished: finished, released: released}), since)
		}
	}
	m.mu.Lock()
	m.read = standingRead{finished: finished, released: released}
	m.mu.Unlock()
	m.standings.schedule(now, result, standingsEvery)
}

// write reads the history once, then sends the batches and the Puts in order, stopping at the
// first write that must be tried again.
func (m *Mirror) write(ctx context.Context, now time.Time) {
	result := m.writeAll(ctx)
	m.writes.schedule(now, result, 0)
}

func (m *Mirror) writeAll(ctx context.Context) registrywrite.Outcome {
	if !m.booted {
		rows, err := m.cfg.Store.AgentConversations(ctx, m.cfg.Project)
		if err != nil {
			m.cfg.Log.Warn("legion daemon could not read its conversation history for the agent registry; trying again", "error", err)
			return registrywrite.Failed
		}
		batches := m.batchesOf(rows, m.standing())
		m.mu.Lock()
		m.batches, m.booted = batches, true
		m.mu.Unlock()
	}
	for {
		m.mu.Lock()
		if len(m.batches) == 0 {
			m.mu.Unlock()
			break
		}
		batch := m.batches[0]
		m.mu.Unlock()
		result := m.writer.Batch(ctx, listenerwire.LegionAgentBatch{Entries: batch})
		switch result {
		case registrywrite.Sent:
			m.popBatch(nil)
		case registrywrite.Refused:
			// One bad entry refuses its whole batch: each is sent alone, and only a bad one is
			// refused again.
			var singles [][]Entry
			if len(batch) > 1 {
				for _, entry := range batch {
					singles = append(singles, []Entry{entry})
				}
			}
			m.popBatch(singles)
		default:
			return result
		}
	}
	for {
		m.mu.Lock()
		if len(m.puts) == 0 {
			m.mu.Unlock()
			return registrywrite.Sent
		}
		entry := m.puts[0]
		m.mu.Unlock()
		result := m.writer.Entry(ctx, entry)
		if result != registrywrite.Sent && result != registrywrite.Refused {
			return result
		}
		m.mu.Lock()
		// A registration of the same session that came while this one was sent replaced it, and
		// is still to be written.
		if len(m.puts) > 0 && m.puts[0] == entry {
			m.puts = m.puts[1:]
		}
		m.mu.Unlock()
	}
}

// popBatch drops the batch at the head, putting replace in its place.
func (m *Mirror) popBatch(replace [][]Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(replace, m.batches[1:]...)
}

// entriesOf is the registry entries of rows, each carrying its issue's standing and its claim's
// release from read. A scanned conversation that would read as its role's newest over a registered
// one (its header time not before that role's newest registration) is left out: a conversation
// found by its title never outranks one a registration recorded. A row the registry would not take
// is logged and left out.
func (m *Mirror) entriesOf(rows []store.AgentConversation, read standingRead) []Entry {
	registered := map[claim.Token]time.Time{}
	for _, row := range rows {
		if row.Source == store.ConversationRegistered && row.StartedAt.After(registered[row.Token]) {
			registered[row.Token] = row.StartedAt
		}
	}
	var entries []Entry
	for _, row := range rows {
		if newest, ok := registered[row.Token]; ok && row.Source == store.ConversationScanned && !row.StartedAt.Before(newest) {
			m.cfg.Log.Info("legion daemon does not write a scanned conversation to the agent registry: it would read as its role's newest over a registered one",
				"session", row.Session, "claim", row.Token, "path", row.Location)
			continue
		}
		finished, released := read.of(row)
		entry, why := entryOf(row, finished, released)
		if why != "" {
			m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: "+why, "session", row.Session, "claim", row.Token)
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// batchesOf is the history's entries in batches within the route's bounds, oldest first.
func (m *Mirror) batchesOf(rows []store.AgentConversation, read standingRead) [][]Entry {
	var batches [][]Entry
	var batch []Entry
	size := 0
	const envelope = len(`{"entries":[]}`)
	for _, entry := range m.entriesOf(rows, read) {
		encoded, err := json.Marshal(entry)
		if err != nil || envelope+len(encoded) > batchBytes {
			m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: its entry exceeds the batch route's 1 MiB", "session", entry.SessionID)
			continue
		}
		grown := size + len(encoded)
		if len(batch) > 0 {
			grown++ // the comma before it
		}
		if len(batch) == batchEntries || envelope+grown > batchBytes {
			batches = append(batches, batch)
			batch, grown = nil, len(encoded)
		}
		batch, size = append(batch, entry), grown
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

// schedule sets when the lane is next tried after a write that came to result at now: after every
// in a success (0 for whenever there is something to write), each minute after a 404, and on a
// backoff after any other failure.
func (l *lane) schedule(now time.Time, result registrywrite.Outcome, every time.Duration) {
	switch result {
	case registrywrite.Sent, registrywrite.Refused:
		l.failures, l.next = 0, now.Add(every)
		if every == 0 {
			l.next = time.Time{}
		}
	case registrywrite.NotFound:
		l.failures, l.next = 0, now.Add(notFoundRetry)
	default:
		l.failures++
		l.next = now.Add(min(backoff.Wait(failureBackoff, l.failures), failureMax))
	}
}
