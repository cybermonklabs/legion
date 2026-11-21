import { count, num, table } from "./corpus-io";
import {
  type OutcomeTally,
  type PenalizedMeasure,
  type S10Analysis,
  statusWords,
} from "./s10-analysis";
import {
  COUNTING_RULE,
  JUDGED_FILE,
  RULE_VERSIONS,
  type RuleVersion,
  type ToolCallDeviation,
} from "./s10-judged-io";
import { sendBackTables } from "./send-back";

/**
 * The claim's analysis of S10 as it prints (`analyze.ts --claim-s10` without `--format json`):
 * every figure of its record (`s10-analysis.ts`), in the order the registered claim text reads
 * them, and nothing the record does not hold.
 */

/** A share and its count over a total, or the words that say nothing was counted. */
function shareText(part: number, whole: number): string {
  return whole === 0
    ? `${count(part)} of 0`
    : `${count(part)} of ${count(whole)} (${num(part / whole)})`;
}

/** The counts of a record, one `name count` a cell, or `none`. */
function countsText(counts: Record<string, number>): string {
  const entries = Object.entries(counts);
  return entries.length === 0
    ? "none"
    : entries.map(([name, times]) => `${name} ${count(times)}`).join(", ");
}

/** One version's measure as a row of the rule table. */
function ruleRow(
  version: RuleVersion,
  { tally, measure }: { tally: OutcomeTally; measure: PenalizedMeasure }
): string[] {
  return [
    version === COUNTING_RULE ? `${version} (counts)` : version,
    count(tally.decided.total),
    count(tally.neverDecides.total),
    count(tally.deviations.total),
    count(measure.n),
    count(measure.disagreeing),
    `at most ${num(measure.bound)}`,
  ];
}

