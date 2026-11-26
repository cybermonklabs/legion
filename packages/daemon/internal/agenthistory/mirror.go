package agenthistory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
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
// StartedAt is RFC 3339 UTC. The mirror leaves RecordedAt out, which the listener sets.
type Entry = listenerwire.LegionAgentEntry

// entryOf is the registry entry of row, or why it has none: a session id the registry cannot key,
// or a row whose token is not the one its project, issue and role build.
func entryOf(row store.AgentConversation) (Entry, string) {
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
	}, ""
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
// hands it, in the order handed. Nothing it does blocks the boot or a registration. Its writes go
// through a registrywrite.Writer: a route an older listener answers 404 is logged once and tried
// again each minute; any other failure is tried again on a backoff. A write the listener refuses
// (400 or 409: an entry it will never take) is logged and dropped; a refused batch is sent again an
// entry at a time, so one bad entry costs only itself.
type Mirror struct {
	cfg  MirrorConfig
	wake chan struct{}

	mu sync.Mutex
	// puts are the registrations Put handed and Run has not written, one per session, in order;
	// a later registration of a session replaces its entry in place.
	puts []Entry

	// The rest is Run's own: the writer, whose log of each route's first 404 is Run's alone too.
	writer    *registrywrite.Writer
	booted    bool
	batches   [][]Entry
	writes    lane
	heartbeat lane
}

// lane is one kind of write's schedule: when it is next tried, and how many failures in a row it
// has had.
type lane struct {
	next     time.Time
	failures int
}

// NewMirror builds a mirror that writes nothing until Run.
func NewMirror(cfg MirrorConfig) *Mirror {
	return &Mirror{cfg: cfg, wake: make(chan struct{}, 1), writer: registrywrite.New(cfg.BaseURL, cfg.Token, cfg.Client, cfg.Log)}
}

// Put queues the registration row for one PUT of its session, never waiting on the listener.
func (m *Mirror) Put(row store.AgentConversation) {
	entry, why := entryOf(row)
	if why != "" {
		m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: "+why, "session", row.Session, "claim", row.Token)
		return
	}
	m.mu.Lock()
	m.hold(entry)
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

// wakeRun ends Run's wait, or has its next wait end at once, without waiting for it.
func (m *Mirror) wakeRun() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run writes until ctx ends: the heartbeat at once and then each minute, the recorded history in
// batches, and every Put as it comes, each kind on its own schedule (lane), so a route an older
// listener lacks never holds up one it has. Without a Store it writes the heartbeat alone.
func (m *Mirror) Run(ctx context.Context) {
	for {
		now := m.cfg.Clock.Now()
		if !now.Before(m.heartbeat.next) {
			m.beat(ctx, now)
		}
		if !now.Before(m.writes.next) && m.pendingWrites() {
			m.write(ctx, now)
		}
		if ctx.Err() != nil {
			return
		}
		next := m.heartbeat.next
		if m.pendingWrites() && m.writes.next.Before(next) {
			next = m.writes.next
		}
		// A registration wakes the loop at once; a lane on a failure's schedule is still not tried
		// before its time.
		select {
		case <-ctx.Done():
			return
		case <-m.cfg.Clock.After(max(next.Sub(m.cfg.Clock.Now()), 0)):
		case <-m.wake:
		}
	}
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
		batches := m.batchesOf(rows)
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

// entriesOf is the registry entries of rows. A scanned conversation that would read as its role's
// newest over a registered one (its header time not before that role's newest registration) is
// left out: a conversation found by its title never outranks one a registration recorded. A row
// the registry would not take is logged and left out.
func (m *Mirror) entriesOf(rows []store.AgentConversation) []Entry {
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
		entry, why := entryOf(row)
		if why != "" {
			m.cfg.Log.Warn("legion daemon does not write the conversation to the agent registry: "+why, "session", row.Session, "claim", row.Token)
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// batchesOf is the history's entries in batches within the route's bounds, oldest first.
func (m *Mirror) batchesOf(rows []store.AgentConversation) [][]Entry {
	var batches [][]Entry
	var batch []Entry
	size := 0
	const envelope = len(`{"entries":[]}`)
	for _, entry := range m.entriesOf(rows) {
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
