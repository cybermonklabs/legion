package independent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// read.go is everything this reading takes out of the database, and the shape it keeps it in.
// Four tables hold what R6's words are written over: `issues` is each issue with its parent and
// its close, `events` is what Dispatch recorded of an issue, `handoffs` is each phase handed on,
// and `legion_observations` is what the daemon's sampler saw. The reading reads them itself, with
// statements of its own, so a fault in another reader's statements cannot reach it.

// mark is one value as of one moment: a status an event carried, or a field the sampler recorded.
type mark struct {
	at    time.Time
	value string
}

// handoff is one phase handed on: the word the handoff names and when it was recorded.
type handoff struct {
	phase string
	at    time.Time
}

// issue is one issue of the project with everything the rules read of it.
type issue struct {
	key     string
	parent  string
	status  string
	created time.Time
	closed  *time.Time

	// firstRoleEvent is the first event each Legion role wrote on this issue, by role: an event
	// of a session titled `Legion <role> · <this issue's key>`. It and handoffs are the issue's
	// Legion phase work.
	firstRoleEvent map[string]time.Time
	handoffs       []handoff

	// statusMarks is every status the issue's own `issue.*` events carried, oldest first: its
	// status writes.
	statusMarks []mark
	// observed is what the sampler recorded of each field, oldest first within a field.
	observed map[string][]mark
	// askTurns is the stretches an ask opened on this issue by a Legion session of its tree
	// stood on a person's turn.
	askTurns []span

	// tree is the tree root the daemon named for the issue, empty where it named none, and
	// reported is whether the daemon reports the issue at all.
	tree     string
	reported bool
}

// worked is whether the issue has Legion phase work: a Legion role wrote on it, or a phase of it
// was handed on.
func (i *issue) worked() bool {
	return len(i.firstRoleEvent) > 0 || len(i.handoffs) > 0
}

// filing is one issue a Legion session created: the issue, when, the parent its `issue.created`
// event gave it, and the title of the session that created it.
type filing struct {
	key, parent string
	at          time.Time
	filer       sessionTitle
}

// population is every issue of the project with the facts the rules read of it, and the filings
// its Legion sessions made.
type population struct {
	project string
	window  Window
	issues  map[string]*issue
	// keys is every issue of the project in key order, so a pass over the population is the same
	// pass every time.
	keys []string
	// asks is every ask turn the read found, before the tree test picked the ones that count for
	// the issue the ask was opened on (keepTreeAsks).
	asks    []askTurn
	filings []filing
	// firstClaim and firstSlot are the moments of the project's first `claim.<role>` and first
	// `slot` observation, nil where it has none.
	firstClaim, firstSlot *time.Time
}

// readPopulation reads the project's issues and everything the rules measure them with.
func readPopulation(ctx context.Context, db Querier, project string, window Window) (*population, error) {
	held := &population{project: project, window: window, issues: map[string]*issue{}}
	for _, read := range []func(context.Context, Querier) error{
		held.readIssues, held.readEvents, held.readHandoffs, held.readObservations, held.readFilings,
	} {
		if err := read(ctx, db); err != nil {
			return nil, err
		}
	}
	return held, nil
}

// readIssues reads every issue of the project: its parent link, its status, when it was created
// and when it closed. The whole project and not a window of it: a tree and a root's children are
// the project's, whatever window a measure counts.
func (p *population) readIssues(ctx context.Context, db Querier) error {
	rows, err := db.Query(ctx, `
		select key, coalesce(parent_key, ''), status, created_at, closed_at
		from issues
		where project_key = $1
		order by key
	`, p.project)
	if err != nil {
		return fmt.Errorf("read the issues of %s: %w", p.project, err)
	}
	defer rows.Close()
	for rows.Next() {
		one := &issue{firstRoleEvent: map[string]time.Time{}, observed: map[string][]mark{}}
		if err := rows.Scan(&one.key, &one.parent, &one.status, &one.created, &one.closed); err != nil {
			return fmt.Errorf("scan an issue of %s: %w", p.project, err)
		}
		p.issues[one.key] = one
		p.keys = append(p.keys, one.key)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues of %s: %w", p.project, err)
	}
	slices.Sort(p.keys)
	return nil
}