/** The analysis as it prints: every figure of its record, in the order the claim text reads them. */
export function renderS10Analysis(analysis: S10Analysis): string[] {
  const { chain, inventory, tally, measure, decidedShare } = analysis;
  const lines = ["# The claim's analysis of S10, over its judged stage", ""];
  lines.push(
    "Points 4, 6, 7, 10 and 12 of the registered claim text, for its one bundle, S10, at " +
      `--claim-threshold ${num(analysis.threshold, 1)} (point 8): a verdict of five answers has ` +
      "at least three behind it, so every approve or request_changes is decided" +
      (analysis.leastDecidedShare === null
        ? "."
        : ` (the least share behind one here is ${num(analysis.leastDecidedShare, 1)}).`) +
      ` Version ${analysis.countingRule.slice(1)} of point 5's tool rule counts.`,
    "",
    "## The chain",
    "",
    `- claim run ${analysis.claimRun.id}, read under ${analysis.reference}, this run's digest ` +
      analysis.runDigest,
    `- build ${chain.build}: snapshot.json ${analysis.claimRun.snapshotSha256}, corpus.jsonl ` +
      `${analysis.claimRun.corpusSha256}, trees.json ${analysis.claimRun.treesSha256}`,
    `- draw ${chain.draw.directory}: draw.json ${chain.draw.drawSha256}, slices.json ` +
      chain.draw.slicesSha256,
    `- inputs ${chain.inputs.directory}: s10-inputs.json ${chain.inputs.stageSha256}, ` +
      `inventory.jsonl ${chain.inputs.inventorySha256}`,
    `- judged ${chain.judged.directory}: ${JUDGED_FILE} ${chain.judged.stageSha256}, ` +
      `${chain.judged.state}` +
      (chain.judged.stops.length === 0
        ? ""
        : `, after ${count(chain.judged.stops.length)} stops (the last: ` +
          `${chain.judged.stops[chain.judged.stops.length - 1]?.reason})`),
    `- log through event ${chain.log.throughEventId}, ${count(chain.log.eventsRead)} events, as ` +
      "many as the build read, walked to place his answers",
    "",
    "## The inventory (point 6)",
    "",
    `${count(inventory.requests)} answered approval requests of the draw's report trees, one an ` +
      `ask, from ${count(inventory.cases)} corpus cases: a case past an ask's first is its ` +
      `provenance, never a row. ${count(inventory.questionCases)} question cases on ` +
      `${count(inventory.questionAsks)} asks in the same trees are no approval request, and in ` +
      `no figure below. ${count(inventory.trees)} eligible trees, ` +
      `${count(measure.withoutRequest)} of them with no request.`,
    "",
    "## Each request's one outcome (points 6 and 7)",
    "",
    ...table(
      ["outcome", "requests", "by kind"],
      [
        [
          "decided",
          count(tally.decided.total),
          `approve ${count(tally.decided.approve)}, request_changes ` +
            count(tally.decided.requestChanges),
        ],
        ["never-decide", count(tally.neverDecides.total), countsText(tally.neverDecides.byVerdict)],
        ["deviation", count(tally.deviations.total), countsText(tally.deviations.byKind)],
      ]
    ),
    "",
    "A fact-check hold and a valid never_decide verdict are never-decides, not deviations. Of the " +
      `deviations, the analysis found ${countsText(tally.deviations.foundByAnalysis)}: ` +
      "transcript, answers and sandbox, a file of the request's kept evidence (a transcript, the " +
      "answers, the bundles, calls, sandbox record, started.json or a pending outcome) lost or " +
      "changed; answer, his own first qualifying answer of which the build holds no case (an " +
      "answer not his own, or none, is on a line the inputs stage made a deviation, and keeps " +
      "its inputs deviation, or unjudged where no run reached it); verdict, a recorded verdict " +
      "unlike point 2's rule recomputed from the five answers the stage kept; documents, his " +
      "answer's (document, version) pairs unlike the judged ones. started is a request a run " +
      "started and never finished; unjudged one no run reached.",
    "",
    "## Item 8's measure, penalized (points 10 and 12)",
    "",
    "Over the draw's eligible trees: every deviation is a decided request that disagrees with his " +
      "answer, whatever the confidence, so its tree disagrees. n is the trees with a decided " +
      "request or a deviation; a tree disagrees when any of them does; the trees with neither " +
      "are never-decide, apart and not in n, their share of every eligible tree and of those " +
      "holding a request. The penalized bound is the one-sided 90 percent Clopper-Pearson upper " +
      "bound on the disagreeing trees over n, and 1 where n is 0.",
    "",
    ...table(
      [
        "eligible trees",
        "no request",
        "n",
        "disagreeing",
        "never-decide",
        "of those with a request",
        "penalized bound",
        `at or under ${num(measure.rate, 2)}`,
      ],
      [
        [
          count(measure.trees),
          count(measure.withoutRequest),
          count(measure.n),
          count(measure.disagreeing),
          measure.neverDecide.share === null
            ? count(measure.neverDecide.count)
            : `${count(measure.neverDecide.count)} (${num(measure.neverDecide.share)})`,
          measure.neverDecide.withRequest.share === null
            ? count(measure.neverDecide.withRequest.count)
            : `${count(measure.neverDecide.withRequest.count)} ` +
              `(${num(measure.neverDecide.withRequest.share)})`,
          `at most ${num(measure.bound)}`,
          measure.withinRate ? "yes" : "no",
        ],
      ]
    ),
    "",
    "## S10's decided share (point 10)",
    "",
    `${shareText(decidedShare.decided, decidedShare.requests)}: the requests with a valid ` +
      "approve or request_changes over every request of the inventory, deviations in the " +
      `denominator and never in the numerator; against d = ${num(decidedShare.planningD, 2)}, a ` +
      "planning figure taken before S10 began (the daily count's point 5), not S10's measured " +
      "share.",
    "",
    "## Point 5's tool rules",
    "",
    `The same outcomes under each version of point 5's rule, version ` +
      `${analysis.countingRule.slice(1)}'s the one that counts, each with its penalized bound.`,
    ""
  );
  lines.push(
    ...table(
      ["rule", "decided", "never-decide", "deviations", "n", "disagreeing", "penalized bound"],
      RULE_VERSIONS.map((version) => ruleRow(version, analysis.byRule[version]))
    )
  );
  for (const change of analysis.ruleChanges) {
    lines.push(
      "",
      `Version ${change.to.slice(1)}'s rule changes the deviation status of ` +
        `${count(change.requests.length)} requests from version ${change.from.slice(1)}'s` +
        (change.requests.length === 0 ? "." : ":")
    );
    for (const one of change.requests) {
      const calls = (list: readonly ToolCallDeviation[]): string =>
        list.length === 0
          ? "none"
          : list.map((call) => `${call.judge} ${call.tools.join("+")} (${call.reason})`).join("; ");
      lines.push(
        `- ${one.askId}: ${one.before} to ${one.after}; refused only by version ` +
          `${change.from.slice(1)}: ${calls(one.refusedOnlyBefore)}; refused only by version ` +
          `${change.to.slice(1)}: ${calls(one.refusedOnlyAfter)}`
      );
    }
  }
  const neighbours = analysis.neighbours;
  const dropped = neighbours.droppedLaterEvent;
  lines.push(
    "",
    "## The neighbours' search (point 4)",
    "",
    `Over the ${count(neighbours.requests)} requests whose candidates the inputs stage counted: ` +
      `${count(neighbours.askHits)} ask hits, ${count(neighbours.candidates)} candidates, ` +
      `${count(neighbours.held)} held by the reserve (${count(neighbours.reserveErrors)} of them ` +
      `undecided), ${count(neighbours.kept)} kept, and ${count(dropped.total)} dropped for an ` +
      `event after the judged moment (seen in the log ${count(dropped.log)}, with no event ` +
      `through the pin ${count(dropped["no-event-through-pin"])}, after the pin ` +
      `${count(dropped["after-pin"])}), by drop scans through ` +
      `${neighbours.dropScanEnds.length === 0 ? "no event" : neighbours.dropScanEnds.join(", ")}. ` +
      neighbours.statement
  );
  lines.push(
    "",
    "### The send-back figures beside item 8",
    "",
    "For the catalog (version 14), over every request of the inventory, each once. A send-back " +
      "is his chosen option, request changes; any other option he chose is no send-back, and " +
      "the answers of an option that is neither approve nor request changes are counted in a " +
      "column of their own. S10 sends a request back where its decided verdict is request " +
      "changes. A never-decide or a deviation is a request S10 did not decide: no verdict is " +
      "made up for it, and each figure counts it as wrong, a missed send-back where he sent it " +
      "back and a false one where he did not; the not-decided counts in brackets say how many. " +
      "Each interval is two-sided at 95 percent and exact over the requests; balanced accuracy's " +
      "is the mean of recall's and specificity's intervals at 97.5 percent each.",
    "",
    ...sendBackTables("bundle", [{ name: "S10", figures: analysis.sendBack }]),
    "",
    "## Each request",
    "",
    "His answer is the first qualifying event after the ask's first opening (point 7): his " +
      "ask.answered, or for an ask of one document his artifact.approved or " +
      "artifact.changes_requested on it at the version the ask names.",
    "",
    ...table(
      ["ask", "tree", "his answer (event)", "outcome", "decided", "disagrees"],
      analysis.requests.map((reading) => {
        const one = reading.byRule[analysis.countingRule];
        const event =
          reading.answer.eventId === null
            ? "none"
            : `${reading.answer.eventId} ${reading.answer.type ?? ""}`.trim();
        return [
          reading.askId,
          reading.group,
          `${reading.answer.verdict} (${event})`,
          statusWords(one),
          one.status === "decided" ? "yes" : "no",
          one.disagrees === null ? "-" : one.disagrees ? "yes" : "no",
        ];
      })
    )
  );
  return lines;
}
