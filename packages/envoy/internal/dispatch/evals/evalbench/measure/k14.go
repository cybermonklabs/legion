package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// k14 (dispatch://INFRA-978, T19.9; Verification v14's k14) is `measure k14`, run by run.sh in mode
// k14 against a server booted on the k14 load (evalbench's Spec.K14), its GitHub App pointed at the
// loopback stand-in (github.go), after the consumers caught up:
//
//  1. the daily whole failure write (evals' walkFailures), which the server's first walking pass
//     after its start makes: each project's whole write of its failure rows, read from the
//     statement log as its transaction, each the longest a change owed while it runs waits (the walk
//     stops between two projects for an owed wake), and the refresher's slow-pass lines that hold
//     it (daily_ms, yielded);
//  2. the pages: three open pages of each k14 project's Evals tab, each its own socket on its own
//     evals view (84 days with an 84-day compare range, 28 days, 7 days), subscribed before the
//     first write and held open to the last (k14_live.go);
//  3. the writes, each through the route a person's page uses, each once the server is quiet
//     (awaitQuiet), each followed by its move back, so every write moves the pages' numbers from
//     what the fixture holds and back: a settings PUT moving PAIR's second source from
//     acme/pair-service to acme/pair-spare, which holds no run; a PUT of MIXED's default
//     repository, which its github_workflow source names no repository for and so reads, from
//     acme/mixed-deploy to acme/mixed-spare; and a settings PUT changing the production job names
//     of PAIR's second source from its production apply to its production plan. For each: the
//     write's commit, the end of the commit of the transaction that ran its statement, from the
//     statement log; each page's update, the moment the frame arrived from which on the page showed
//     the watched eval (the project's deploy-frequency) as a fresh read of the page's window answers
//     it once the write has settled; the time from the commit to the last page's update, which R14
//     bounds at 2 s at both rates; whether each page then shows every item the fresh read answers
//     (a page that shows another is a wrong delta); and every transaction from the write to its
//     settling, by what wrote it, with the longest of those the write starts (its own, the
//     refresher's, the production signals' and the change log's drain), and the live hub's
//     recompute lines and the refresher's transaction lines of the server's log;
//  4. the warm reads of each project's two first-paint reads, written to -expect-file, which the
//     first paints after the restarts that follow must answer item for item (`measure paint -mode
//     k14`).
//
// It fails a write past 2 s, a page that never showed the fresh read's value or shows other items
// than it, a write that moved no value the pages show (the fixture did not arrange the move), a
// write whose commit the log does not hold, fewer than three writes measured, a kind of write never
// measured, and a k14 project with no daily whole failure write. At ten times the stated rate it
// fails the same: R14 binds the writes at both rates. Each RESULT line names the rate and the clock.

// The three kinds of write k14 names.
const (
	kindSourceMove  = "source-move"
	kindDefaultMove = "default-move"
	kindRuleChange  = "rule-change"
)

// k14Kinds is the three kinds, in the order k14 writes them.
var k14Kinds = []string{kindSourceMove, kindDefaultMove, kindRuleChange}

// k14WriteBudgetMs is R14's bound on a write: from its commit to the update of the last open page.
const k14WriteBudgetMs = 2000

// k14MinWrites is the fewest writes k14 measures at a rate (the parent's review of Verification
// v14, dispatch://INFRA-978/comment/39ab37f8-ccd0-4f3d-b200-f38f0f178c38).
const k14MinWrites = 3

// pageWait bounds how long k14 waits for a page to show a write's move, and quietWait for the
// server to be quiet before a write and after it.
const (
	pageWait  = 5 * time.Minute
	quietWait = 15 * time.Minute
)