// eventRow is one row of the event read: what it was, who wrote it, and the payload fields the
// rules read.
type eventRow struct {
	key, kind, actorKind, session, status, ask string
	at                                         time.Time
}

// readEvents reads every event of the project's issues and takes from it the three things the
// rules read of the event log: the first event of each Legion role on each issue, the statuses the
// issues passed through, and the asks that stood on a person's turn. One pass, by the moment of
// the event and by the row's own id where two events share a moment.
func (p *population) readEvents(ctx context.Context, db Querier) error {
	rows, err := db.Query(ctx, `
		select e.issue_key, e.created_at, e.type, coalesce(e.actor->>'kind', ''),
			coalesce(e.actor->>'id', ''), coalesce(e.payload->>'status', ''),
			coalesce(e.payload->>'id', '')
		from events e
		join issues i on i.key = e.issue_key
		where i.project_key = $1
		order by e.created_at, e.id
	`, p.project)
	if err != nil {
		return fmt.Errorf("read the events of %s: %w", p.project, err)
	}
	defer rows.Close()
	var read []eventRow
	sessions := map[string]bool{}
	for rows.Next() {
		var one eventRow
		if err := rows.Scan(&one.key, &one.at, &one.kind, &one.actorKind, &one.session,
			&one.status, &one.ask); err != nil {
			return fmt.Errorf("scan an event of %s: %w", p.project, err)
		}
		read = append(read, one)
		if one.actorKind == "session" && one.session != "" {
			sessions[one.session] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the events of %s: %w", p.project, err)
	}
	titles, err := readTitles(ctx, db, sessions)
	if err != nil {
		return err
	}
	p.takeRoleEvents(read, titles)
	p.takeStatusMarks(read)
	p.takeAskTurns(read, titles)
	return nil
}

// readTitles is the title each of the sessions goes by: "a session, by its session id, has a
// title `Legion <role> · <KEY>` anywhere in Dispatch's events", so the newest title that session
// stamped on anything it wrote, whatever issue it wrote it on. A session that stamped none has
// none.
func readTitles(ctx context.Context, db Querier, sessions map[string]bool) (map[string]string, error) {
	named := slices.Sorted(maps.Keys(sessions))
	titles := make(map[string]string, len(named))
	if len(named) == 0 {
		return titles, nil
	}
	rows, err := db.Query(ctx, `
		select distinct on (actor->>'id') actor->>'id', actor->'origin'->>'session_title'
		from events
		where actor->>'kind' = 'session' and actor->>'id' = any($1)
			and nullif(actor->'origin'->>'session_title', '') is not null
		order by actor->>'id', created_at desc, id desc
	`, named)
	if err != nil {
		return nil, fmt.Errorf("read the session titles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var session, title string
		if err := rows.Scan(&session, &title); err != nil {
			return nil, fmt.Errorf("scan a session title: %w", err)
		}
		titles[session] = title
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the session titles: %w", err)
	}
	return titles, nil
}

// takeRoleEvents keeps, for each issue, the first event each Legion role wrote on that same
// issue: "an event on an issue by a Legion session of another issue is not a worker event of that
// issue", so a root's architect filing a child wrote on the child without working it.
func (p *population) takeRoleEvents(read []eventRow, titles map[string]string) {
	for _, one := range read {
		if one.actorKind != "session" {
			continue
		}
		title, isLegion := parseSessionTitle(titles[one.session])
		if !isLegion || title.controller || title.issue != one.key {
			continue
		}
		held, seen := p.issues[one.key].firstRoleEvent[title.role]
		if !seen || one.at.Before(held) {
			p.issues[one.key].firstRoleEvent[title.role] = one.at
		}
	}
}

// takeStatusMarks keeps every status write of the issues, oldest first: the status an `issue.*`
// event carried. They say when an issue sat parked and, for an issue the sampler never saw, which
// phase it was in.
func (p *population) takeStatusMarks(read []eventRow) {
	for _, one := range read {
		if !strings.HasPrefix(one.kind, "issue.") || one.status == "" {
			continue
		}
		held := p.issues[one.key]
		held.statusMarks = append(held.statusMarks, mark{at: one.at, value: one.status})
	}
}

// askOpenings and askClosings are the events that put an ask on a person's turn and the events
// that take it back: "`ask.opened` or `ask.handed_back`, until `ask.answered`, `ask.resolved`".
var (
	askOpenings = []string{"ask.opened", "ask.handed_back"}
	askClosings = []string{"ask.answered", "ask.resolved"}
)

// askTurn is one stretch an ask stood on a person's turn: the issue it was opened on, the issue
// the opening session's title named, and the stretch itself.
type askTurn struct {
	on, by  string
	stretch span
}

// takeAskTurns turns the ask events into the stretches each ask stood on a person's turn: from
// the moment a Legion session opened it or handed it back, to the answer or the resolution that
// took it back, and to the issue's own end where neither came. Whether a turn counts for the
// issue it was opened on needs the trees, so the tree test runs later (keepTreeAsks).
func (p *population) takeAskTurns(read []eventRow, titles map[string]string) {
	open := map[string]askTurn{}
	for _, one := range read {
		if one.ask == "" {
			continue
		}
		switch {
		case slices.Contains(askOpenings, one.kind):
			if _, already := open[one.ask]; already || one.actorKind != "session" {
				continue
			}
			title, isLegion := parseSessionTitle(titles[one.session])
			if !isLegion {
				continue
			}
			open[one.ask] = askTurn{on: one.key, by: title.issue, stretch: span{from: one.at}}
		case slices.Contains(askClosings, one.kind):
			turn, waiting := open[one.ask]
			if !waiting {
				continue
			}
			delete(open, one.ask)
			turn.stretch.to = one.at
			p.asks = append(p.asks, turn)
		}
	}
	for _, turn := range open {
		turn.stretch.to = p.end(turn.on)
		p.asks = append(p.asks, turn)
	}
}

// readHandoffs reads every phase handed on, oldest first.
func (p *population) readHandoffs(ctx context.Context, db Querier) error {
	rows, err := db.Query(ctx, `
		select h.issue_key, h.phase, h.created_at
		from handoffs h
		join issues i on i.key = h.issue_key
		where i.project_key = $1
		order by h.created_at, h.id
	`, p.project)
	if err != nil {
		return fmt.Errorf("read the handoffs of %s: %w", p.project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var one handoff
		if err := rows.Scan(&key, &one.phase, &one.at); err != nil {
			return fmt.Errorf("scan a handoff of %s: %w", p.project, err)
		}
		if held := p.issues[key]; held != nil {
			held.handoffs = append(held.handoffs, one)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the handoffs of %s: %w", p.project, err)
	}
	return nil
}

// readObservations reads what the daemon's sampler recorded of the project's issues, oldest first
// within each field: the phase of each issue, the slot it held, each role's claim and the wait the
// daemon reported, with which issues the daemon reports at all and the tree root it last named
// for each. It reads too the moments of the project's first claim and first slot observation,
// over every row of the project, which is where idle time and slot wait begin to have data.
func (p *population) readObservations(ctx context.Context, db Querier) error {
	if err := db.QueryRow(ctx, `
		select min(observed_at) filter (where field like 'claim.%'),
		       min(observed_at) filter (where field = 'slot')
		from legion_observations
		where project = $1
	`, p.project).Scan(&p.firstClaim, &p.firstSlot); err != nil {
		return fmt.Errorf("read the first observations of %s: %w", p.project, err)
	}
	rows, err := db.Query(ctx, `
		select issue_key, field, value::text, observed_at
		from legion_observations
		where project = $1 and issue_key <> ''
		order by observed_at, id
	`, p.project)
	if err != nil {
		return fmt.Errorf("read the observations of %s: %w", p.project, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, field, value string
		var at time.Time
		if err := rows.Scan(&key, &field, &value, &at); err != nil {
			return fmt.Errorf("scan an observation of %s: %w", p.project, err)
		}
		held := p.issues[key]
		if held == nil {
			continue
		}
		held.reported = true
		held.observed[field] = append(held.observed[field], mark{at: at, value: value})
		if field == fieldTree {
			held.tree = jsonString(value)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the observations of %s: %w", p.project, err)
	}
	return nil
}

// readFilings reads every issue a Legion session of the project created, in whichever project
// the issue lives: the `issue.created` event of each issue whose actor is a session titled
// `Legion <role> · <an issue of this project>` or `Legion controller · <this project>`, with the
// parent the event gave the new issue.
func (p *population) readFilings(ctx context.Context, db Querier) error {
	rows, err := db.Query(ctx, `
		select issue_key, created_at, actor->>'id', coalesce(payload->>'parent', '')
		from events
		where type = 'issue.created' and actor->>'kind' = 'session'
		order by created_at, id
	`)
	if err != nil {
		return fmt.Errorf("read the issues Legion sessions created: %w", err)
	}
	defer rows.Close()
	type created struct {
		key, session, parent string
		at                   time.Time
	}
	var read []created
	sessions := map[string]bool{}
	for rows.Next() {
		var one created
		if err := rows.Scan(&one.key, &one.at, &one.session, &one.parent); err != nil {
			return fmt.Errorf("scan an issue a Legion session created: %w", err)
		}
		read = append(read, one)
		sessions[one.session] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the issues Legion sessions created: %w", err)
	}
	titles, err := readTitles(ctx, db, sessions)
	if err != nil {
		return err
	}
	filed := map[string]bool{}
	for _, one := range read {
		title, isLegion := parseSessionTitle(titles[one.session])
		if !isLegion || filed[one.key] {
			continue
		}
		ours := title.controller && title.subject == p.project
		if !title.controller {
			_, ours = p.issues[title.issue]
		}
		if !ours {
			continue
		}
		filed[one.key] = true
		p.filings = append(p.filings, filing{key: one.key, parent: one.parent, at: one.at, filer: title})
	}
	return nil
}

// end is where an issue's time stops: its close, or the end of the window for one still open
// ("An issue that has not closed ... runs to the end of the window").
func (p *population) end(key string) time.Time {
	if held := p.issues[key]; held != nil && held.closed != nil {
		return *held.closed
	}
	return p.window.To
}

// jsonString is the string a recorded value holds, and "" for a null, a number or a value of any
// other shape: the sampler writes each field as JSON.
func jsonString(value string) string {
	var held string
	if err := json.Unmarshal([]byte(value), &held); err != nil {
		return ""
	}
	return held
}

// sessionTitle is what a session's title says it is: the role it works and the issue it works, or
// that it is the controller of the project it names.
type sessionTitle struct {
	role       string
	issue      string
	subject    string
	controller bool
}

// legionRoles are the six worker types a Legion session can name, which is what makes a title a
// role's title.
var legionRoles = []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}

// parseSessionTitle reads a session's title as Legion writes it: `Legion <role> · <KEY>` for a
// role working an issue, and `Legion controller · <PROJECT>` for a project's controller. A title
// of another shape, or one naming a role Legion does not run, is no Legion session.
func parseSessionTitle(title string) (sessionTitle, bool) {
	rest, isLegion := strings.CutPrefix(strings.TrimSpace(title), "Legion ")
	if !isLegion {
		return sessionTitle{}, false
	}
	role, subject, split := strings.Cut(rest, " · ")
	if !split {
		return sessionTitle{}, false
	}
	role, subject = strings.TrimSpace(role), strings.TrimSpace(subject)
	if subject == "" {
		return sessionTitle{}, false
	}
	if role == "controller" {
		return sessionTitle{role: role, subject: subject, controller: true}, true
	}
	if !slices.Contains(legionRoles, role) {
		return sessionTitle{}, false
	}
	return sessionTitle{role: role, issue: subject, subject: subject}, true
}
