package workflow

import (
	"cmp"
	"context"
	"log/slog"
	"reflect"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A step record names the model its agent's launches were set to. It also names each model that
// answered the agent's own calls during the step, with its count. The agent's shim counts the
// calls of its launch, and the daemon reads those totals before each fact (Config.ReadTotals). A
// step's calls are what the shim counted between the read its start used and the read its end
// used. A launch that started during the step counts from zero. The final totals of a launch that
// ended during the step stand in for the read at its end. Where any of these is missing, the
// record says that it does not know its counts.

// FactReads is each read of a launch's totals that a fact made before its transaction and that
// gave totals, by launch (Config.ReadTotals).
type FactReads map[record.LaunchKey]record.LaunchRead

// appliedFact is the fact a transaction applies, and the reads of its agents' totals that its
// read made before the transaction (Engine.ReadFact).
type appliedFact struct {
	fact  intake.Fact
	reads FactReads
}

// appliedFactKey is the context key of the fact a transaction applies (withAppliedFact).
type appliedFactKey struct{}

// withAppliedFact is ctx carrying fact and the reads made for it.
func withAppliedFact(ctx context.Context, fact intake.Fact, reads FactReads) context.Context {
	return context.WithValue(ctx, appliedFactKey{}, appliedFact{fact: fact, reads: reads})
}

// appliedFactOf is the fact ctx carries, and false in a transaction no fact opened.
func appliedFactOf(ctx context.Context) (appliedFact, bool) {
	applied, ok := ctx.Value(appliedFactKey{}).(appliedFact)
	return applied, ok
}

var _ intake.Reader = (*Engine)(nil)

// ReadFact reads, before fact's transaction, the totals of each live launch of the agents whose
// steps fact can start or end (intake.Reader). It answers ctx carrying fact and those reads.
//
// The agents are the claims of the issue fact names, and the architects of its tree
// (record.Store.StepAgentClaims). A fact of a pull request or a branch names the issue of its
// pull request. A fact that names no issue reads nothing. The issue and its agents are read in one
// read-only transaction on pool. That transaction ends before the totals are read, so a read that
// waits holds no connection. Without Config.ReadTotals nothing is read.
func (e *Engine) ReadFact(ctx context.Context, pool store.TxPool, fact intake.Fact) context.Context {
	var reads FactReads
	if e.cfg.ReadTotals != nil {
		agents, err := store.ReadOnly(ctx, pool, "read the agents of the steps a fact can start or end", func(ctx context.Context, tx pgx.Tx) ([]claim.Token, error) {
			issue, err := e.factIssue(ctx, tx, fact)
			if err != nil || issue == "" {
				return nil, err
			}
			return e.store.StepAgentClaims(ctx, tx, issue)
		})
		// The read-only transaction has ended here.
		switch {
		case err != nil:
			e.log.Warn("workflow: a fact reads no agent's calls, since the read of its issue's agents failed", "error", err)
		case len(agents) > 0:
			reads = e.cfg.ReadTotals(ctx, agents)
		}
	}
	return withAppliedFact(ctx, fact, reads)
}

// factIssue is the issue fact names: its own, or the issue of the pull request it names by number
// or by branch. That pull request is read as the fact's handlers read it (pullRequestRow,
// pullRequestRowByBranch). So it is found under a renamed repository's other name too, and only
// where its issue is on the fact's repository. It is "" for a fact that names none, or a pull
// request the store does not hold.
func (e *Engine) factIssue(ctx context.Context, tx pgx.Tx, fact intake.Fact) (string, error) {
	if issue, ok := intake.IssueOf(fact); ok {
		return issue, nil
	}
	var pr *record.PullRequest
	var err error
	if push, ok := fact.(intake.Push); ok {
		pr, err = e.pullRequestRowByBranch(ctx, tx, push.Repo, push.Branch)
	} else if repo, number, ok := pullRequestOf(fact); ok {
		pr, err = e.pullRequestRow(ctx, tx, repo, number)
	}
	if err != nil || pr == nil {
		return "", err
	}
	return pr.Issue, nil
}

// pullRequestOf is the repository and number of the pull request fact is of, for a fact of one.
func pullRequestOf(fact intake.Fact) (string, int, bool) {
	switch fact := fact.(type) {
	case intake.PullRequestReview:
		return fact.Repo, fact.Number, true
	case intake.PullRequestChecks:
		return fact.Repo, fact.Number, true
	case intake.PullRequestMergeability:
		return fact.Repo, fact.Number, true
	case intake.PullRequestMerged:
		return fact.Repo, fact.Number, true
	case intake.PullRequestClosed:
		return fact.Repo, fact.Number, true
	case intake.PullRequestDequeued:
		return fact.Repo, fact.Number, true
	case intake.PullRequestSynchronized:
		return fact.Repo, fact.Number, true
	case intake.PullRequestOpened:
		return fact.Repo, fact.Number, true
	}
	return "", 0, false
}

// factReads is the reads the fact of ctx made, nil in a transaction no fact opened.
func factReads(ctx context.Context) FactReads {
	applied, _ := appliedFactOf(ctx)
	return applied.reads
}

// launchAt is the launch row of generation among launches, nil for none.
func launchAt(launches record.AgentLaunches, generation uint64) *record.Launch {
	for i := range launches.Launches {
		if launches.Launches[i].Generation == generation {
			return &launches.Launches[i]
		}
	}
	return nil
}

// usedRead is the read a step uses of a live launch. It is the newer, by its read number, of the
// fact's read of key and the newest read a committed fact kept (record.Launch.Newest). It is nil
// where the fact did not read key. A read the fact did not make cannot show that the launch made
// no call since the kept one.
func usedRead(reads FactReads, key record.LaunchKey, launch *record.Launch) *record.LaunchRead {
	read, ok := reads[key]
	if !ok {
		return nil
	}
	if launch != nil && launch.Newest != nil && launch.Newest.Totals.Read > read.Totals.Read {
		newest := *launch.Newest
		return &newest
	}
	return &read
}

// keepStartTotals keeps what the start of the step at place on issue read of agent. step is the
// record's id and startedAt the moment the start wrote. It reads the agent's generation and
// liveness now. A live launch's read is the one usedRead gives, which is kept and logged. A start
// with no agent, or an engine that reads no totals, keeps nothing.
func (e *Engine) keepStartTotals(ctx context.Context, tx pgx.Tx, issue string, place record.StepPlace, agent claim.Token, step string, startedAt time.Time) error {
	if agent == "" || e.cfg.ReadTotals == nil {
		return nil
	}
	launches, err := e.store.AgentLaunches(ctx, tx, agent, 0)
	if err != nil {
		return err
	}
	key := record.LaunchKey{Claim: agent, Generation: launches.Generation}
	var used *record.LaunchRead
	if launches.Live {
		used = usedRead(factReads(ctx), key, launchAt(launches, key.Generation))
	}
	if err := e.store.KeepStartTotals(ctx, tx, issue, place, launches.StartTotals(used)); err != nil {
		return err
	}
	if used == nil {
		return nil
	}
	if err := e.store.KeepRead(ctx, tx, key, *used); err != nil {
		return err
	}
	e.logUsedRead(ctx, stepRead{step: step, issue: issue, place: place, startedAt: startedAt, side: "start", key: key, read: *used})
	return nil
}

// launchSpan is one launch of a step's agent during the step. setModel is its set model, "" where
// it is not known. start and end are its totals at the step's start and at its end, nil where not
// known.
type launchSpan struct {
	setModel   string
	start, end *shimwire.Totals
}

// spanRead is a read a step's end used: of which launch, the read, and whether it is the launch's
// final totals.
type spanRead struct {
	key   record.LaunchKey
	read  record.LaunchRead
	final bool
}

// stepSpans is each launch of agent during a step. began is what the step's start kept, and now is
// the agent's launches from began's generation on. starting says the generation now is a launch
// still starting (startingLaunch), and reads are the fact's reads. The launches are the start's
// live launch, each launch from began.FromZero to the generation now, and the launch still
// starting. A launch counts from zero where it started after the start. Its end is the read
// usedRead gives where it is live now. Where it ended, or is live with no such read, its end is the
// final totals its shim sent, if any. A launch is live a moment after its shim sent them and closed,
// until its claim's locator is cleared, and its final totals cannot change. A launch still starting
// has no end. used is each read the end used.
func stepSpans(agent claim.Token, began record.StartTotals, now record.AgentLaunches, starting bool, reads FactReads) (spans []launchSpan, used []spanRead) {
	var generations []uint64
	if began.Live {
		generations = append(generations, began.Generation)
	}
	for _, launch := range now.Launches {
		if launch.Generation >= began.FromZero && launch.Generation <= now.Generation && !slices.Contains(generations, launch.Generation) {
			generations = append(generations, launch.Generation)
		}
	}
	if (now.Live || starting) && now.Generation >= began.FromZero && !slices.Contains(generations, now.Generation) {
		generations = append(generations, now.Generation)
	}
	for _, generation := range generations {
		key := record.LaunchKey{Claim: agent, Generation: generation}
		launch := launchAt(now, generation)
		var span launchSpan
		if began.Live && generation == began.Generation {
			if began.Read != nil {
				span.start = &began.Read.Totals
			}
		} else {
			span.start = &shimwire.Totals{}
		}
		var end *record.LaunchRead
		final := false
		if now.Live && generation == now.Generation {
			end = usedRead(reads, key, launch)
		}
		if end == nil && launch != nil && launch.Final != nil {
			end, final = launch.Final, true
		}
		if end != nil {
			span.end = &end.Totals
			used = append(used, spanRead{key: key, read: *end, final: final})
		}
		known := []*shimwire.Totals{span.end, span.start}
		if launch != nil && launch.Newest != nil {
			known = append(known, &launch.Newest.Totals)
		}
		if launch != nil && launch.Final != nil {
			known = append(known, &launch.Final.Totals)
		}
		for _, totals := range known {
			if totals != nil && totals.SetModel != "" {
				span.setModel = totals.SetModel
				break
			}
		}
		spans = append(spans, span)
	}
	return spans, used
}

// countStep is a step's set model and its answers, from its launches. The set model is the one
// model every launch names, and "" where they differ or one is not known. No launch is no set
// model and no call.
//
// The answers are each model's calls summed over the launches, without zeros, most calls first,
// then by model. They are nil, not known, where a launch's start or end is not known, or its end
// overflowed. They are nil too where a count outside the form changed, or a count fell. They are
// nil where a name is outside the form of a step's agent text, or more than
// steprecord.AnswersMax models remain.
func countStep(spans []launchSpan) (string, *[]record.ModelCalls) {
	if len(spans) == 0 {
		none := []record.ModelCalls{}
		return "", &none
	}
	setModel := spans[0].setModel
	for _, span := range spans[1:] {
		if span.setModel != setModel {
			setModel = ""
		}
	}
	if !steprecord.AgentTextHolds(setModel) {
		setModel = ""
	}
	sums := map[string]uint64{}
	for _, span := range spans {
		if span.start == nil || span.end == nil || span.end.Overflowed || span.end.OutsideForm != span.start.OutsideForm {
			return setModel, nil
		}
		before := map[string]uint64{}
		for _, count := range span.start.Counts {
			before[count.Model] = count.Calls
		}
		for _, count := range span.end.Counts {
			earlier := before[count.Model]
			if count.Calls < earlier {
				return setModel, nil
			}
			delete(before, count.Model)
			if count.Calls > earlier {
				sums[count.Model] += count.Calls - earlier
			}
		}
		for _, earlier := range before {
			if earlier > 0 {
				return setModel, nil
			}
		}
	}
	if len(sums) > steprecord.AnswersMax {
		return setModel, nil
	}
	answers := make([]record.ModelCalls, 0, len(sums))
	for model, calls := range sums {
		if !steprecord.AgentTextHolds(model) {
			return setModel, nil
		}
		answers = append(answers, record.ModelCalls{Model: model, Calls: calls})
	}
	slices.SortFunc(answers, func(a, b record.ModelCalls) int {
		return cmp.Or(cmp.Compare(b.Calls, a.Calls), cmp.Compare(a.Model, b.Model))
	})
	return setModel, &answers
}

// writeCounts gives write, the end of a step's run whose start is start, its set model and its
// answers. It keeps and logs each read the end used. It writes neither where the run's start kept
// no totals, its agent is none, or the engine reads no totals.
func (e *Engine) writeCounts(ctx context.Context, tx pgx.Tx, issue string, place record.StepPlace, start record.StepStart, write *record.StepRecord) error {
	agent := claim.Token(start.Agent)
	if agent == "" || e.cfg.ReadTotals == nil || start.StartTotals == nil {
		return nil
	}
	now, err := e.store.AgentLaunches(ctx, tx, agent, start.StartTotals.Generation)
	if err != nil {
		return err
	}
	starting, err := e.startingLaunch(ctx, tx, agent, *start.StartTotals, now)
	if err != nil {
		return err
	}
	spans, used := stepSpans(agent, *start.StartTotals, now, starting, factReads(ctx))
	setModel, answers := countStep(spans)
	if setModel != "" {
		write.SetModel = &setModel
	}
	write.Answers = answers
	ended := endedBy(ctx, agent)
	for _, read := range used {
		if !read.final {
			if err := e.store.KeepRead(ctx, tx, read.key, read.read); err != nil {
				return err
			}
		}
		e.logUsedRead(ctx, stepRead{step: write.ID, issue: issue, place: place, startedAt: start.StartedAt, side: "end",
			key: read.key, read: read.read, final: read.final, endedBy: ended})
	}
	return nil
}

// startingLaunch says whether the agent's generation now is a launch still starting at the end of a
// step that began as began. Its claim records no process, and the store holds no row of that
// launch. The claim is launching, or the boot found its launch unfinished (launch_uncertain). A try
// of that launch was written, so its process may run before the store records it, and no read of it
// exists. Only a generation at or past began.FromZero can be a launch of the step.
func (e *Engine) startingLaunch(ctx context.Context, tx pgx.Tx, agent claim.Token, began record.StartTotals, now record.AgentLaunches) (bool, error) {
	if now.Live || now.Generation == 0 || now.Generation < began.FromZero || launchAt(now, now.Generation) != nil {
		return false, nil
	}
	claims, err := e.store.RoleClaims(ctx, tx, []claim.Token{agent})
	if err != nil {
		return false, err
	}
	held, ok := claims[agent]
	return ok && (held.State == supervise.StateLaunching || held.State == supervise.StateLaunchUncertain), nil
}

// endedBy is who ended a step whose agent is agent, as the end's log line says it. It is agent
// where the fact that ended it is that agent's own completion or sign-off. Else it is the fact's Go
// type name, and settle in a transaction no fact opened.
func endedBy(ctx context.Context, agent claim.Token) string {
	applied, ok := appliedFactOf(ctx)
	if !ok || applied.fact == nil {
		return "settle"
	}
	switch fact := applied.fact.(type) {
	case intake.HandoffComplete:
		if fact.Claim == agent {
			return "agent"
		}
	case intake.SignOff:
		if fact.By == agent {
			return "agent"
		}
	}
	kind := reflect.TypeOf(applied.fact)
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	return kind.Name()
}

// stepRead is one read a step's start or end used, with the step it was used for.
type stepRead struct {
	step      string
	issue     string
	place     record.StepPlace
	startedAt time.Time
	side      string
	key       record.LaunchKey
	read      record.LaunchRead
	final     bool
	endedBy   string
}

// usedReadLine is the message of the log line of each read a step used.
const usedReadLine = "workflow: a step used a read of its agent's calls"

// logUsedRead writes the line of read once the fact's transaction commits. A count list the read
// holds none of is written as an empty list.
func (e *Engine) logUsedRead(ctx context.Context, read stepRead) {
	counts := read.read.Totals.Counts
	if counts == nil {
		counts = []shimwire.ModelCount{}
	}
	args := []any{"step", read.step, "issue", read.issue, "task", read.place.Task, "kind", read.place.Kind, "place", read.place.Place,
		"startedAt", read.startedAt.Format(time.RFC3339Nano), "side", read.side,
		"claim", string(read.key.Claim), "generation", read.key.Generation, "read", read.read.Totals.Read, "final", read.final,
		"setModel", read.read.Totals.SetModel, slog.Any("counts", counts), "outsideForm", read.read.Totals.OutsideForm,
		"overflowed", read.read.Totals.Overflowed, "readAt", read.read.At}
	if read.side == "end" {
		args = append(args, "endedBy", read.endedBy)
	}
	e.logOnCommit(ctx, usedReadLine, args...)
}
