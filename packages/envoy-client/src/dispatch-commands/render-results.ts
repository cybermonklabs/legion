// What `dispatch results` prints of an issue's checks and their results (formatCheckResults), and
// the time format the attribution counts share (minuteUTC). The commands that fetch them are in
// issues.ts and session.ts; this module only renders what the server answered.
import type {
  BasisLearnedAt,
  CheckBasis,
  CheckBlock,
  CheckCI,
  CheckPassStands,
  CheckResult,
  HeadNotRead,
  IssueCheckResults,
} from "@legion/contracts";
import { newestResultsText } from "@legion/contracts";
import { actorText } from "../actor-text";

/** How many characters of a reading `--brief` keeps. R2 of the spec sets the number: a reader
 *  who wants the gate alone on an issue holding hundreds of results reads this much of a reading
 *  that stands at none of the compared commits, and the whole of every other one. */
const BRIEF_READING_CHARACTERS = 200;

/** The five reasons a check compared its results with nothing, and the four a result's commit
 *  cannot be placed, in words a reader acts on. A check takes all five and a result all but
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
  head_not_read: () => "Dispatch has not read the head of a pull request of this issue yet",
  issue_not_known: () =>
    "Dispatch does not know yet whether a pull request it read is this issue's",
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

/** Why GitHub refuses the read that is owed for a head, by the word the server gives. */
const REFUSAL_WORDS: Readonly<Record<NonNullable<HeadNotRead["refused_as"]>, string>> = {
  app_not_installed: "the GitHub App is not installed on the repository",
  repository_unreadable: "the GitHub App cannot read the repository",
  pull_request_not_found: "GitHub has no such pull request",
};

/** A time the server gives (RFC 3339), in UTC to the minute: `2026-11-17 08:09 UTC`. The minute
 *  is cut, not rounded. A value that is not a time prints as the server gave it. */
export function minuteUTC(at: string): string {
  const time = Date.parse(at);
  if (Number.isNaN(time)) return at;
  const iso = new Date(time).toISOString();
  return `${iso.slice(0, 10)} ${iso.slice(11, 16)} UTC`;
}

/** The line of one head that Dispatch has not read, and the line of GitHub's refusal of its read
 *  directly after it, where GitHub refuses that read. */
