package evals

import (
	"embed"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// The seeded catalogue (dispatch://INFRA-2916/artifact/plan-md, "The seeded catalogue", the
// contract the evals' later editors read): every computed eval, which Ensure writes into each
// project's registry as version 1 (written_via seed), with its series and its section of the
// project's `evals-md`.
//
// An entry's definition is its code (R5): its source is `computed:` and the one exported function
// that computes its value, the fold the eval routes and the delivery routes all call
// (or, for unowned-p0, the function that counts it), and its number_rule is empty, since the
// function is the rule. The prose about the number is its section's method block, written from
// the code the source names: the question and the decision it changes, how it is measured, what
// it does not count and its failure modes, each with its guard (catalogueEntry.method).
//
// The series are those the day builders write (delivery_days.go, legion_days.go), every cut of a
// Legion series included, so every stored day row names a series of its eval. A listed series is
// one the list read folds; pr-ci's job series are not, and are read by the items route alone. A
// series is a headline one where a card of the eval states it, every cut of a Legion series
// included (R6: a chart draws the cut it needs, and no toggle picks one), and a control one where
// it qualifies another (change-fail-rate's upper bound, pr-ci's job series). An eval's primary
// series is the one its target judges, or the one its card leads with; an eval whose series are
// parts of equal standing (a bar per role, a count per class of filing) has none, and a target
// set on it names the series it judges.
//
// The seeded targets are main's (measures.DefaultTargets) where main sets one, and off where it
// sets none: "at least" is an inclusive target of a higher_better eval, "under" a strict one of a
// lower_better eval, and unowned-p0's "exactly 0" an inclusive 0 of a lower_better count.

// SeriesJobs is the one series row of pr-ci that defines every job series its day rows hold
// (`job:<name>`, measures.JobSeriesPrefix and the job's name): the unit, the fold, the listing and
// the min_n each job's series takes. A job's name is data, so no seeded row can name each job.
const SeriesJobs = measures.JobSeriesPrefix + "*"

// SeriesRow is the name of the eval_series row of slug's eval that defines the day rows stored
// under series: SeriesJobs for one of pr-ci's job series, and series itself for any other.
func SeriesRow(slug, series string) string {
	if slug == SlugPRCI && strings.HasPrefix(series, measures.JobSeriesPrefix) {
		return SeriesJobs
	}
	return series
}

// catalogueEntry is one computed eval of the catalogue: the fields its version 1 holds in every
// project (all but its id, its project, its doc block, its state and its writer, which Ensure
// sets), its series, and the body of its method block (methods/<slug>.md).
type catalogueEntry struct {
	definition Definition
	series     []Series
	body       string
}

// methods holds the body of each eval's method block, one file per slug: how the eval is
// measured, in words that name the functions its source and its builder are, and what it does
// not count.
//
//go:embed methods/*.md
var methods embed.FS

// methodBody is the embedded body of slug's method block. A slug with no file is a catalogue
// written without its prose, which this package's tests read at their start, so it panics.
func methodBody(slug string) string {
	text, err := methods.ReadFile("methods/" + slug + ".md")
	if err != nil {
		panic("evals: the catalogue eval " + slug + " has no method: " + err.Error())
	}
	return string(text)
}

// method is the text of the eval's method block: its question and the decision it changes, the
// embedded body, and its failure modes, each with its guard. The question, the decision and the
// failure modes are the definition's own fields, so the row and its section say the same.
func (e catalogueEntry) method() string {
	var text strings.Builder
	text.WriteString("**Question.** " + e.definition.Question + "\n\n")
	text.WriteString("**The decision it changes.** " + e.definition.Decision + "\n\n")
	text.WriteString(strings.TrimSpace(e.body) + "\n\n**Failure modes.**\n")
	for _, mode := range e.definition.FailureModes {
		text.WriteString("\n- " + mode.Mode + " Guard: " + mode.Guard)
	}
	return text.String() + "\n"
}

// headline is a listed headline series of unit, folded by agg.
func headline(name, unit, agg string) Series {
	return Series{Series: name, Unit: unit, Role: RoleHeadline, Listed: true, Agg: agg, Links: []Link{}}
}

// primary is series as the one its eval is judged by.
func primary(series Series) Series {
	series.IsPrimary = true
	return series
}

// control is series as one that qualifies another of its eval.
func control(series Series) Series {
	series.Role = RoleControl
	return series
}

// The cuts a Legion series is stored in (metrics.CutSeries): every series in all three, and the
// splitting eval's root figures, which count roots alone, in the all and roots cuts.
var (
	everyCut = []metrics.Scope{metrics.ScopeAll, metrics.ScopeRoots, metrics.ScopeChildren}
	rootCuts = []metrics.Scope{metrics.ScopeAll, metrics.ScopeRoots}
)

// inCuts is each of series in each of scopes, named as the cut stores it; only the all cut of a
// primary series is primary.
func inCuts(scopes []metrics.Scope, series ...Series) []Series {
	cut := make([]Series, 0, len(series)*len(scopes))
	for _, one := range series {
		for _, scope := range scopes {
			stored := one
			stored.Series = metrics.CutSeries(one.Series, scope)
			stored.IsPrimary = one.IsPrimary && scope == metrics.ScopeAll
			cut = append(cut, stored)
		}
	}
	return cut
}

// perRole is one headline series of hours for each of roles, every one folded as a quantile.
func perRole(roles ...string) []Series {
	series := make([]Series, len(roles))
	for i, role := range roles {
		series[i] = headline(role, "hours", AggQuantile)
	}
	return series
}

// The units the catalogue's evals and series are in.
const (
	unitMinutes = "minutes"
	unitHours   = "hours"
	unitRatio   = "ratio"
	unitIssues  = "issues"
)

// timing is the one series of a CI eval: the minutes of each timed result, with each outcome
// counted beside them.
var timing = primary(headline(measures.SeriesMinutes, unitMinutes, AggQuantile))

// catalogue is every computed eval, in the order a project's `evals-md` holds their sections.
var catalogue = []catalogueEntry{
	{
		definition: Definition{
			Slug:     SlugDeployFrequency,
			Name:     "Deploy frequency",
			Question: "How many deploys reach production each day?",
			Decision: "Whether to make changes smaller or the deploy pipeline faster, so that more changes reach production each day.",
			FailureModes: []FailureMode{
				{Mode: "A deploy that ships no change still counts as a deploy.", Guard: "`with_prs` counts the deploys that shipped a pull request, beside the total."},
				{Mode: "A deploy run that is still running has not reached production, so its day reads one deploy short.", Guard: "Dispatch builds the day again when the run concludes."},
				{Mode: "A deploy that ships pull requests of two projects counts once in each project, so the projects' counts add up to more than the deployment's count.", Guard: "The Delivery page counts each deploy once for the whole deployment."},
			},
			Unit: "deploys/day", Direction: DirectionHigherBetter,
			Target: new(20.0), TargetInclusive: true, TargetJudges: []string{"deploys.per_day"},
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldDeployFrequency",
		},
		series: []Series{primary(headline(measures.SeriesDeploys, "deploys", AggSum))},
	},
	{
		definition: Definition{
			Slug:     SlugChangeFailRate,
			Name:     "Change failure rate",
			Question: "What share of deploys broke production?",
			Decision: "Whether changes need more tests or review before they ship, because too many deploys break production.",
			FailureModes: []FailureMode{
				{Mode: "A failure that nobody flagged is not counted.", Guard: "No failure source exists yet, so the eval reads \"no data\" rather than a rate of 0."},
				{Mode: "A pending flag is not in the rate until the dora role confirms it.", Guard: "The `upper_bound` series is the rate if every pending flag were confirmed."},
				{Mode: "A deploy that shipped two broken pull requests could count twice.", Guard: "Each share counts a deploy once, whatever number of flags name it."},
			},
			Unit: unitRatio, Direction: DirectionLowerBetter,
			Target: new(0.05), TargetInclusive: false, TargetJudges: []string{"per_deploy.rate"},
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldChangeFailureRate",
		},
		series: []Series{
			primary(headline(measures.SeriesPerDeploy, unitRatio, AggRatio)),
			control(headline(measures.SeriesUpperBound, unitRatio, AggRatio)),
			headline(measures.SeriesPerPR, unitRatio, AggRatio),
		},
	},
	{
		definition: Definition{
			Slug:     SlugFailedDeployRecovery,
			Name:     "Failed deployment recovery time",
			Question: "How long does production stay broken after a deploy breaks it?",
			Decision: "Whether rollbacks and fixes must reach production faster.",
			FailureModes: []FailureMode{
				{Mode: "A failure that nobody flagged has no recovery time.", Guard: "No failure source exists yet, so the eval reads \"no data\" rather than a time."},
				{Mode: "A failure whose fix has not deployed is left out, so the newest failures are missing.", Guard: "The flag stays stored, and its time counts once the fix deploys."},
				{Mode: "A median hides one long outage among short ones.", Guard: "The day rows store every recovery time, not only the median."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldTimeToRestore",
		},
		series: []Series{primary(headline(measures.SeriesMinutes, unitMinutes, AggQuantile))},
	},
	{
		definition: Definition{
			Slug:     SlugMergeToProduction,
			Name:     "Merge to production",
			Question: "How long does a merged pull request take to reach production?",
			Decision: "Whether to make the deploy pipeline faster or to deploy more often.",
			FailureModes: []FailureMode{
				{Mode: "A pull request that has not deployed yet is left out, so a slow deploy hides its newest waits.", Guard: "Dispatch builds the merge day again when the run that ships the pull request lands."},
				{Mode: "One slow pull request sets the maximum for the whole window.", Guard: "The target judges the median beside the maximum."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Target: new(45.0), TargetInclusive: false, TargetJudges: []string{"minutes.max", "minutes.median"},
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldLeadTime",
		},
		series: []Series{primary(headline(measures.SeriesMinutes, unitMinutes, AggQuantile))},
	},
	{
		definition: Definition{
			Slug:     SlugFirstCommitToProduction,
			Name:     "First commit to production",
			Question: "How long does a change take from its first commit to production?",
			Decision: "Whether changes stay too long on their branches before they ship.",
			FailureModes: []FailureMode{
				{Mode: "The first commit's time is its author date, which the author's own git client records, so a wrong clock or an old commit moves it.", Guard: "One wrong time moves the median little."},
				{Mode: "A pull request whose first commit time Dispatch does not know is left out.", Guard: "The count `n` says how many pull requests the figures cover."},
				{Mode: "A pull request that has not deployed yet is left out.", Guard: "Dispatch builds the merge day again when the run that ships the pull request lands."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldLeadTime",
		},
		series: []Series{primary(headline(measures.SeriesMinutes, unitMinutes, AggQuantile))},
	},
	{
		definition: Definition{
			Slug:     SlugOpenToProduction,
			Name:     "PR opened to production",
			Question: "How long does a pull request take from its opening to production?",
			Decision: "Whether review or the deploy pipeline holds changes back from production.",
			FailureModes: []FailureMode{
				{Mode: "A pull request opened as a draft long before its review reads as a long wait.", Guard: "pr-open-to-merge and merge-to-production split the same time at the merge."},
				{Mode: "A pull request that has not deployed yet is left out.", Guard: "Dispatch builds the merge day again when the run that ships the pull request lands."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldLeadTime",
		},
		series: []Series{primary(headline(measures.SeriesMinutes, unitMinutes, AggQuantile))},
	},
	{
		definition: Definition{
			Slug:     SlugPROpenToMerge,
			Name:     "PR open to merge",
			Question: "How long is a pull request open before it merges?",
			Decision: "Whether reviews are too slow, or pull requests too large to review quickly.",
			FailureModes: []FailureMode{
				{Mode: "A pull request opened as a draft long before its review reads as a long wait.", Guard: "The target judges the median, which a few drafts move little."},
				{Mode: "A pull request that waits for days does not move the median.", Guard: "The 90th percentile shows the slow end."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Target: new(60.0), TargetInclusive: false, TargetJudges: []string{"minutes.median"},
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldLeadTime",
		},
		series: []Series{primary(headline(measures.SeriesMinutes, unitMinutes, AggQuantile))},
	},
	{
		definition: Definition{
			Slug:     SlugDeployRunSuccess,
			Name:     "Deploy runs reaching production",
			Question: "What share of deploy runs reach production?",
			Decision: "Whether to fix the flaky or failing steps of the deploy pipeline.",
			FailureModes: []FailureMode{
				{Mode: "A cancelled run is no deploy attempt and is left out, so many cancellations leave the share over few runs.", Guard: "The count of cancelled runs stands beside the share, and the count of concluded runs says how many runs it covers."},
				{Mode: "A run that is still running counts in neither part of the share.", Guard: "Dispatch builds the day again when the run concludes."},
			},
			Unit: unitRatio, Direction: DirectionHigherBetter,
			Target: new(0.9), TargetInclusive: true, TargetJudges: []string{"runs.rate"},
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldDeployRunSuccess",
		},
		series: []Series{primary(headline(measures.SeriesRuns, unitRatio, AggRatio))},
	},
	{
		definition: Definition{
			Slug:     SlugUnownedP0,
			Name:     "P0 issues with no owner",
			Question: "How many open P0 issues have no owner?",
			Decision: "Whether someone must take an urgent issue now.",
			FailureModes: []FailureMode{
				{Mode: "The count is a moment, so a P0 issue that had no owner for an hour inside a window does not show in the window's value.", Guard: "Each day's row keeps the count at that day's last refresh, so the days show when the count was above 0."},
				{Mode: "A window that ends in the past reads the count of its last day, not the count now.", Guard: "Today's row is written again at each refresh, so a window that ends today reads the newest count."},
			},
			Unit: unitIssues, Direction: DirectionLowerBetter,
			Target: new(0.0), TargetInclusive: true, TargetJudges: []string{"issues.value"},
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery.ListUnownedP0",
		},
		series: []Series{primary(headline(SeriesUnownedP0, unitIssues, AggLast))},
	},
	{
		definition: Definition{
			Slug:     SlugReworkShare,
			Name:     "Rework share",
			Question: "What share of merged pull requests fix or revert earlier work?",
			Decision: "Whether changes ship with too many defects, so that too much of the work is fixing earlier work.",
			FailureModes: []FailureMode{
				{Mode: "The rule reads titles alone, so a fix with another title is not counted, and a title such as \"Fix typo\" is.", Guard: "Each pull request's title is stored with it, so a reader can check what the rule matched."},
				{Mode: "A window with few merged pull requests gives a share of a small count.", Guard: "The day rows keep the count of pull requests beside the count of rework."},
			},
			Unit: unitRatio, Direction: DirectionLowerBetter,
			Kind: KindDelivery, Source: SourceComputedPrefix + "delivery/measures.FoldReworkShare",
		},
		series: []Series{primary(headline(measures.SeriesPRs, unitRatio, AggRatio))},
	},
	{
		definition: Definition{
			Slug:     SlugPRCI,
			Name:     "PR CI",
			Question: "How long do the pull-request checks take, and how often do they pass?",
			Decision: "Which check jobs to make faster or more reliable.",
			FailureModes: []FailureMode{
				{Mode: "A run's wall time counts the time it waited for a runner as well as the time it ran.", Guard: "The job series time each job alone, from its start to its completion."},
				{Mode: "A failure is often the pull request's own change rather than the pipeline.", Guard: "The Pipeline section lists the jobs that failed in the last 24 hours, each with its latest failed run."},
				{Mode: "A run whose jobs Dispatch has not stored counts in `minutes` and adds nothing to the job series.", Guard: "Each job series counts its own results, so its count shows how many runs it covers."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindCI, Source: SourceComputedPrefix + "delivery/measures.FoldTiming",
		},
		series: []Series{timing, func() Series {
			jobs := control(headline(SeriesJobs, unitMinutes, AggQuantile))
			jobs.Listed, jobs.MinN = false, new(int(measures.SlowestMinResults))
			return jobs
		}()},
	},
	{
		definition: Definition{
			Slug:     SlugStagingE2E,
			Name:     "Staging end-to-end gate",
			Question: "How long does the staging end-to-end gate take, and how often does it pass?",
			Decision: "Whether to make the end-to-end tests faster or more reliable.",
			FailureModes: []FailureMode{
				{Mode: "A gate job that a run skipped, or one that is still running, is not counted.", Guard: "Dispatch builds the day again when the job concludes."},
				{Mode: "The eval follows one job name from the delivery settings, so a renamed job stops being counted.", Guard: "A change of the settings rebuilds every day of the delivery and CI evals."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindCI, Source: SourceComputedPrefix + "delivery/measures.FoldTiming",
		},
		series: []Series{timing},
	},
	{
		definition: Definition{
			Slug:     SlugStagingDeploy,
			Name:     "Deploy to staging",
			Question: "How long does the staging deploy take, and how often does it succeed?",
			Decision: "Whether to make the staging deploy faster or more reliable.",
			FailureModes: []FailureMode{
				{Mode: "A staging deploy job that a run skipped, or one that is still running, is not counted.", Guard: "Dispatch builds the day again when the job concludes."},
				{Mode: "The eval follows one job name from the delivery settings, so a renamed job stops being counted.", Guard: "A change of the settings rebuilds every day of the delivery and CI evals."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindCI, Source: SourceComputedPrefix + "delivery/measures.FoldTiming",
		},
		series: []Series{timing},
	},
	{
		definition: Definition{
			Slug:     SlugProductionDeploy,
			Name:     "Deploy to production",
			Question: "How long does the production deploy job take, and how often does it succeed?",
			Decision: "Whether to make the production deploy faster or more reliable.",
			FailureModes: []FailureMode{
				{Mode: "A production job that succeeded and applied nothing could read as a fast success.", Guard: "The production rule of the delivery settings reads such a job as skipped, so it is not counted."},
				{Mode: "A production job that is still running is not counted.", Guard: "Dispatch builds the day again when the job concludes."},
			},
			Unit: unitMinutes, Direction: DirectionLowerBetter,
			Kind: KindCI, Source: SourceComputedPrefix + "delivery/measures.FoldTiming",
		},
		series: []Series{timing},
	},
	{
		definition: Definition{
			Slug:     SlugLegionWorkTime,
			Name:     "Work time to merge",
			Question: "How many hours of work does a Legion issue take, from its first worker event to its close?",
			Decision: "Whether to split Legion's issues smaller, or to remove what holds them up.",
			FailureModes: []FailureMode{
				{Mode: "A long wait for a person counts as work time.", Guard: "Phase time's segment of waiting on a person shows those hours."},
				{Mode: "An issue that closes again on a later day moves to its new close day.", Guard: "Dispatch records the day each issue was counted on, so it builds the old day and the new day again."},
				{Mode: "An issue with no worker event has no work time and is left out.", Guard: "The count `n` says how many issues the median covers."},
			},
			Unit: unitHours, Direction: DirectionLowerBetter,
			Kind: KindLegion, Source: SourceComputedPrefix + "legion/metrics.FoldWorkTime",
		},
		series: inCuts(everyCut, primary(headline(metrics.SeriesHours, unitHours, AggQuantile))),
	},
	{
		definition: Definition{
			Slug:     SlugLegionPhaseTime,
			Name:     "Phase time by worker type",
			Question: "Where does a finished issue's work time go, by worker type, and how much of it is idle?",
			Decision: "Which worker type's phase to make faster, and whether waits on people, CI or children hold the work up.",
			FailureModes: []FailureMode{
				{Mode: "The median per issue counts every finished issue, with a zero where the role did not work, so a bar that most issues skip reads a median of 0.", Guard: "The count `issues` says how many issues hold hours on each bar."},
				{Mode: "Each hour goes to one segment only, by a fixed order, so an hour in a turn while CI runs counts as in a turn.", Guard: "The code writes the order once, and every bar uses it."},
				{Mode: "The idle segment is the hours that no other segment claimed, which is not idle time by role.", Guard: "legion-idle-time reads the roles' own claim states."},
			},
			Unit: unitHours, Direction: DirectionLowerBetter,
			Kind: KindLegion, Source: SourceComputedPrefix + "legion/metrics.FoldPhaseTime",
		},
		series: inCuts(everyCut, perRole("architect", "planner", "implementer", "tester", "reviewer", "merger", "no_role")...),
	},
	{
		definition: Definition{
			Slug:     SlugLegionIdleTime,
			Name:     "Idle time by role",
			Question: "For each role, how many hours does an issue hold its slot in that role's phase while the role is not in a turn?",
			Decision: "Whether a role's sessions leave issues waiting between turns, so that the daemon must start their turns sooner.",
			FailureModes: []FailureMode{
				{Mode: "An hour in which a role waits for CI or for a person between its turns counts as idle.", Guard: "Phase time's segments show what those hours waited on."},
				{Mode: "A role with no claim observation counts as not working, so before the daemon records claim states every hour would read as idle.", Guard: "The eval reads \"no data\" for a window that starts before the project's first claim observation."},
			},
			Unit: unitHours, Direction: DirectionLowerBetter,
			Kind: KindLegion, Source: SourceComputedPrefix + "legion/metrics.FoldIdleTime",
		},
		series: inCuts(everyCut, perRole("architect", "planner", "implementer", "tester", "reviewer", "merger")...),
	},
	{
		definition: Definition{
			Slug:     SlugLegionSlotWait,
			Name:     "Slot wait",
			Question: "How many hours did a finished issue wait for a slot?",
			Decision: "Whether Legion needs more slots, or admits more issues than its slots can serve.",
			FailureModes: []FailureMode{
				{Mode: "An issue that the daemon never observed has no slot wait, so it reads 0 hours.", Guard: "The eval reads \"no data\" for a window that starts before the project's first slot observation."},
				{Mode: "Flow analysis version 1 measured slot wait by another rule, so its 39.3 percent does not compare with this value.", Guard: "The method states both rules and why the rule changed."},
			},
			Unit: unitHours, Direction: DirectionLowerBetter,
			Kind: KindLegion, Source: SourceComputedPrefix + "legion/metrics.FoldSlotWait",
		},
		series: inCuts(everyCut, primary(headline(metrics.SeriesHours, unitHours, AggQuantile))),
	},
	{
		definition: Definition{
			Slug:     SlugLegionRework,
			Name:     "Rework",
			Question: "How many review rounds does a finished issue take, and how often is it sent back to implementing?",
			Decision: "Whether plans and implementations need more care before review, so that fewer issues come back.",
			FailureModes: []FailureMode{
				{Mode: "Before the daemon's observations of an issue, its rounds and send-backs come from Dispatch's statuses, where `needs_review` is both reviewing and accepting.", Guard: "From the first observation on, the daemon's own phases replace the statuses."},
				{Mode: "A send-back for a red check counts as rework, although the change itself can be right.", Guard: "`hours_after_first_review` shows how much work the send-backs cost."},
			},
			Unit: "rounds", Direction: DirectionLowerBetter,
			Kind: KindLegion, Source: SourceComputedPrefix + "legion/metrics.FoldRework",
		},
		series: inCuts(everyCut,
			primary(headline(metrics.SeriesReviewRounds, "rounds", AggQuantile)),
			headline(metrics.SeriesSentBack, unitRatio, AggRatio),
			headline(metrics.SeriesHoursAfterFirstReview, unitHours, AggQuantile),
		),
	},
	{
		definition: Definition{
			Slug:     SlugLegionSplitting,
			Name:     "Splitting and follow-ups",
			Question: "How many issues do Legion sessions file, of which class, and how many children does a root have when it finishes?",
			Decision: "Whether Legion splits its work too much or too little at design, and how many follow-ups its work leaves behind.",
			FailureModes: []FailureMode{
				{Mode: "A filing's class reads the parent the issue had when it was filed, so a later move does not change its class.", Guard: "The cuts read the filed issue's tree as it stands now, so a moved issue counts in the tree it is in."},
				{Mode: "A filing into another project has no tree in this project and counts as a root.", Guard: "Its class still reads the filer's tree, so the filing counts as out of the tree."},
				{Mode: "More filings are neither better nor worse, so a target could reward the wrong thing.", Guard: "The eval has no direction and no target."},
			},
			Unit: unitIssues,
			Kind: KindLegion, Source: SourceComputedPrefix + "legion/metrics.FoldSplitting",
		},
		series: append(
			inCuts(everyCut,
				headline(metrics.FilingSeries(metrics.ClassInTreeAtDesign), unitIssues, AggSum),
				headline(metrics.FilingSeries(metrics.ClassInTreeLater), unitIssues, AggSum),
				headline(metrics.FilingSeries(metrics.ClassOutOfTree), unitIssues, AggSum),
				headline(metrics.FilingSeries(metrics.ClassByTheController), unitIssues, AggSum),
				headline(metrics.SeriesFinished, unitIssues, AggSum),
			),
			inCuts(rootCuts,
				headline(metrics.SeriesRoots, unitIssues, AggSum),
				headline(metrics.SeriesHoursWithChildren, unitHours, AggQuantile),
				headline(metrics.SeriesHoursWithoutChildren, unitHours, AggQuantile),
			)...,
		),
	},
}

// The bodies are read once, when the package starts, so a catalogue entry with no method file
// fails every test of the package and every binary that links it.
func init() {
	for i := range catalogue {
		catalogue[i].body = methodBody(catalogue[i].definition.Slug)
	}
}