// settingsStatement matches the settings route's write (delivery.PutProjectSettings), and
// defaultStatement the default-repository route's (api's project_default_repository.go).
var (
	settingsStatement = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+project_(?:delivery_settings|deploy_sources)\b`)
	defaultStatement  = regexp.MustCompile(`(?is)\bupdate\s+projects\s+set\s+default_repo\b`)
)

// k14Write is one write: its name, its kind, the project whose pages it moves, the eval whose value
// the pages must show moved, the statement of the route's write, and the write itself.
type k14Write struct {
	name, kind, project string
	watched             string
	statement           *regexp.Regexp
	send                func(context.Context) (int, []byte, error)
}

// k14Run is one invocation's state: the report, the flags, the database, the client and the pages.
type k14Run struct {
	r      *report
	o      options
	pool   *store.Pool
	client *http.Client
	pages  map[string][]*livePage
}

// rate is the run rate the fixture is loaded at, as each RESULT line names it.
func (k *k14Run) rate() string {
	if k.r.tenTimes {
		return "ten-times"
	}
	return "stated"
}

// stamp is what every measured RESULT line of k14 ends with: the clock, the rate, the scale and
// the node.
func (k *k14Run) stamp() string {
	return fmt.Sprintf("at=%s rate=%s scale=%s node=%s", time.Now().UTC().Format(time.RFC3339Nano), k.rate(), cmp.Or(k.o.scale, "unnamed"),
		cmp.Or(k.o.node, "unnamed"))
}

// k14 is `measure k14` (the comment above).
func k14(ctx context.Context, r *report, o options) error {
	database, err := store.Open(ctx, withApplication(o.database))
	if err != nil {
		return err
	}
	defer database.Pool.Close()
	k := &k14Run{r: r, o: o, pool: database.Pool, client: &http.Client{Timeout: 2 * time.Minute}, pages: map[string][]*livePage{}}
	if err := setStatementLog(ctx, k.pool, true); err != nil {
		return err
	}
	defer func() {
		if err := setStatementLog(context.WithoutCancel(ctx), k.pool, false); err != nil {
			r.fail("statement-log %v", err)
		}
	}()
	r.result("k14 args=%q today=%s %s", strings.Join(os.Args[1:], " "), o.now.Format(time.DateOnly), k.stamp())
	k.daily(ctx)
	k.github("before the writes")
	defer func() {
		for _, pages := range k.pages {
			for _, page := range pages {
				page.close()
			}
		}
	}()
	for _, project := range evalbench.K14Projects {
		for _, one := range k14Windows(o.now) {
			page, err := openPage(ctx, o.url, o.token, project+"-"+one.name, project, one.window, one.compare)
			if err != nil {
				return err
			}
			k.pages[project] = append(k.pages[project], page)
		}
	}
	measured := map[string]int{}
	for _, write := range k.writes() {
		if k.measure(ctx, write) {
			measured[write.kind]++
		}
	}
	k.judgeCount(measured)
	k.github("after the writes")
	return k.expect(ctx)
}

// k14Window is one page's window: its name, the window and its compare range, none where nil.
type k14Window struct {
	name    string
	window  window
	compare *window
}

// k14Windows is the pages' windows, each ending on today, the day the history ends: 84 days with
// the 84-day compare range before them, the first paint's, 28 days, and 7 days.
func k14Windows(today time.Time) []k14Window {
	c84 := before(today.AddDate(0, 0, -84), 84)
	return []k14Window{
		{name: "84d-compare", window: before(today, 84), compare: &c84},
		{name: "28d", window: before(today, 28)},
		{name: "7d", window: before(today, 7)},
	}
}

// writes is k14's writes, in order, each followed by its move back.
func (k *k14Run) writes() []k14Write {
	pair, mixed := evalbench.PairProject, evalbench.MixedProject
	service := evalbench.PairRepos[1]
	pairWatched, mixedWatched := evals.EvalID(pair, evals.SlugDeployFrequency), evals.EvalID(mixed, evals.SlugDeployFrequency)
	return []k14Write{
		{name: "source-move", kind: kindSourceMove, project: pair, watched: pairWatched, statement: settingsStatement,
			send: k.editSource(pair, service, func(source *k14Source) { source.Repo = new(evalbench.PairSpare) })},
		{name: "source-move-back", kind: kindSourceMove, project: pair, watched: pairWatched, statement: settingsStatement,
			send: k.editSource(pair, evalbench.PairSpare, func(source *k14Source) { source.Repo = new(service) })},
		{name: "default-move", kind: kindDefaultMove, project: mixed, watched: mixedWatched, statement: defaultStatement,
			send: k.moveDefault(mixed, evalbench.MixedRepos[0], evalbench.MixedSpare)},
		{name: "default-move-back", kind: kindDefaultMove, project: mixed, watched: mixedWatched, statement: defaultStatement,
			send: k.moveDefault(mixed, evalbench.MixedSpare, evalbench.MixedRepos[0])},
		{name: "rule-change", kind: kindRuleChange, project: pair, watched: pairWatched, statement: settingsStatement,
			send: k.editSource(pair, service, func(source *k14Source) { source.ProductionJobNames = []string{evalbench.RuleChangeJob} })},
		{name: "rule-change-back", kind: kindRuleChange, project: pair, watched: pairWatched, statement: settingsStatement,
			send: k.editSource(pair, service, func(source *k14Source) { source.ProductionJobNames = []string{evalbench.ProductionJob} })},
	}
}

// k14Source is one deploy source as the settings route answers it and takes it.
type k14Source struct {
	Kind                    string   `json:"kind"`
	Repo                    *string  `json:"repo"`
	DeployWorkflowPath      *string  `json:"deploy_workflow_path"`
	ProductionJobNames      []string `json:"production_job_names"`
	ProductionApplyStepName *string  `json:"production_apply_step_name"`
	StagingE2EJobName       *string  `json:"staging_e2e_job_name"`
	StagingApplyJobName     *string  `json:"staging_apply_job_name"`
	PRChecksWorkflowPath    *string  `json:"pr_checks_workflow_path"`
	PRChecksRollupJobName   *string  `json:"pr_checks_rollup_job_name"`
}

// k14Settings is the project's delivery settings as the route answers them (GET), whose every
// field the save sends back.
type k14Settings struct {
	Delivery              bool        `json:"delivery"`
	DefaultRepo           *string     `json:"default_repo"`
	Sources               []k14Source `json:"sources"`
	PopulationAuthors     []string    `json:"population_authors"`
	ExcludedRepos         []string    `json:"excluded_repos"`
	FailureSources        []string    `json:"failure_sources"`
	FailuresMeasuredSince *string     `json:"failures_measured_since"`
}

// k14Actor is the actor k14's writes name, as k16's move does.
var k14Actor = map[string]string{"kind": "session", "id": "evalbench"}

// readSettings reads project's delivery settings through the route.
func (k *k14Run) readSettings(ctx context.Context, project string) (k14Settings, error) {
	status, body, err := getBody(ctx, k.client, k.o.url+"/api/v1/projects/"+project+"/settings/delivery", k.o.token)
	if err != nil {
		return k14Settings{}, err
	}
	if status != http.StatusOK {
		return k14Settings{}, fmt.Errorf("the settings read of %s answered %d: %s", project, status, body)
	}
	var settings k14Settings
	if err := json.Unmarshal(body, &settings); err != nil {
		return k14Settings{}, fmt.Errorf("read the settings of %s: %w", project, err)
	}
	return settings, nil
}

// editSource is a settings save of project that changes, with edit, its github_workflow source
// that names repo, every other field as the route answered it: the body a person's save sends,
// PUT /api/v1/projects/{key}/settings/delivery. The read of the settings is made before the write's
// clock starts.
func (k *k14Run) editSource(project, repo string, edit func(*k14Source)) func(context.Context) (int, []byte, error) {
	return func(ctx context.Context) (int, []byte, error) {
		settings, err := k.readSettings(ctx, project)
		if err != nil {
			return 0, nil, err
		}
		at := slices.IndexFunc(settings.Sources, func(source k14Source) bool { return source.Repo != nil && *source.Repo == repo })
		if at < 0 {
			return 0, nil, fmt.Errorf("no source of %s names %s", project, repo)
		}
		edit(&settings.Sources[at])
		body, err := json.Marshal(map[string]any{
			"delivery": settings.Delivery, "sources": settings.Sources, "population_authors": settings.PopulationAuthors,
			"excluded_repos": settings.ExcludedRepos, "failure_sources": settings.FailureSources,
			"failures_measured_since": settings.FailuresMeasuredSince, "actor": k14Actor,
		})
		if err != nil {
			return 0, nil, err
		}
		return k.timedPut(ctx, "/api/v1/projects/"+project+"/settings/delivery", body)
	}
}

// moveDefault is a move of project's default repository from was to now, as a person's page makes
// it: PUT /api/v1/projects/{key}/default-repository.
func (k *k14Run) moveDefault(project, was, now string) func(context.Context) (int, []byte, error) {
	return func(ctx context.Context) (int, []byte, error) {
		settings, err := k.readSettings(ctx, project)
		if err != nil {
			return 0, nil, err
		}
		if settings.DefaultRepo == nil || *settings.DefaultRepo != was {
			return 0, nil, fmt.Errorf("%s's default repository is %v, not %s", project, settings.DefaultRepo, was)
		}
		body, err := json.Marshal(map[string]any{"repo": now, "actor": k14Actor})
		if err != nil {
			return 0, nil, err
		}
		return k.timedPut(ctx, "/api/v1/projects/"+project+"/default-repository", body)
	}
}

// sentAt is when the write's request left, set by timedPut, the start of the window its
// transactions are read in.
type sentAt struct{}

// timedPut is putJSON of body to path, recording when the request left in the context's sentAt.
func (k *k14Run) timedPut(ctx context.Context, path string, body []byte) (int, []byte, error) {
	if sent, ok := ctx.Value(sentAt{}).(*time.Time); ok {
		*sent = time.Now()
	}
	return putJSON(ctx, k.client, k.o.url+path, k.o.token, body)
}

// k14Measured is what k14 read of one write, which judgeWrite judges.
type k14Measured struct {
	write            k14Write
	status           int
	answer           []byte
	sent, answered   time.Time
	committed        *time.Time
	routeMs          float64
	reaches          []pageReach
	unmoved          []string
	transactions     []transaction
	kinds            []string
	background       int
	backgroundMs     float64
	longest          *transaction
	longestKind      string
	settled          time.Time
	settledWaitMs    float64
	hubLines, txLogs []string
}

// lastPageMs is the time from the commit to the last page's update, and false where the commit or
// a page's update is missing.
func (m k14Measured) lastPageMs() (float64, bool) {
	if m.committed == nil || len(m.reaches) == 0 {
		return 0, false
	}
	var last time.Time
	for _, reach := range m.reaches {
		if reach.reached == nil {
			return 0, false
		}
		if reach.reached.After(last) {
			last = *reach.reached
		}
	}
	return float64(last.Sub(*m.committed).Microseconds()) / 1000, true
}

// measure makes one write and reads what it did (the package comment's step 3), and answers whether
// it was measured whole and within its bound.
func (k *k14Run) measure(ctx context.Context, write k14Write) bool {
	waited, err := awaitQuiet(ctx, k.pool, quietWait)
	if err != nil {
		k.r.fail("k14 write=%s: before the write: %v", write.name, err)
		return false
	}
	k.r.result("k14-quiet write=%s when=before waited_ms=%d %s", write.name, waited.Milliseconds(), k.stamp())
	pages := k.pages[write.project]
	before := map[string]string{}
	for _, page := range pages {
		if err := page.failed(); err != nil {
			k.r.fail("k14 write=%s: %v", write.name, err)
			return false
		}
		before[page.name] = page.value(write.watched)
	}
	logFrom, err := fileSize(k.o.pgLog)
	if err != nil {
		k.r.fail("k14 write=%s: read the statement log: %v", write.name, err)
		return false
	}
	serverFrom, err := fileSize(k.o.serverLog)
	if err != nil {
		k.r.fail("k14 write=%s: read the server log: %v", write.name, err)
		return false
	}
	m := k14Measured{write: write}
	m.sent = time.Now()
	status, answer, err := write.send(context.WithValue(ctx, sentAt{}, &m.sent))
	m.answered, m.status, m.answer = time.Now(), status, answer
	if err != nil || status != http.StatusOK {
		k.r.fail("k14 write=%s: the write answered %d (%v): %.400s", write.name, status, err, answer)
		return false
	}
	// Each page shows the move, or the wait gives up and the judgement names the page.
	for _, page := range pages {
		for deadline := m.sent.Add(pageWait); page.value(write.watched) == before[page.name] && time.Now().Before(deadline) && page.failed() == nil; {
			time.Sleep(20 * time.Millisecond)
		}
	}
	waited, err = awaitQuiet(ctx, k.pool, quietWait)
	if err != nil {
		k.r.fail("k14 write=%s: after the write: %v", write.name, err)
		return false
	}
	time.Sleep(500 * time.Millisecond)
	m.settled, m.settledWaitMs = time.Now(), float64(waited.Milliseconds())
	for _, page := range pages {
		status, body, err := getBody(ctx, k.client, k.o.url+page.path, k.o.token)
		if err != nil || status != http.StatusOK {
			k.r.fail("k14 write=%s page=%s: the fresh read answered %d (%v): %.400s", write.name, page.name, status, err, body)
			return false
		}
		fresh, err := itemsOf(body)
		if err != nil {
			k.r.fail("k14 write=%s page=%s: read the fresh read: %v", write.name, page.name, err)
			return false
		}
		if fresh[write.watched] == "" || fresh[write.watched] == before[page.name] {
			m.unmoved = append(m.unmoved, page.name)
		}
		m.reaches = append(m.reaches, reachOf(page, write.watched, m.sent, fresh[write.watched], fresh))
	}
	transactions, err := scanTransactions(k.o.pgLog, logFrom, markWrite(write.statement))
	if err != nil {
		k.r.fail("k14 write=%s: %v", write.name, err)
		return false
	}
	readTransactions(&m, transactions)
	m.hubLines, m.txLogs, err = serverLines(k.o.serverLog, serverFrom)
	if err != nil {
		k.r.fail("k14 write=%s: read the server log: %v", write.name, err)
		return false
	}
	return judgeWrite(k.r, m, k.stamp())
}

// readTransactions reads, from the statement log's transactions from the write on, the write's own
// transaction and commit and every transaction from the write to its settling, but the
// instrument's own.
func readTransactions(m *k14Measured, transactions []transaction) {
	own, found := writeOf(transactions, m.sent.Add(-500*time.Millisecond), m.answered)
	if found && own.ended == "commit" {
		m.committed, m.routeMs = new(own.end), own.ms()
	}
	for _, one := range transactions {
		if one.app == measureApp || one.began.Before(m.sent.Add(-500*time.Millisecond)) || one.began.After(m.settled) {
			continue
		}
		kind := kindOf(one, own)
		if !startedBy(kind) {
			m.background++
			m.backgroundMs += one.ms()
			continue
		}
		m.transactions, m.kinds = append(m.transactions, one), append(m.kinds, kind)
		if m.longest == nil || one.ms() > m.longest.ms() {
			m.longest, m.longestKind = new(one), kind
		}
	}
}

// judgeWrite prints one write's lines and judges it (the package comment), answering whether it
// was measured whole and within its bound.
func judgeWrite(r *report, m k14Measured, stamp string) bool {
	w := m.write
	ok := true
	last, whole := m.lastPageMs()
	committed, longest, longestKind := "none", 0.0, "none"
	if m.committed != nil {
		committed = m.committed.UTC().Format(time.RFC3339Nano)
	}
	if m.longest != nil {
		longest, longestKind = m.longest.ms(), m.longestKind
	}
	lastText := "none"
	if whole {
		lastText = strconv.FormatFloat(last, 'f', 1, 64)
	}
	r.result("k14-write write=%s kind=%s project=%s status=%d put_ms=%.1f commit=%s route_transaction_ms=%.1f pages=%d last_page_ms=%s budget_ms=%d longest_transaction_ms=%.1f longest_transaction_kind=%s transactions_started=%d background_transactions=%d background_ms=%.1f settle_wait_ms=%.0f hub_recomputes=%d %s",
		w.name, w.kind, w.project, m.status, float64(m.answered.Sub(m.sent).Microseconds())/1000, committed, m.routeMs, len(m.reaches), lastText,
		k14WriteBudgetMs, longest, longestKind, len(m.transactions), m.background, m.backgroundMs, m.settledWaitMs, len(m.hubLines), stamp)
	for _, reach := range m.reaches {
		reachedMs := "none"
		if reach.reached != nil && m.committed != nil {
			reachedMs = strconv.FormatFloat(float64(reach.reached.Sub(*m.committed).Microseconds())/1000, 'f', 1, 64)
		}
		r.result("k14-page write=%s page=%s reached_ms=%s updates=%d items_as_fresh_read=%t differing=%q %s", w.name, reach.page, reachedMs,
			reach.updates, len(reach.differing) == 0, reach.differing, stamp)
		switch {
		case reach.reached == nil:
			r.fail("k14 write=%s page=%s: the page never came to show %s as a fresh read answers it once the write settled", w.name, reach.page, w.watched)
			ok = false
		case len(reach.differing) > 0:
			r.fail("k14 write=%s page=%s: a wrong delta: the page shows items other than a fresh read answers: %s", w.name, reach.page, strings.Join(reach.differing, ", "))
			ok = false
		}
	}
	for at, one := range m.transactions {
		r.result("k14-transaction write=%s kind=%s pid=%d began=%s ms=%.1f statements=%d ended=%s writes=[%s] after_last_page=%t", w.name, m.kinds[at], one.pid,
			one.began.UTC().Format(time.RFC3339Nano), one.ms(), one.statements, one.ended, strings.Join(one.writes, " "), whole && m.committed != nil &&
				one.began.Sub(*m.committed).Seconds()*1000 > last)
	}
	for _, line := range m.hubLines {
		r.result("k14-hub write=%s %s", w.name, line)
	}
	for _, line := range m.txLogs {
		r.result("k14-server write=%s %s", w.name, line)
	}
	switch {
	case len(m.unmoved) > 0:
		r.fail("k14 write=%s: the write moved no value of %s on page %s, so the pages measured no update of it", w.name, w.watched, strings.Join(m.unmoved, ", "))
		ok = false
	case m.committed == nil:
		r.fail("k14 write=%s: the statement log holds no commit of the write's statement, so its time from the commit is unknown", w.name)
		ok = false
	case !whole:
		ok = false
	case last > k14WriteBudgetMs:
		r.fail("k14 write=%s: the last page's update came %.1f ms after the write's commit, over R14's %d ms", w.name, last, k14WriteBudgetMs)
		ok = false
	}
	return ok
}

// judgeCount fails a run that measured fewer than k14MinWrites writes, or no write of a kind.
func (k *k14Run) judgeCount(measured map[string]int) {
	total := 0
	var kinds []string
	for _, kind := range k14Kinds {
		total += measured[kind]
		kinds = append(kinds, fmt.Sprintf("%s=%d", kind, measured[kind]))
	}
	k.r.result("k14-writes measured=%d minimum=%d by_kind=[%s] %s", total, k14MinWrites, strings.Join(kinds, " "), k.stamp())
	judgeWriteCount(k.r, measured)
}

// judgeWriteCount fails fewer than k14MinWrites writes measured whole and within their bound, and a
// kind of write none of them is.
func judgeWriteCount(r *report, measured map[string]int) {
	total := 0
	for _, kind := range k14Kinds {
		total += measured[kind]
		if measured[kind] == 0 {
			r.fail("k14: no %s write was measured whole within its bound", kind)
		}
	}
	if total < k14MinWrites {
		r.fail("k14: %d writes were measured whole within their bound, fewer than %d", total, k14MinWrites)
	}
}

// awaitQuiet waits until the server is quiet, and answers how long it waited: no cut of the stored
// figures marked stale, no rebuild building, no change waiting in the change log's outbox, and no
// backend of the server's busy, on six polls in a row a quarter of a second apart. It fails once
// wait has passed.
func awaitQuiet(ctx context.Context, pool *store.Pool, wait time.Duration) (time.Duration, error) {
	started, quiet := time.Now(), 0
	for {
		var stale, building, pending, busy int64
		if err := pool.QueryRow(ctx, `
			select (select count(*) from eval_figure_cuts_stale), (select count(*) from eval_rebuilds where state = 'building'),
			       (select count(*) from delivery_changes_pending),
			       (select count(*) from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid()
			          and backend_type = 'client backend' and coalesce(application_name, '') <> $1 and state <> 'idle')
		`, measureApp).Scan(&stale, &building, &pending, &busy); err != nil {
			return time.Since(started), fmt.Errorf("read whether the server is quiet: %w", err)
		}
		if stale+building+pending+busy == 0 {
			quiet++
		} else {
			quiet = 0
		}
		if quiet >= 6 {
			return time.Since(started), nil
		}
		if time.Since(started) > wait {
			return time.Since(started), fmt.Errorf("the server was not quiet %s on: %d stale cuts, %d rebuilds building, %d changes in the outbox, %d busy backends",
				wait, stale, building, pending, busy)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// hubLine matches the live hub's recompute line of an evals view (api's finishRecompute).
var hubLine = regexp.MustCompile(`dispatch: delivery live recompute view=evals cause=\S+ elapsed_ms=\d+.*`)

// transactionField matches a refresher line that states the time of its transaction.
var transactionField = regexp.MustCompile(`\b(?:transaction_ms|publication_ms)=\d+`)

// serverLines reads the server's log from offset: the evals views' recompute lines, and every line
// that states the time of a transaction.
func serverLines(path string, offset int64) (hub, transactions []string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		text := scanner.Text()
		if match := hubLine.FindString(text); match != "" {
			hub = append(hub, match)
		}
		if transactionField.MatchString(text) {
			transactions = append(transactions, text)
		}
	}
	return hub, transactions, scanner.Err()
}

// The daily whole failure write (the package comment's step 1).

// dailyWait bounds how long k14 waits for the server's first walking pass to write each k14
// project's failure rows whole.
const dailyWait = 5 * time.Minute

// wholeFailureWrite matches the statement of a project's whole write of its failure rows that
// deletes its rows outside the days written (evals' Store.writeFailureDays), which only the whole
// write runs.
var wholeFailureWrite = regexp.MustCompile(`(?s)delete from eval_days where project_key = \$1 and eval_id = any\(\$2\) and writer = \$3`)

// slowPass matches the refresher's slow-pass line (evals' refreshed.logSlow), and passField each of
// its fields.
var (
	slowPass  = regexp.MustCompile(`dispatch: the eval day-row refresher's pass took 100 ms or more`)
	passField = regexp.MustCompile(`\b(total_ms|batch|yielded|walks_left|pages|changes|daily_ms)=(\S+)`)
)