function headNotReadLines(head: HeadNotRead): string[] {
  const line = `  Not read yet: the head of pull request #${head.number}.`;
  if (head.refused_as === undefined) return [line];
  const since = head.refused_since === undefined ? "" : `, since ${minuteUTC(head.refused_since)}`;
  return [
    line,
    `  Not read: GitHub refuses the read of pull request #${head.number} (${REFUSAL_WORDS[head.refused_as]})${since}.`,
  ];
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
 *  commit. An answer a linked repository decided names it (`repo`). */
function commitStandsText(stands: CheckResult["commit_stands"]): string {
  if (stands === undefined) {
    return "Where that commit stands is not known: this Dispatch server does not say";
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
 *  are named either way, because they are the handles a reader reads the two wordings with. */
function checkWordsText(check: CheckBlock, result: CheckResult): string {
  const versions = `result version ${result.check_version}, check version ${check.version}`;
  if (result.check_words_moved === undefined) {
    return `Whether the check's words moved is not known (${versions})`;
  }
  return result.check_words_moved
    ? `The check's words moved after this result (${versions})`
    : `The check's words did not move after this result (${versions})`;
}

/** R5 for one check: Dispatch's answer to whether a pass of it stands, alone. A check CI proves
 *  names the roles of the passes after it (ciCheckLines); a recorded check's head line names the
 *  newest result of each role (recordedPassText). */
function passStandsText(pass: CheckPassStands | undefined): string {
  if (pass === undefined)
    return "Whether a pass stands is not known: this Dispatch server does not say";
  if (pass.answer === "yes") return "A pass stands: yes";
  if (pass.answer === "no") return "A pass stands: no";
  return "A pass stands: not known";
}

/** A recorded check's answer for the gate: Dispatch's answer, whether a pass stands, then the
 *  newest result of each role at what the check compared with, pass and fail alike, in the words
 *  the dashboard's Verification tab shows too (newestResultsText): `A pass stands: yes. Newest at
 *  that commit: implementer: pass (2120a467); tester: fail (9f5fd8c4, newer)`. With no result
 *  there, the answer stands alone. What counts as a pass that stands is Dispatch's answer, which
 *  this does not judge. */
function recordedPassText(check: CheckBlock): string {
  const answer = passStandsText(check.pass_stands);
  const newest = newestResultsText(check);
  return newest === undefined ? answer : `${answer}. ${newest}`;
}

/** Text a role or a person wrote (a reading, a retraction's reason, a check's words) on a line that
 *  starts at `indent`. Each of its lines after the first starts two spaces further in, and four
 *  spaces in at least. A check line starts `- ` and a result line `  - `, and such a text can hold
 *  either, as an e2e run's log quoted whole does: indented, none of its lines reads as one, and a
 *  blank line of it does not end the check's block, which a blank line does. A line ends at every
 *  break a reader may split at, not `\n` alone: `\r\n`, a lone `\r` (a terminal goes back to the
 *  line's start), and `\v`, `\f`, `\x1c` to `\x1e`, U+0085, U+2028 and U+2029 (Python's
 *  str.splitlines). Each is written as `\n`. */
function quoted(text: string, indent: string): string {
  const continued = `${indent}  `.length < 4 ? "    " : `${indent}  `;
  // biome-ignore lint/suspicious/noControlCharactersInRegex: \x1c to \x1e are line breaks to str.splitlines
  return text.replace(/\r\n|[\n\v\f\r\x1c-\x1e\u0085\u2028\u2029]/g, `\n${continued}`);
}

/** The reading one role recorded, under the line of its result at `indent`. `brief` cuts a reading
 *  whose result stands at none of the compared commits, and leaves every other reading whole: the
 *  reading is the record, and a reader who asked for the gate alone still reads the whole of what
 *  stands. Every line of the reading is indented (`quoted`). */
function readingLines(result: CheckResult, brief: boolean, indent = "    "): string[] {
  const observed = result.observed;
  if (observed === "") return [`${indent}The role recorded no reading.`];
  const cut = brief && result.commit_stands?.answer !== "at_basis";
  if (cut && observed.length > BRIEF_READING_CHARACTERS) {
    return [
      `${indent}Reading: ${quoted(observed.slice(0, BRIEF_READING_CHARACTERS), indent)}… (result ${result.id})`,
    ];
  }
  return [`${indent}Reading: ${quoted(observed, indent)}`];
}

/** One result, in the order the API served it: the verdict, the role, the whole commit, R4's two
 *  facts, the time, the id, the retraction that stands against it, and the reading. */
function resultLines(check: CheckBlock, result: CheckResult, brief: boolean): string[] {
  const lines = [
    `  - ${result.verdict} by ${result.role} at ${result.commit_sha}. ${commitStandsText(result.commit_stands)}. ${checkWordsText(check, result)}. Recorded ${result.created_at}. Result ${result.id}.`,
  ];
  const retracted = result.retracted;
  if (retracted !== undefined) {
    lines.push(
      `    Retracted by ${actorText(retracted.actor)} at ${retracted.created_at}. The reason: ${quoted(retracted.reason, "    ")}`
    );
  }
  return [...lines, ...readingLines(result, brief)];
}

/** A check CI proves, which Dispatch marks with `ci`: no role records its result, so the check and
 *  its one result read as one line. A derived result names its verdict, its commit, the producer
 *  `ci`, the run and the conclusion that decided it, and where that commit stands, as a recorded
 *  result's line says it; a check the run did not decide says why, in the sentence Dispatch wrote.
 *  The check's words end the line. Whether a pass stands follows, from the same `pass_stands` and
 *  in the same words as a recorded check's head line, so the gate reads both kinds alike, and
 *  where one stands the roles of its passes, `from ci`. The pull-request body line Dispatch wrote
 *  for the check follows, which the implementer copies, and then the run's reading: its
 *  conclusion, output and URL. */
function ciCheckLines(check: CheckBlock, ci: CheckCI, brief: boolean): string[] {
  const pass = check.pass_stands;
  const roles = pass?.answer === "yes" ? (pass.roles ?? []) : [];
  const passStandsLine = `  ${passStandsText(pass)}${roles.length === 0 ? "" : `, from ${roles.join(", ")}`}.`;
  const pullRequestLine = `  Pull request line: ${ci.line}`;
  const derived = check.results?.[0];
  if (derived === undefined) {
    return [
      `- ${check.block_id} (${check.when}): no result: ${ci.detail} ${quoted(check.text, "")}`,
      passStandsLine,
      pullRequestLine,
    ];
  }
  const stands = commitStandsText(derived.commit_stands);
  return [
    `- ${check.block_id} (${check.when}): ${derived.verdict} at ${derived.commit_sha} by ${derived.role} (CI run ${ci.run}, ${ci.case}), ${stands.charAt(0).toLowerCase()}${stands.slice(1)}. ${quoted(check.text, "")}`,
    passStandsLine,
    pullRequestLine,
    ...readingLines(derived, brief, "  "),
  ];
}

/** One check: a head line naming what it compared with, whether a pass of it stands and the newest
 *  result of each role at the compared commit (recordedPassText), a line for each pull request
 *  whose head Dispatch has not read and each one Dispatch does not know is this issue's, the
 *  check's own words once, then every result recorded against it. */
function checkLines(check: CheckBlock, brief: boolean): string[] {
  if (check.ci !== undefined) return ciCheckLines(check, check.ci, brief);
  const bases = check.compared_with ?? [];
  const reason = check.compared_nothing;
  const compared =
    bases.length > 0
      ? `Compared with ${bases.map(basisText).join(", ")}`
      : reason === undefined
        ? "Compared with nothing, and this Dispatch server does not say why"
        : `Compared with nothing: ${reasonText(reason)}`;
  const results = check.results ?? [];
  return [
    `- ${check.block_id} (${check.when}). ${compared}. ${recordedPassText(check)}.`,
    ...(check.heads_not_read ?? []).flatMap(headNotReadLines),
    ...(check.issues_not_known ?? []).map(
      (entry) => `  Not known yet: whether pull request #${entry.number} is this issue's.`
    ),
    `  The check's words: ${quoted(check.text, "  ")}`,
    ...(results.length === 0
      ? ["  No result is recorded for this check."]
      : results.flatMap((result) => resultLines(check, result, brief))),
  ];
}

/** The links of the issue that Dispatch reads no more (`unreadable_links`), one line each: GitHub
 *  refused the read of each because the GitHub App is not installed on its repository, and Dispatch
 *  reads it again only once the issue links it again, an event of its pull request arrives, or the
 *  App is installed on that repository. No check's answer turns on them. */
function unreadableLinkLines(results: IssueCheckResults): string[] {
  const links = results.unreadable_links ?? [];
  if (links.length === 0) return [];
  return [
    "Links Dispatch reads no more until the issue links them again, an event of the pull request arrives, or the GitHub App is installed on their repository:",
    ...links.map((link) => {
      const since =
        link.refused_since === undefined ? "" : `, since ${minuteUTC(link.refused_since)}`;
      return `- ${link.url}: GitHub refuses its read (${REFUSAL_WORDS.app_not_installed})${since}.`;
    }),
  ];
}

/** What `dispatch results` prints: each check of the issue's Verification document, what Dispatch
 *  compared its results with, whether a pass of it stands, its own words once, and every result
 *  recorded against it in the order the API serves them. A check CI proves prints its derived
 *  result, or why it has none, as one line, then whether a pass of it stands (`ciCheckLines`). A
 *  check with no result says so, and an issue with no Verification document says that instead of
 *  printing an empty list: Dispatch answers `verification_artifact_id` "" for such an issue, never
 *  null, so that is what is compared. The links Dispatch reads no more end the text, whatever the
 *  checks are (`unreadableLinkLines`). */
export function formatCheckResults(results: IssueCheckResults, brief = false): string {
  const unreadable = unreadableLinkLines(results);
  const withLinks = (text: string): string =>
    unreadable.length === 0 ? text : [text, unreadable.join("\n")].join("\n\n");
  if (results.verification_artifact_id === "") {
    return withLinks(
      `${results.issue} has no Verification document, so it has no checks. Write one (verification.md) with a check block per thing that must be proved.`
    );
  }
  const checks = results.checks ?? [];
  if (checks.length === 0) {
    return withLinks(`${results.issue}'s Verification document holds no check blocks.`);
  }
  const blocks = checks.map((check) => checkLines(check, brief).join("\n"));
  return withLinks([`Checks of ${results.issue}:`, ...blocks].join("\n\n"));
}
