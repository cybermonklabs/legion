import type { CheckBasis, CheckBlock, PassStandsSubject } from "./dispatch-api";

// check-results-words.ts holds the words of the state at each head of a check, which
// `dispatch results` prints on the line of each head and the Verification tab shows in the row of
// each head, so the two say one thing in one way (R9 to R11 of the spec of several open pull
// requests).

/**
 * A head's place, as its line names it: `#<number>` for an open pull request's head,
 * `#<number> (merged)` for a merged one's final head, and `branch <name>` for a branch head. A
 * pull request in another repository than the issue's, whose basis names that repository, is
 * `<owner>/<repo>#<number>`, since a number alone does not say which repository holds it (R18 of
 * the spec of several open pull requests).
 */
export function subjectPlace(basis: CheckBasis): string {
  if (basis.kind === "branch_head") return `branch ${basis.branch ?? "unnamed"}`;
  const number =
    basis.pull_request === undefined
      ? "a pull request"
      : `${basis.repo ?? ""}#${basis.pull_request}`;
  return basis.merged === true ? `${number} (merged)` : number;
}

/**
 * The state at one head, its roles and the ids of the results that decide it, as the words after
 * `<place> at <commit>: `. A pass and a fail at the head name both, and what ends that state; a
 * head whose CI run Dispatch could not read names the run.
 */
export function subjectStateWords(subject: PassStandsSubject): string {
  const ids = subject.results ?? [];
  if (subject.answer === "yes") {
    const roles = subject.roles ?? [];
    const from = roles.length === 0 ? "yes." : `yes, from ${roles.join(", ")}.`;
    if (ids.length === 0) return from;
    return ids.length === 1
      ? `${from} Pass ${ids[0]} decides it.`
      : `${from} Passes ${ids.join(", ")} decide it.`;
  }
  if (subject.answer === "no") {
    if (subject.reason !== "fail") return "no, as it holds no result.";
    return ids.length === 0 ? "no, as a fail counts there." : `no, as fail ${ids[0]} counts there.`;
  }
  if (subject.reason === "pass_and_fail") {
    // Dispatch names the newest pass, then the newest fail, at the head (PassStandsSubject).
    const [pass, fail] = ids;
    return `not known, as pass ${pass} and fail ${fail} count at this commit. A later pass does not end this. A retraction of the wrong one ends it, and so does a change of the check's words. A new head of the pull request gets a state of its own.`;
  }
  if (subject.reason === "read_failed") {
    return `not known, as Dispatch could not read CI run ${subject.ci?.run ?? "of the check"} there.`;
  }
  return "not known.";
}

/**
 * The line that counts the merged pull requests a check compared with, which decide nothing
 * while an open one's head is a subject of it; undefined where no open head is a subject or no
 * merged pull request is compared with. A merged pull request gets no line of its own then.
 */
export function mergedPullRequestsWords(check: CheckBlock): string | undefined {
  const open = (check.pass_stands?.subjects ?? []).some(
    (subject) => subject.basis.kind === "pull_request" && subject.basis.merged !== true
  );
  const merged = (check.compared_with ?? []).filter(
    (basis) => basis.kind === "pull_request" && basis.merged === true
  ).length;
  if (!open || merged === 0) return undefined;
  return merged === 1
    ? "The 1 merged pull request decides nothing while one is open."
    : `The ${merged} merged pull requests decide nothing while one is open.`;
}
