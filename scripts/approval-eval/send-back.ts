import type { CaseVerdict } from "./build-corpus";
import { count, num, table } from "./corpus-io";
import {
  clopperPearsonInterval,
  effectiveSizeInterval,
  type Interval,
  kishEffectiveSize,
} from "./stats";

/**
 * The send-back figures the catalog (version 14) has the claim's registration hold beside item 8
 * ("Beside item 8" in the registered claim text): recall, precision, specificity and balanced
 * accuracy of a send-back, each with its exact 95 percent interval over the requests and its
 * interval over trees, and the always-approve baseline on the same requests. Two readers score
 * with them: `analyze.ts`, for each arm of a round over a draw that recorded the final test's
 * trees, and `s10-analysis.ts`, for the claim's one bundle, S10, over the claim's inventory. Each
 * hands in its requests with the verdict it decided or null, and prints the tables here under
 * words of its own that say what a request it did not decide was.
 */

/** The confidence of each interval the send-back figures carry, as the catalog asks: 95 percent. */
const FIGURE_CONFIDENCE = 0.95;

/**
 * The confidence of each of the two exact intervals balanced accuracy's interval is made of:
 * 1 - 0.05 / 2, by Bonferroni, so that both hold together at 95 percent at least.
 */
const BALANCED_PART_CONFIDENCE = 1 - (1 - FIGURE_CONFIDENCE) / 2;

/**
 * A share of requests with two two-sided 95 percent intervals, all null where nothing is counted.
 * `interval` is the exact Clopper-Pearson interval over the requests, as the registration names
 * it, which holds only where the requests are independent. `treeInterval` is the same share read
 * at `effectiveTotal`, Kish's effective size of the requests grouped by tree, in Korn and
 * Graubard's beta form: what the share is worth where every request of a tree goes one way.
 */
export interface ExactShare {
  count: number;
  total: number;
  share: number | null;
  interval: Interval | null;
  effectiveTotal: number | null;
  treeInterval: Interval | null;
}

/** Balanced accuracy, and its interval over the requests and over the trees. */
export interface BalancedAccuracy {
  value: number | null;
  interval: Interval | null;
  treeInterval: Interval | null;
}

/** One answered approval request as the send-back figures read it. */
export interface SendBackCase {
  /** The eligible tree the request is in. */
  tree: string;
  /** The person's verdict, read from the option they chose. */
  person: CaseVerdict;
  /**
   * The verdict the scored reviewer decided the request with (a round's arm at the claim's
   * threshold, or S10), and null where it decided none.
   */
  arm: CaseVerdict | null;
}

/**
 * The send-back figures the catalog (version 14) has the claim's registration hold beside item 8,
 * over the answered approval requests the caller hands in. A send-back is the person's chosen
 * option, request changes; any other option he chose, approve or another, is no send-back. The
 * reviewer sends a request back where it decided request changes, and does not where it decided
 * any other option. A request it did not decide is a wrong verdict, as the registration counts a
 * never-decide in every figure: a missed send-back where the person sent it back, and a false one
 * where he did not, so it is in precision's denominator too; no verdict is made up for it.
 * Recall, precision, specificity and agreement carry the two intervals `ExactShare` names.
 * Balanced accuracy is the mean of recall and specificity, and each of its intervals the mean of
 * theirs, each taken at 97.5 percent, so the two hold together at 95 percent at least, whatever
 * their dependence.
 */
export interface SendBackFigures {
  /** Where the send-back label comes from. */
  label: "chosen option";
  /** Every request handed in. */
  requests: number;
  /** Of those, the answers of an option that is neither approve nor request changes. */
  otherAnswers: number;
  /** The person sent the request back, and the reviewer decided request changes. */
  caught: number;
  /** The person sent the request back, and the reviewer decided another option or none. */
  missed: number;
  /** The person did not send the request back, and the reviewer decided another option. */
  cleared: number;
  /** The person did not send it back, and the reviewer decided request changes or none. */
  falseSendBacks: number;
  /** Of `missed` and of `falseSendBacks`, the requests the reviewer did not decide. */
  notDecided: { missed: number; falseSendBacks: number };
  recall: ExactShare;
  precision: ExactShare;
  specificity: ExactShare;
  balancedAccuracy: BalancedAccuracy;
  /** The caught and cleared requests over every request. */
  agreement: ExactShare;
  /**
   * A reviewer that approves every request, on the same requests: it sends none back, so its
   * recall is 0, its specificity 1, its balanced accuracy 0.5, and it has no precision; its
   * agreement is the share the person did not send back. Each is null where its class holds no
   * request.
   */
  alwaysApprove: {
    recall: number | null;
    specificity: number | null;
    balancedAccuracy: number | null;
    agreement: ExactShare;
  };
}