// daily reads the daily whole failure write of the server's first walking pass since it started
// (-pg-log-from, the statement log's size when run.sh started it): each project's whole write as
// its transaction, and the slow-pass lines that hold it. It fails a k14 project none was found of
// within dailyWait.
func (k *k14Run) daily(ctx context.Context) {
	deadline := time.Now().Add(dailyWait)
	var writes []dailyWrite
	for {
		transactions, err := scanTransactions(k.o.pgLog, k.o.pgLogFrom, markWholeFailureWrite)
		if err != nil {
			k.r.fail("k14-daily: %v", err)
			return
		}
		writes = dailyWrites(transactions)
		found := map[string]bool{}
		for _, one := range writes {
			found[one.project] = true
		}
		if !slices.ContainsFunc(evalbench.K14Projects, func(project string) bool { return !found[project] }) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	passes, err := slowPasses(k.o.serverLog)
	if err != nil {
		k.r.fail("k14-daily: read the server log: %v", err)
	}
	judgeDaily(k.r, writes, passes, k.stamp())
}

// dailyWrite is one project's whole failure write.
type dailyWrite struct {
	project string
	tx      transaction
}

// markWholeFailureWrite marks a statement of a whole failure write (wholeFailureWrite) with the
// project its first parameter names.
func markWholeFailureWrite(line logLine) string {
	if !wholeFailureWrite.MatchString(line.text) || len(line.params) == 0 {
		return ""
	}
	return strings.Trim(line.params[0], "'")
}

// dailyWrites is each committed whole failure write of transactions, read with
// markWholeFailureWrite, in the order they began, under the project its mark names.
func dailyWrites(transactions []transaction) []dailyWrite {
	var writes []dailyWrite
	for _, one := range transactions {
		if one.app != measureApp && one.ended == "commit" && one.mark != "" {
			writes = append(writes, dailyWrite{project: one.mark, tx: one})
		}
	}
	return writes
}

// slowPasses is the fields of each slow-pass line of the server's log that spent time on the daily
// work (daily_ms above 0).
func slowPasses(path string) ([]map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	var passes []map[string]string
	for scanner.Scan() {
		if !slowPass.MatchString(scanner.Text()) {
			continue
		}
		fields := map[string]string{}
		for _, match := range passField.FindAllStringSubmatch(scanner.Text(), -1) {
			fields[match[1]] = match[2]
		}
		if fields["daily_ms"] != "" && fields["daily_ms"] != "0" {
			passes = append(passes, fields)
		}
	}
	return passes, scanner.Err()
}

// judgeDaily prints the daily whole failure writes and the passes that made them, and fails a k14
// project with none. Each project's write is one stretch the walk does not stop in, so the longest
// of them is the longest a change owed during the walk waits for the pass that serves it.
func judgeDaily(r *report, writes []dailyWrite, passes []map[string]string, stamp string) {
	longest, total, project := 0.0, 0.0, "none"
	found := map[string]bool{}
	for _, one := range writes {
		found[one.project] = true
		total += one.tx.ms()
		if one.tx.ms() > longest {
			longest, project = one.tx.ms(), one.project
		}
		r.result("k14-daily-write project=%s began=%s ms=%.1f statements=%d writes=[%s] %s", one.project, one.tx.began.UTC().Format(time.RFC3339Nano),
			one.tx.ms(), one.tx.statements, strings.Join(one.tx.writes, " "), stamp)
	}
	yielded := 0
	for _, pass := range passes {
		if pass["yielded"] == "true" {
			yielded++
		}
		var fields []string
		for _, name := range slices.Sorted(maps.Keys(pass)) {
			fields = append(fields, name+"="+pass[name])
		}
		r.result("k14-daily-pass %s %s", strings.Join(fields, " "), stamp)
	}
	r.result("k14-daily writes=%d total_ms=%.1f longest_ms=%.1f longest_project=%s owed_change_wait_bound_ms=%.1f passes=%d passes_yielded=%d %s",
		len(writes), total, longest, project, longest, len(passes), yielded, stamp)
	for _, key := range evalbench.K14Projects {
		if !found[key] {
			r.fail("k14-daily: no whole write of %s's failure rows was found in the statement log since the server started, so the daily failure write was not measured", key)
		}
	}
}

// github prints how many requests the GitHub stand-in served by route (-github-log), and fails a
// run whose settings writes reached no source check.
func (k *k14Run) github(when string) {
	if k.o.githubLog == "" {
		return
	}
	file, err := os.Open(k.o.githubLog)
	if err != nil {
		k.r.fail("k14-github: %v", err)
		return
	}
	defer file.Close()
	counts := map[string]int{}
	route := regexp.MustCompile(`\broute=(\S+) method=\S+ path=\S+ status=(\d+)`)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if match := route.FindStringSubmatch(scanner.Text()); match != nil {
			counts[match[1]+"/"+match[2]]++
		}
	}
	var lines []string
	for _, key := range slices.Sorted(maps.Keys(counts)) {
		lines = append(lines, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	k.r.result("k14-github when=%q requests=[%s] %s", when, strings.Join(lines, " "), k.stamp())
	if when == "after the writes" && counts["repository/200"] == 0 {
		k.r.fail("k14-github: the stand-in answered no repository read, so no settings write's source check reached it")
	}
}

// k14Expected is one warm read of a first paint's read (expect), which the paint after the restart
// must answer item for item.
type k14Expected struct {
	Project string            `json:"project"`
	Case    string            `json:"case"`
	Items   map[string]string `json:"items"`
}

// expect writes, to -expect-file, each k14 project's two first-paint reads as the warm server
// answers them once the writes are back where they began.
func (k *k14Run) expect(ctx context.Context) error {
	if k.o.expectFile == "" {
		return nil
	}
	if _, err := awaitQuiet(ctx, k.pool, quietWait); err != nil {
		return fmt.Errorf("before the warm reads: %w", err)
	}
	file, err := os.Create(k.o.expectFile)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, project := range evalbench.K14Projects {
		for _, one := range k14Cases(project, k.o.now, 1) {
			status, body, err := getBody(ctx, k.client, k.o.url+one.path, k.o.token)
			if err != nil || status != http.StatusOK {
				return fmt.Errorf("the warm read %s of %s answered %d (%v): %.400s", one.name, project, status, err, body)
			}
			items, err := itemsOf(body)
			if err != nil {
				return err
			}
			if err := encoder.Encode(k14Expected{Project: project, Case: one.name, Items: items}); err != nil {
				return err
			}
			k.r.result("k14-expect project=%s case=%s items=%d %s", project, one.name, len(items), k.stamp())
		}
	}
	return nil
}

// readExpected reads the warm reads expect wrote.
func readExpected(path string) ([]k14Expected, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var expected []k14Expected
	decoder := json.NewDecoder(file)
	for {
		var one k14Expected
		if err := decoder.Decode(&one); errors.Is(err, io.EOF) {
			return expected, nil
		} else if err != nil {
			return nil, err
		}
		expected = append(expected, one)
	}
}
