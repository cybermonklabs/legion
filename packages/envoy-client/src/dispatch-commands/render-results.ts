import type {
  BasisLearnedAt,
  CheckBasis,
  CheckBlock,
  CheckCI,
  CheckPassStands,
  CheckResult,
  IssueCheckResults,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import { leftOutAt, orLeftOut } from "./as-of";

/** How many characters of a reading `--brief` keeps. R2 of the spec sets the number: a reader
 *  who wants the gate alone on an issue holding hundreds of results reads this much of a reading
 *  that stands at none of the compared commits, and the whole of every other one. */
const BRIEF_READING_CHARACTERS = 200;

/** The three reasons a check compared its results with nothing, and the two a result's commit
 *  cannot be placed, in words a reader acts on. A check takes all three and a result all but
 *  no_pull_request_or_branch, so the check's set types this map and answers both. Each takes the
 *  repository the answer is about where the server names one (`repo`): a result whose commit is
 *  taken to be of a repository the issue links a pull request in is decided by that repository,
 *  and "the repository" would read as the issue's own. Keyed by `compared_nothing`'s reasons, so a
 *  reason the contract adds to that union is a type error until it has words here; one it adds to
 *  the result's `commit_stands.reason` alone is not, and `reasonText` prints it by its code, as it
 *  prints any reason this build has no words for. */
const COMPARISON_REASONS: Readonly<
  Record<NonNullable<CheckBlock["compared_nothing"]>, (repo: string | undefined) => string>
> = {
  no_pull_request_or_branch: () => "the issue has no pull request and no branch to compare with",
  repository_unreadable: (repo = "the repository") => `Dispatch cannot read ${repo}`,
  read_failed: (repo) =>
    repo === undefined
      ? "a read Dispatch made did not answer"
      : `a read Dispatch made of ${repo} did not answer`,
};

/** Whether a reason the server sent is one this build words. The server gives the reasons its
 *  own build knows, so a later server can send one this build has no words for. */
function isComparisonReason(reason: string): reason is keyof typeof COMPARISON_REASONS {
  return Object.hasOwn(COMPARISON_REASONS, reason);
}

/** `reason` in words, naming `repo` where the server names the repository the answer is about.
 *  A reason this build has no words for is printed as the server gave it, with that repository
 *  beside it in parentheses, so the answer still says which repository it is about. */
function reasonText(reason: string, repo?: string): string {
  if (isComparisonReason(reason)) return COMPARISON_REASONS[reason](repo);
  return repo === undefined ? reason : `${reason} (${repo})`;
}

/** When Dispatch learned a pull request's head, and which time that is: `read_at` is Dispatch's
 *  own read of the pull request, `updated_at` is GitHub's update time as a webhook carried it. */
function learnedText(learned: BasisLearnedAt): string {
  if (learned.kind === "read_at") return `Dispatch read it ${learned.at}`;
  return `GitHub updated it ${learned.at}`;
}

/** A pull request as a reader tells it apart: `owner/repo#7`, GitHub's own name for it, for one
 *  of a repository the issue links a pull request in and does not resolve to, which the server
 *  names in `repo`, and `pull request #7` for one of the issue's own repository. */
function pullRequestName(basis: CheckBasis, number: number): string {
  return basis.repo === undefined ? `pull request #${number}` : `${basis.repo}#${number}`;
}

/** A branch as a reader tells it apart: `main of owner/repo` for a branch of a linked repository,
 *  and `main` for one of the issue's own repository. */
function branchName(basis: CheckBasis): string {
  const branch = basis.branch ?? "unnamed";
  return basis.repo === undefined ? branch : `${branch} of ${basis.repo}`;
}

/** One thing a check compared its results with, as the head line names it. */
function basisText(basis: CheckBasis): string {
  const learned = basis.learned_at === undefined ? "" : ` (${learnedText(basis.learned_at)})`;
  if (basis.kind === "pull_request") {
    const pullRequest =
      basis.pull_request === undefined
        ? "pull request "
        : `${pullRequestName(basis, basis.pull_request)} `;
    return `the head of ${pullRequest}at ${basis.commit ?? "an unnamed commit"}${learned}`;
  }
  if (basis.kind === "branch") return `branch ${branchName(basis)}${learned}`;
  return `the commit you named, ${basis.commit ?? ""}${learned}`;
}

/** Where a result's commit stands, as the basis it matched names that place. The head line has
 *  already printed each basis whole, so this names it short. */
function basisPlace(basis: CheckBasis): string {
  if (basis.kind === "pull_request") {
    return basis.pull_request === undefined
      ? "the head of a pull request"
      : `the head of ${pullRequestName(basis, basis.pull_request)}`;
  }
  if (basis.kind === "branch") return `on branch ${branchName(basis)}`;
  return "the commit you named";
}

/** The first of R4's two facts: where one result's commit stands against what its check compared
 *  with. A Dispatch server that does not answer it says so rather than reading as another
 *  commit. An answer a linked repository decided names it (`repo`). At a cut (`asOf`) it is left
 *  out: it is read from GitHub now. */
function commitStandsText(stands: CheckResult["commit_stands"], asOf: string | undefined): string {
  if (stands === undefined) {
    return asOf === undefined
      ? "Where that commit stands is not known: this Dispatch server does not say"
      : `Where that commit stands is ${leftOutAt(asOf)}`;
  }
  if (stands.answer === "at_basis") {
    return stands.basis === undefined
      ? "That commit is one of the compared commits"
      : `That commit is ${basisPlace(stands.basis)}`;
  }
  if (stands.answer === "other_commit") {
    return stands.repo === undefined
      ? "That commit is none of the compared commits"
      : `That commit, of ${stands.repo}, is none of the compared commits`;
  }
  // The reason is the server's to give and may be absent, so the answer stands on its own
  // where there is none.
  if (stands.reason === undefined) {
    return stands.repo === undefined
      ? "Where that commit stands is not known"
      : `Where that commit of ${stands.repo} stands is not known`;
  }
  return `Where that commit stands is not known: ${reasonText(stands.reason, stands.repo)}`;
}

/** R4's other fact: whether the check's words moved after the result was recorded. Both versions
 *  are named either way, because they are the handles a reader reads the two wordings with. At a
 *  cut (`asOf`) the fact and the check's version are left out where no index holds the version. */
function checkWordsText(check: CheckBlock, result: CheckResult, asOf: string | undefined): string {
  const version: number | undefined = check.version;
  const versions = `result version ${result.check_version}, check version ${orLeftOut(version, asOf)}`;
  if (result.check_words_moved === undefined) {
    return asOf === undefined
      ? `Whether the check's words moved is not known (${versions})`
      : `Whether the check's words moved is ${leftOutAt(asOf)} (${versions})`;
  }
  return result.check_words_moved
    ? `The check's words moved after this result (${versions})`
    : `The check's words did not move after this result (${versions})`;
}

/** R5 for one check: whether a pass of it stands, and from which roles. At a cut (`asOf`) it is
 *  left out: it is read from GitHub now. */
function passStandsText(pass: CheckPassStands | undefined, asOf: string | undefined): string {
  if (pass === undefined) {
    return asOf === undefined
      ? "Whether a pass stands is not known: this Dispatch server does not say"
      : `Whether a pass stands is ${leftOutAt(asOf)}`;
  }
  if (pass.answer === "yes") {
    const roles = pass.roles ?? [];
    return roles.length === 0
      ? "A pass stands: yes"
      : `A pass stands: yes, from ${roles.join(", ")}`;
  }
  if (pass.answer === "no") return "A pass stands: no";
  return "A pass stands: not known";
}

/** The reading one role recorded, under the line of its result at `indent`. `brief` cuts a reading
 *  whose result stands at none of the compared commits, and leaves every other reading whole: the
 *  reading is the record, and a reader who asked for the gate alone still reads the whole of what
 *  stands. */
function readingLines(result: CheckResult, brief: boolean, indent = "    "): string[] {
  const observed = result.observed;
  if (observed === "") return [`${indent}The role recorded no reading.`];
  const cut = brief && result.commit_stands?.answer !== "at_basis";
  if (cut && observed.length > BRIEF_READING_CHARACTERS) {
    return [
      `${indent}Reading: ${observed.slice(0, BRIEF_READING_CHARACTERS)}… (result ${result.id})`,
    ];
  }
  return [`${indent}Reading: ${observed}`];
}

/** One result, in the order the API served it: the verdict, the role, the whole commit, R4's two
 *  facts, the time, the id, the retraction that stands against it, and the reading. */
function resultLines(
  check: CheckBlock,
  result: CheckResult,
  brief: boolean,
  asOf: string | undefined
): string[] {
  const lines = [
    `  - ${result.verdict} by ${result.role} at ${result.commit_sha}. ${commitStandsText(result.commit_stands, asOf)}. ${checkWordsText(check, result, asOf)}. Recorded ${result.created_at}. Result ${result.id}.`,
  ];
  const retracted = result.retracted;
  if (retracted !== undefined) {
    lines.push(
      `    Retracted by ${actorText(retracted.actor)} at ${retracted.created_at}. The reason: ${retracted.reason}`
    );
  }
  return [...lines, ...readingLines(result, brief)];
}

/** A check CI proves, which Dispatch marks with `ci`: no role records its result, so the check and
 *  its one result read as one line. A derived result names its verdict, its commit, the producer
 *  `ci`, the run and the conclusion that decided it, and where that commit stands, as a recorded
 *  result's line says it; a check the run did not decide says why, in the sentence Dispatch wrote.
 *  The check's words end the line. Whether a pass stands follows, from the same `pass_stands` and
 *  in the same words as a recorded check's head line, so the gate reads both kinds alike. The
 *  pull-request body line Dispatch wrote for the check follows, which the implementer copies, and
 *  then the run's reading: its conclusion, output and URL. */
function ciCheckLines(check: CheckBlock, ci: CheckCI, brief: boolean): string[] {
  const passStandsLine = `  ${passStandsText(check.pass_stands, undefined)}.`;
  const pullRequestLine = `  Pull request line: ${ci.line}`;
  const derived = check.results?.[0];
  if (derived === undefined) {
    return [
      `- ${check.block_id} (${check.when}): no result: ${ci.detail} ${check.text}`,
      passStandsLine,
      pullRequestLine,
    ];
  }
  const stands = commitStandsText(derived.commit_stands, undefined);
  return [
    `- ${check.block_id} (${check.when}): ${derived.verdict} at ${derived.commit_sha} by ${derived.role} (CI run ${ci.run}, ${ci.case}), ${stands.charAt(0).toLowerCase()}${stands.slice(1)}. ${check.text}`,
    passStandsLine,
    pullRequestLine,
    ...readingLines(derived, brief, "  "),
  ];
}

/** What a check's head line says it compared its results with: each basis, or why it compared
 *  them with nothing. At a cut (`asOf`) both are left out: they are read from GitHub now. */
function comparedText(check: CheckBlock, asOf: string | undefined): string {
  const bases = check.compared_with ?? [];
  if (bases.length > 0) return `Compared with ${bases.map(basisText).join(", ")}`;
  const reason = check.compared_nothing;
  if (asOf !== undefined && check.compared_with === undefined && reason === undefined) {
    return `What it is compared with is ${leftOutAt(asOf)}`;
  }
  if (reason === undefined)
    return "Compared with nothing, and this Dispatch server does not say why";
  return `Compared with nothing: ${reasonText(reason)}`;
}

/** One check: a head line naming what it compared with and whether a pass of it stands, the
 *  check's own words once, then every result recorded against it. A read at a cut (`asOf`)
 *  reads nothing from GitHub, so a check CI proves reads as any other, its derived result left
 *  out. */
function checkLines(check: CheckBlock, brief: boolean, asOf: string | undefined): string[] {
  if (check.ci !== undefined) return ciCheckLines(check, check.ci, brief);
  const compared = comparedText(check, asOf);
  const results = check.results ?? [];
  return [
    `- ${check.block_id} (${check.when}). ${compared}. ${passStandsText(check.pass_stands, asOf)}.`,
    `  The check's words: ${check.text}`,
    ...(results.length === 0
      ? ["  No result is recorded for this check."]
      : results.flatMap((result) => resultLines(check, result, brief, asOf))),
  ];
}

/** What `dispatch results` prints: each check of the issue's Verification document, what Dispatch
 *  compared its results with, whether a pass of it stands, its own words once, and every result
 *  recorded against it in the order the API serves them. A check CI proves prints its derived
 *  result, or why it has none, as one line, then whether a pass of it stands (`ciCheckLines`). A
 *  check with no result says so, and an issue with no Verification document says that instead of
 *  printing an empty list: Dispatch answers `verification_artifact_id` "" for such an issue, never
 *  null, so that is what is compared. At a cut (`asOf`) what GitHub says now is left out. */
export function formatCheckResults(
  results: IssueCheckResults,
  brief = false,
  asOf?: string
): string {
  if (results.verification_artifact_id === "") {
    return `${results.issue} has no Verification document, so it has no checks. Write one (verification.md) with a check block per thing that must be proved.`;
  }
  const checks = results.checks ?? [];
  if (checks.length === 0) {
    return `${results.issue}'s Verification document holds no check blocks.`;
  }
  const blocks = checks.map((check) => checkLines(check, brief, asOf).join("\n"));
  return [`Checks of ${results.issue}:`, ...blocks].join("\n\n");
}