/** How many requests of one tree, or of every tree, fall in each cell of the send-back table. */
interface SendBackCells {
  caught: number;
  missed: number;
  cleared: number;
  falseSendBacks: number;
}

/** One cell of the send-back table. */
type SendBackCell = keyof SendBackCells;

/** Every cell, which is what agreement and the baseline's agreement are read over. */
const ALL_CELLS: readonly SendBackCell[] = ["caught", "missed", "cleared", "falseSendBacks"];

/** The requests of `cells` that fall in the named cells. */
function inCells(cells: SendBackCells, names: readonly SendBackCell[]): number {
  let sum = 0;
  for (const name of names) sum += cells[name];
  return sum;
}

/**
 * The share of the requests in the `over` cells that fall in the `hits` cells, which are among
 * them, with its exact interval over the requests and its interval at Kish's effective size of
 * those requests by tree (`ExactShare`), each at `confidence`; nulls where `over` holds nothing.
 */
function exactShare(
  trees: readonly SendBackCells[],
  hits: readonly SendBackCell[],
  over: readonly SendBackCell[],
  confidence: number = FIGURE_CONFIDENCE
): ExactShare {
  const sizes = trees.map((tree) => inCells(tree, over)).filter((size) => size > 0);
  let count = 0;
  for (const tree of trees) count += inCells(tree, hits);
  let total = 0;
  for (const size of sizes) total += size;
  if (total === 0) {
    return { count, total, share: null, interval: null, effectiveTotal: null, treeInterval: null };
  }
  const effectiveTotal = kishEffectiveSize(sizes);
  return {
    count,
    total,
    share: count / total,
    interval: clopperPearsonInterval(count, total, confidence),
    effectiveTotal,
    treeInterval: effectiveSizeInterval(count, total, effectiveTotal, confidence),
  };
}

/** The interval whose ends are the means of the ends of two intervals. */
function meanInterval(one: Interval | null, two: Interval | null): Interval | null {
  if (one === null || two === null) return null;
  return { lower: (one.lower + two.lower) / 2, upper: (one.upper + two.upper) / 2 };
}

/**
 * Balanced accuracy, the mean of recall and specificity, with the mean of their intervals each
 * taken at 97.5 percent, over the requests and over the trees; all null where either class holds
 * no request.
 */
function balancedAccuracyOf(trees: readonly SendBackCells[]): BalancedAccuracy {
  const recall = exactShare(trees, ["caught"], ["caught", "missed"], BALANCED_PART_CONFIDENCE);
  const specificity = exactShare(
    trees,
    ["cleared"],
    ["cleared", "falseSendBacks"],
    BALANCED_PART_CONFIDENCE
  );
  if (recall.share === null || specificity.share === null) {
    return { value: null, interval: null, treeInterval: null };
  }
  return {
    value: (recall.share + specificity.share) / 2,
    interval: meanInterval(recall.interval, specificity.interval),
    treeInterval: meanInterval(recall.treeInterval, specificity.treeInterval),
  };
}

/**
 * The send-back figures over answered approval requests (`SendBackFigures`): each request's
 * chosen option against the arm's decided verdict, a request the arm did not decide taking the
 * wrong class, each figure's requests grouped by their tree for the interval over trees.
 */
export function sendBackFiguresOf(cases: readonly SendBackCase[]): SendBackFigures {
  const byTree = new Map<string, SendBackCells>();
  let missedNotDecided = 0;
  let falseNotDecided = 0;
  let otherAnswers = 0;
  for (const one of cases) {
    if (one.person === "other") otherAnswers += 1;
    const sentBack = one.person === "request_changes";
    // A request the arm did not decide takes the wrong class, as the registration counts it.
    const armSendsBack = one.arm === null ? !sentBack : one.arm === "request_changes";
    if (one.arm === null && sentBack) missedNotDecided += 1;
    if (one.arm === null && !sentBack) falseNotDecided += 1;
    const cell: SendBackCell = sentBack
      ? armSendsBack
        ? "caught"
        : "missed"
      : armSendsBack
        ? "falseSendBacks"
        : "cleared";
    const cells = byTree.get(one.tree) ?? { caught: 0, missed: 0, cleared: 0, falseSendBacks: 0 };
    cells[cell] += 1;
    byTree.set(one.tree, cells);
  }
  const trees = [...byTree.values()];
  const sum = (names: readonly SendBackCell[]): number => {
    let total = 0;
    for (const tree of trees) total += inCells(tree, names);
    return total;
  };
  const sendBacks = sum(["caught", "missed"]);
  const notSentBack = sum(["cleared", "falseSendBacks"]);
  return {
    label: "chosen option",
    requests: cases.length,
    otherAnswers,
    caught: sum(["caught"]),
    missed: sum(["missed"]),
    cleared: sum(["cleared"]),
    falseSendBacks: sum(["falseSendBacks"]),
    notDecided: { missed: missedNotDecided, falseSendBacks: falseNotDecided },
    recall: exactShare(trees, ["caught"], ["caught", "missed"]),
    precision: exactShare(trees, ["caught"], ["caught", "falseSendBacks"]),
    specificity: exactShare(trees, ["cleared"], ["cleared", "falseSendBacks"]),
    balancedAccuracy: balancedAccuracyOf(trees),
    agreement: exactShare(trees, ["caught", "cleared"], ALL_CELLS),
    alwaysApprove: {
      recall: sendBacks === 0 ? null : 0,
      specificity: notSentBack === 0 ? null : 1,
      balancedAccuracy: sendBacks === 0 || notSentBack === 0 ? null : 0.5,
      agreement: exactShare(trees, ["cleared", "falseSendBacks"], ALL_CELLS),
    },
  };
}

