import type {
  BasisLearnedAt,
  CheckBasis,
  CheckBlock,
  CheckCI,
  CheckPassStands,
  CheckResult,
  IssueCheckResults,
  PassStandsSubject,
} from "@legion/contracts";
import {
  checkWithNewerFields,
  mergedPullRequestsWords,
  subjectPlace,
  subjectStateWords,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import { printableStrings } from "../runner-log";

// check-results.ts is what `dispatch results --issue` prints: each check of an issue's
// Verification document, what Dispatch compared its results with, whether a pass of it stands,
// and every result recorded against it (formatCheckResults).

/** How many characters of a reading `--brief` keeps. R2 of the spec sets the number: a reader
 *  who wants the gate alone on an issue holding hundreds of results reads this much of a reading
 *  that stands at none of the compared commits, and the whole of every other one. */
const BRIEF_READING_CHARACTERS = 200;

/** The reasons a check compared its results with nothing or a result's commit cannot be placed,
 *  in words a reader acts on. The check's set types this map and answers both. Each takes the
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
  not_merged: () => "the work has no merge that Dispatch knows",
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
 *  of another repository than the one the issue resolves to, which the server names in `repo`,
 *  and `pull request #7` for one of the issue's own repository. */
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
  if (basis.kind === "branch") {
    // A branch an after_merge check reads at its work's merge names that merge, and the repository
    // it was read in where the server names one.
    if (basis.merge_commit === undefined) return `branch ${branchName(basis)}${learned}`;
    const repository = basis.repo === undefined ? "" : `in ${basis.repo} `;
    return `${repository}branch ${basis.branch ?? "unnamed"}, merged at ${basis.merge_commit}${learned}`;
  }
  if (basis.kind === "branch_head") {
    return `the head of branch ${basis.branch ?? "unnamed"} at ${basis.commit ?? "an unnamed commit"}`;
  }
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
  if (basis.kind === "branch_head") return `at the head of branch ${basis.branch ?? "unnamed"}`;
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

/** `<k> of <n> heads read yes` for the subjects of a check. A head is a commit of one repository,
 *  and its state belongs to that commit (R3), so pull requests whose heads are one commit are one
 *  head however many of them there are, and one commit in two repositories is two heads (R18): the
 *  basis names its repository where it is another than the issue's. A head reads yes where each
 *  subject at it does. A subject that names no commit is a head of its own, since nothing else can
 *  be said to be at it. */
function headsReadYes(subjects: readonly PassStandsSubject[]): string {
  const heads = new Map<string, boolean>();
  subjects.forEach((subject, index) => {
    const { commit, repo } = subject.basis;
    const head = commit === undefined ? `subject ${index}` : `${repo ?? ""} ${commit}`;
    heads.set(head, (heads.get(head) ?? true) && subject.answer === "yes");
  });
  const yes = [...heads.values()].filter((reads) => reads).length;
  return `${yes} of ${heads.size} heads read yes`;
}

/** Whether `check` is compared with a branch: an after_merge or default_branch check read with no
 *  commit named (`at`), whose answer names the fact that decides it (`branchPassStandsText`). */
function comparedWithBranch(check: CheckBlock): boolean {
  return (
    (check.when === "after_merge" || check.when === "default_branch") &&
    !(check.compared_with ?? []).some((basis) => basis.kind === "named")
  );
}

/** R5 for a check compared with a branch, by the reason Dispatch gives: the newest result that
 *  counts at a commit holding the work's merge decides, and the words name it and its commit, or
 *  the pass and the fail that both count there, the result Dispatch could not place, the failed
 *  read, or, for a check of a running program, the deploy it reads. `repository` is the check
 *  owner's repository, which a basis omits. Undefined where the answer gives no reason this build
 *  words, or names no result the reason needs, so `passStandsText` words it as it words any other
 *  check. */
function branchPassStandsText(
  check: CheckBlock,
  pass: CheckPassStands,
  yes: string,
  repository: string | undefined
): string | undefined {
  const results = check.results ?? [];
  const deciding = pass.results ?? [];
  const passed = results.find(
    (result) => result.verdict === "pass" && deciding.includes(result.id)
  );
  const failed = results.find(
    (result) => result.verdict === "fail" && deciding.includes(result.id)
  );
  const result = results.find((result) => deciding.includes(result.id));
  const repo = repository ?? check.compared_with?.[0]?.repo ?? "the repository";
  const deploy = pass.deploy;
  const program = deploy?.program ?? check.program ?? "the program";
  if (pass.answer === "yes" && passed !== undefined) {
    return `${yes}, as the newest result is pass ${passed.id} at ${pass.commit ?? passed.commit_sha}`;
  }
  switch (pass.reason) {
    case "fail":
      return failed === undefined
        ? undefined
        : `A pass stands: no, as the newest result is fail ${failed.id} at ${pass.commit ?? failed.commit_sha}`;
    case "no_result":
      return "A pass stands: no, as no result counts at a commit that holds the merge";
    case "pass_and_fail":
      return passed === undefined || failed === undefined
        ? undefined
        : `A pass stands: not known, as pass ${passed.id} and fail ${failed.id} both count at ${pass.commit ?? passed.commit_sha}. A retraction of the wrong one ends it`;
    case "not_placed":
      return result === undefined
        ? undefined
        : `A pass stands: not known, as Dispatch could not place ${result.verdict} ${result.id} at ${pass.commit ?? result.commit_sha}`;
    case "read_failed":
      return "A pass stands: not known, as a read Dispatch made did not answer";
    case "program_not_named":
      return `A pass stands: not known, as ${repo} runs more than one program and this check names none`;
    case "no_deploy":
      return `A pass stands: not known, as Dispatch holds no deploy of ${program} in ${repo}`;
    case "deploy_not_merged":
      return deploy === undefined
        ? undefined
        : `A pass stands: not known, as ${program} runs ${deploy.commit} since ${deploy.deployed_at}, which does not hold the merge`;
    case "no_result_from_deploy":
      return deploy === undefined
        ? undefined
        : `A pass stands: not known, as no result counts for ${program} at ${deploy.commit}, deployed ${deploy.deployed_at}`;
    default:
      return undefined;
  }
}

/** R5 and R12 for one check: whether a pass of it stands, and from which roles. A check with
 *  subjects, the heads a pass must stand at, says how many of the heads read yes where the answer
 *  is not yes (`headsReadYes`); each subject's own line follows (`subjectLines`). A check compared
 *  with a branch names the fact that decides it (`branchPassStandsText`). Any other answer that
 *  names the result that decided it, such as a branch check read at a commit you named, names that
 *  result, or the one Dispatch could not place, at its commit. A check whose answer names no
 *  `pass_stands` gets no such words at all (`checkLines`). */
function passStandsText(check: CheckBlock, pass: CheckPassStands, repository?: string): string {
  const roles = pass.roles ?? [];
  const yes =
    roles.length === 0 ? "A pass stands: yes" : `A pass stands: yes, from ${roles.join(", ")}`;
  const subjects = pass.subjects ?? [];
  if (subjects.length > 0) {
    if (pass.answer === "yes") return yes;
    const read = headsReadYes(subjects);
    return pass.answer === "no"
      ? `A pass stands: no, as ${read}`
      : `A pass stands: not known, as ${read}`;
  }
  if (comparedWithBranch(check)) {
    const branch = branchPassStandsText(check, pass, yes, repository);
    if (branch !== undefined) return branch;
  }
  const decidedID = pass.results?.[0];
  const decided = (check.results ?? []).find((result) => result.id === decidedID);
  if (decided !== undefined && pass.reason !== "pass_and_fail") {
    const named = `${decided.verdict} ${decided.id} at ${pass.commit ?? decided.commit_sha}`;
    if (pass.answer === "unknown") {
      return `A pass stands: not known, as Dispatch could not place ${named}`;
    }
    return `${pass.answer === "yes" ? yes : "A pass stands: no"}, as the newest result is ${named}`;
  }
  if (pass.answer === "yes") return yes;
  if (pass.answer === "no") return "A pass stands: no";
  return pass.reason === "pass_and_fail"
    ? "A pass stands: not known, as a pass and a fail count at one commit"
    : "A pass stands: not known";
}

/** R9 to R11 under a check's head line: the line of each head a pass of the check must stand at,
 *  `  - <place> at <commit>: <state>` in the words the Verification tab shows too
 *  (`subjectStateWords`), each followed, on a check CI proves, by the pull-request body line of
 *  that head; then, where an open pull request's head is one, the line that counts the merged pull
 *  requests the check compared with, which decide nothing while one is open and get no line of
 *  their own. */
function subjectLines(check: CheckBlock): string[] {
  const lines = (check.pass_stands?.subjects ?? []).flatMap((subject) => [
    `  - ${subjectPlace(subject.basis)} at ${subject.basis.commit ?? "an unnamed commit"}: ${subjectStateWords(subject)}`,
    ...(subject.ci === undefined ? [] : [`    Pull request line: ${subject.ci.line}`]),
  ]);
  const merged = mergedPullRequestsWords(check);
  return merged === undefined ? lines : [...lines, `  ${merged}`];
}

/** The line of the reading one role recorded, under the line of its result at `indent`. The
 *  reading is one line however many it holds: its line breaks are written as `\n`, as every string
 *  of the answer is (`formatCheckResults`), so no line of a CI job's log reads as a line of this
 *  command's own. `brief` cuts a reading whose result stands at none of the compared commits, and
 *  leaves every other reading whole: the reading is the record, and a reader who asked for the
 *  gate alone still reads the whole of what stands. */
function readingLine(result: CheckResult, brief: boolean, indent = "    "): string {
  const observed = result.observed;
  if (observed === "") return `${indent}The role recorded no reading.`;
  const cut = brief && result.commit_stands?.answer !== "at_basis";
  if (cut && observed.length > BRIEF_READING_CHARACTERS) {
    return `${indent}Reading: ${observed.slice(0, BRIEF_READING_CHARACTERS)}… (result ${result.id})`;
  }
  return `${indent}Reading: ${observed}`;
}

/** One result, in the order the API served it: the verdict, the role, the branch a CI job's
 *  result was pushed on, the whole commit, R4's two facts, the time, the id, the retraction that
 *  stands against it, and the reading; then, for a result that counts for nothing, the reason
 *  Dispatch served in `not_counted`. */
function resultLines(check: CheckBlock, result: CheckResult, brief: boolean): string[] {
  const branch = result.branch === undefined ? "" : ` on branch ${result.branch}`;
  const lines = [
    `  - ${result.verdict} by ${result.role}${branch} at ${result.commit_sha}. ${commitStandsText(result.commit_stands)}. ${checkWordsText(check, result)}. Recorded ${result.created_at}. Result ${result.id}.`,
  ];
  const retracted = result.retracted;
  if (retracted !== undefined) {
    lines.push(
      `    Retracted by ${actorText(retracted.actor)} at ${retracted.created_at}. The reason: ${retracted.reason}`
    );
  }
  lines.push(readingLine(result, brief));
  const notCounted = result.not_counted ?? "";
  if (notCounted !== "") lines.push(`    It counts for nothing: ${notCounted}`);
  return lines;
}

/** What a derived result's id begins with, before its check run's own id, as Dispatch writes it
 *  (`ciResultIDPrefix` in packages/envoy/internal/dispatch/api/results_ci.go). */
const CI_RESULT_ID_PREFIX = "ci-run:";

/** The result `ci` derived, among results. Dispatch names the head `ci` read (`commit`, and
 *  `repo`) and the result that read derived (`result`, absent where the run decided nothing), so
 *  the result is the one of that id, and no other read's result can stand in for it however alike
 *  their words are. A server that names no head of `ci` read one head alone, the newest, so the
 *  result derived from a run is that read's, wherever the API serves it. */
function derivedResult(ci: CheckCI, results: readonly CheckResult[]): CheckResult | undefined {
  if (ci.commit === undefined) {
    return results.find((result) => result.id.startsWith(CI_RESULT_ID_PREFIX));
  }
  return ci.result === undefined ? undefined : results.find((result) => result.id === ci.result);
}

/** A check CI proves, which Dispatch marks with `ci`: no role records its result. The result
 *  derived from the read `ci` names (`derivedResult`) reads as one line with the check: its
 *  verdict, its commit, the producer `ci`, the run and the conclusion that decided it, and where
 *  that commit stands, as a recorded result's line says it; a check the run did not decide says
 *  why, in the sentence Dispatch wrote. The check's words end the line. Whether a pass stands
 *  follows, from the same `pass_stands` and in the same words as a recorded check's head line, so
 *  the gate reads both kinds alike, then the line of each head (`subjectLines`), each with the
 *  pull-request body line Dispatch wrote for that head, which the implementer copies into that
 *  pull request's body. A server that names no read at each head gets the one body line of `ci`
 *  under the pass line instead, and so does no issue with no pull request head (`no_head`), which
 *  has no body to write it in. Then the run's reading: its conclusion, output and URL. Each other
 *  result follows, the derived results at the other heads and each the CI job recorded, in the
 *  order the API serves them, as a recorded result prints (`resultLines`). */
function ciCheckLines(
  check: CheckBlock,
  ci: CheckCI,
  pass: CheckPassStands,
  brief: boolean
): string[] {
  const results = check.results ?? [];
  const subjects = pass.subjects ?? [];
  const derived = derivedResult(ci, results);
  const others = results.filter((result) => result !== derived);
  const recorded = others.flatMap((result) => resultLines(check, result, brief));
  const passStandsLine = `  ${passStandsText(check, pass)}.`;
  const perHead = subjects.some((subject) => subject.ci !== undefined);
  const pullRequestLine =
    perHead || ci.case === "no_head" ? [] : [`  Pull request line: ${ci.line}`];
  const heads = subjectLines(check);
  if (derived === undefined) {
    // A pass the CI job recorded may stand where the run derives nothing, so the head line says
    // so rather than "no result" above a line that says a pass stands. A result derived at
    // another head is no result of the job: its head's line says what it decides.
    const jobResults = others.some((result) => !result.id.startsWith(CI_RESULT_ID_PREFIX));
    const head = jobResults
      ? `its CI job's results below, and CI run ${ci.run} derives none`
      : "no result";
    return [
      `- ${check.block_id} (${check.when}): ${head}: ${ci.detail} ${check.text}`,
      passStandsLine,
      ...heads,
      ...pullRequestLine,
      ...recorded,
    ];
  }
  const stands = commitStandsText(derived.commit_stands);
  return [
    `- ${check.block_id} (${check.when}): ${derived.verdict} at ${derived.commit_sha} by ${derived.role} (CI run ${ci.run}, ${ci.case}), ${stands.charAt(0).toLowerCase()}${stands.slice(1)}. ${check.text}`,
    passStandsLine,
    ...heads,
    ...pullRequestLine,
    readingLine(derived, brief, "  "),
    ...recorded,
  ];
}

/** One check: a head line naming what it compared with and whether a pass of it stands, the line
 *  of the deploy a check of a running program reads, which a pass must come from, the line of each
 *  head a pass must stand at (`subjectLines`), the check's own words once, then every result
 *  recorded against it. Where an open pull request's head is a subject, the head line counts the
 *  open pull requests in place of naming each basis, since each head has its own line and the
 *  merged ones have a line of count. `repository` is the check owner's repository, which the
 *  answer about a branch check's program names (`branchPassStandsText`).
 *
 *  An answer that names no `pass_stands` says nothing of whether a pass stands, and this prints
 *  nothing of it either: no state on the head line, no line for each head, and no line of a CI
 *  read, which the server leaves out with it. The head line ends at what the check compared with,
 *  counting the open pull requests among its bases, and the words and every result follow, so a
 *  reader judges the results themselves. A run's Dispatch server answers so while its state is
 *  hidden (`DISPATCH_TEST_HIDE_STATE_FILE`), and so does a server that predates the answer. */
function checkLines(check: CheckBlock, brief: boolean, repository?: string): string[] {
  const pass = check.pass_stands;
  if (pass !== undefined && check.ci !== undefined) {
    return ciCheckLines(check, check.ci, pass, brief);
  }
  const bases = check.compared_with ?? [];
  const reason = check.compared_nothing;
  const open = (
    pass === undefined ? bases : (pass.subjects ?? []).map((subject) => subject.basis)
  ).filter((basis) => basis.kind === "pull_request" && basis.merged !== true).length;
  const compared =
    open > 0
      ? `Compared with the heads of ${open} open ${open === 1 ? "pull request" : "pull requests"}`
      : bases.length > 0
        ? `Compared with ${bases.map(basisText).join(", ")}`
        : reason === undefined
          ? "Compared with nothing, and this Dispatch server does not say why"
          : `Compared with nothing: ${reasonText(reason)}`;
  const results = check.results ?? [];
  const deploy = pass?.deploy;
  return [
    pass === undefined
      ? `- ${check.block_id} (${check.when}). ${compared}.`
      : `- ${check.block_id} (${check.when}). ${compared}. ${passStandsText(check, pass, repository)}.`,
    ...(deploy === undefined
      ? []
      : [
          `  Program ${deploy.program} runs ${deploy.commit} since ${deploy.deployed_at}; a pass of this check must come from this deploy.`,
        ]),
    ...subjectLines(check),
    `  The check's words: ${check.text}`,
    ...(results.length === 0
      ? ["  No result is recorded for this check."]
      : results.flatMap((result) => resultLines(check, result, brief))),
  ];
}

/** What `dispatch results` prints: each check of the issue's Verification document, what Dispatch
 *  compared its results with, whether a pass of it stands, its own words once, and every result
 *  recorded against it in the order the API serves them. A check CI proves prints its derived
 *  result, or why it has none, as one line, then whether a pass of it stands, then each result
 *  its CI job recorded (`ciCheckLines`). A check with no result says so, and an issue with no
 *  Verification document says that instead of printing an empty list: Dispatch answers
 *  `verification_artifact_id` "" for such an issue, never null, so that is what is compared.
 *  `repositories` names each check owner's repository, by issue key, where a check's answer names
 *  it and its bases do not (`resultsCommand`, issues.ts).
 *
 *  Every string of the answer is text this command did not write: a CI job's log, a role's
 *  reading, a check's words, a reason. Each is read as runnerlog.Printable writes it
 *  (`printableStrings`, runner-log.ts), so it prints inside a line of this command's own, its
 *  line breaks as `\n`, and no line it prints holds a "##[": no text from outside starts a line,
 *  and none reads as a line this command wrote, such as a check's head line or its
 *  `A pass stands:` line. */
export function formatCheckResults(
  answer: IssueCheckResults,
  brief = false,
  repositories?: Readonly<Record<string, string>>
): string {
  const results = printableStrings(answer);
  if (results.verification_artifact_id === "") {
    return `${results.issue} has no Verification document, so it has no checks. Write one (verification.md) with a check block per thing that must be proved.`;
  }
  const checks = results.checks ?? [];
  if (checks.length === 0) {
    return `${results.issue}'s Verification document holds no check blocks.`;
  }
  const blocks = checks.map((check) =>
    checkLines(checkWithNewerFields(check), brief, repositories?.[check.issue_key]).join("\n")
  );
  return [`Checks of ${results.issue}:`, ...blocks].join("\n\n");
}