/** An interval as a report prints it. */
function intervalText(interval: Interval): string {
  return `[${num(interval.lower)}, ${num(interval.upper)}]`;
}

/** A share with its exact interval and its counts, or the words that say nothing was counted. */
function exactShareText(value: ExactShare): string {
  if (value.share === null || value.interval === null) return "none (0 counted)";
  return (
    `${num(value.share)} ${intervalText(value.interval)} ` +
    `(${count(value.count)}/${count(value.total)})`
  );
}

/** A share's interval over trees, with the effective size it is read at. */
function treeIntervalText(value: ExactShare): string {
  if (value.treeInterval === null || value.effectiveTotal === null) return "none (0 counted)";
  return `${intervalText(value.treeInterval)} at ${num(value.effectiveTotal, 1)}`;
}

/**
 * The three tables of the send-back figures, one row a reviewer (`name`, under the column header
 * `first`): the figures with their exact intervals over the requests; the same figures' intervals
 * over trees; and the always-approve baseline on the same requests. The caller prints, above
 * them, the words that say what its requests are and what a request its reviewer did not decide
 * was.
 */
export function sendBackTables(
  first: string,
  rows: readonly { name: string; figures: SendBackFigures }[]
): string[] {
  const lines = table(
    [
      first,
      "requests",
      "other answers",
      "caught",
      "missed (not decided)",
      "cleared",
      "false send-backs (not decided)",
      "recall",
      "precision",
      "specificity",
      "balanced accuracy",
      "agreement",
    ],
    rows.map(({ name, figures }) => {
      const balanced = figures.balancedAccuracy;
      return [
        name,
        count(figures.requests),
        count(figures.otherAnswers),
        count(figures.caught),
        `${count(figures.missed)} (${count(figures.notDecided.missed)})`,
        count(figures.cleared),
        `${count(figures.falseSendBacks)} (${count(figures.notDecided.falseSendBacks)})`,
        exactShareText(figures.recall),
        exactShareText(figures.precision),
        exactShareText(figures.specificity),
        balanced.value === null || balanced.interval === null
          ? "none (a class holds no request)"
          : `${num(balanced.value)} ${intervalText(balanced.interval)}`,
        exactShareText(figures.agreement),
      ];
    })
  );
  lines.push(
    "",
    "The exact intervals hold only where the requests are independent, and the requests of one " +
      "tree may not be. The same figures read over trees: each share at Kish's effective size " +
      "of its requests grouped by tree (the number after each interval), in Korn and " +
      "Graubard's beta form, which is what the share is worth where every request of a tree " +
      "goes one way.",
    ""
  );
  lines.push(
    ...table(
      [first, "recall", "precision", "specificity", "balanced accuracy", "agreement"],
      rows.map(({ name, figures }) => {
        const balanced = figures.balancedAccuracy.treeInterval;
        return [
          name,
          treeIntervalText(figures.recall),
          treeIntervalText(figures.precision),
          treeIntervalText(figures.specificity),
          balanced === null ? "none (a class holds no request)" : intervalText(balanced),
          treeIntervalText(figures.agreement),
        ];
      })
    )
  );
  lines.push(
    "",
    "A reviewer that approves every request, on the same requests, sends none back: its recall " +
      "is 0, its specificity 1 and its balanced accuracy 0.5, and it has no precision. The " +
      "figures above are read against these.",
    ""
  );
  lines.push(
    ...table(
      [first, "always approve: recall", "specificity", "balanced accuracy", "agreement"],
      rows.map(({ name, figures }) => {
        const baseline = figures.alwaysApprove;
        return [
          name,
          baseline.recall === null ? "none (no send-back)" : num(baseline.recall),
          baseline.specificity === null ? "none (none not sent back)" : num(baseline.specificity),
          baseline.balancedAccuracy === null
            ? "none (a class holds no request)"
            : num(baseline.balancedAccuracy),
          exactShareText(baseline.agreement),
        ];
      })
    )
  );
  return lines;
}
