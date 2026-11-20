import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { issueResultsQuery } from "../../api/queries";
import type {
  Artifact,
  CheckBasis,
  CheckBlock,
  CheckResult,
  ResultCommitStands,
} from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { pillClassName, pillShapeClassName } from "../../components/Pill";
import {
  card,
  dangerText,
  successText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ReplacingUploadNote } from "../artifacts/ArtifactUpload";
import { AgentName } from "../refs/AgentName";
import { BlockReferences } from "../refs/ReferencedBy";
import { ReferenceLink } from "../refs/ReferenceLink";
import { Timestamp } from "../refs/Timestamp";

/**
 * The one line that answers the gate's question for a check: whether a pass of it stands at
 * what the check compared its results with. The server reads it over every result of the
 * check, so no word here comes from the newest result alone: a pass recorded before a fail
 * still stands, and a fail recorded after a pass takes nothing away from it.
 */
const PASS_STANDS_LABELS: Record<CheckBlock["pass_stands"]["answer"], string> = {
  no: "No pass stands",
  unknown: "Dispatch cannot say whether a pass stands",
  yes: "A pass stands",
};

/** The pill sits on `surfaceMutedBg`, so its muted answer takes the token written for that
 *  background: `textMutedOnSurface` is slate-500, which is 4.34:1 on slate-100 and under
 *  WCAG AA's 4.5:1 for text this size (axe's `color-contrast`, serious). */
const PASS_STANDS_TEXT: Record<CheckBlock["pass_stands"]["answer"], string> = {
  no: dangerText,
  unknown: textMutedOnSurfaceMuted,
  yes: successText,
};

/** Why a check compared its results with nothing at all, and why one result's commit could not
 *  be placed. A check takes all three and a result all but no_pull_request_or_branch, so the
 *  check's set types this map and answers both. */
const REASON_WORDS: Record<NonNullable<CheckBlock["compared_nothing"]>, string> = {
  no_pull_request_or_branch: "This issue holds no pull request and no branch.",
  read_failed: "A read Dispatch made did not answer.",
  repository_unreadable: "Dispatch cannot read the repository.",
};

/** Which time a basis learned from a pull request carries: when Dispatch last read the pull
 *  request from GitHub, or the pull request's own update time as GitHub gave it. */
const LEARNED_AT_WORDS: Record<NonNullable<CheckBasis["learned_at"]>["kind"], string> = {
  read_at: "Dispatch read it",
  updated_at: "GitHub updated it",
};

/** One name/value row of a check or a result. A value the writer left blank still shows its
 *  name: that the check named no negative control is itself what a reader came to see. */
function Fact({
  children,
  label,
  testId,
}: {
  children?: ReactNode;
  label: string;
  testId: string;
}): ReactNode {
  return (
    <div className="flex flex-wrap items-baseline gap-x-2">
      <dt className={`text-xs font-medium ${textMutedOnSurface}`}>{label}</dt>
      <dd className={`text-sm ${textSecondaryOnSurface}`} data-testid={testId}>
        {children === undefined || children === "" ? "—" : children}
      </dd>
    </div>
  );
}

/** A screenshot a result names, which is a `dispatch://` reference to an image document of the
 *  issue that recorded it; one that names nothing shows as the text it is. */
function Screenshot({ reference }: { reference: string }): ReactNode {
  return <ReferenceLink reference={reference} testId="verification-result-screenshot" />;
}

/** What one basis is, in words: the commit a reader named in the request, the head of a pull
 *  request, or a branch. The commit itself is drawn beside this, whole. */
function basisWords(basis: CheckBasis): string {
  if (basis.kind === "branch") {
    return `branch ${basis.branch ?? ""}`;
  }
  if (basis.kind === "pull_request") {
    return basis.pull_request === undefined
      ? "the head of the pull request"
      : `the head of pull request ${basis.pull_request}`;
  }
  return "the commit this read named";
}

/** One commit or branch a check's results are compared with: what it is, its whole commit
 *  where it has one, and when Dispatch learned that commit. A reader who sees a comparison
 *  older than the last push reads it here. */
function Basis({ basis }: { basis: CheckBasis }): ReactNode {
  const commit = basis.commit ?? "";
  return (
    <>
      {basisWords(basis)}
      {commit === "" ? null : (
        <>
          {" "}
          <code className="break-all">{commit}</code>
        </>
      )}
      {basis.learned_at === undefined ? null : (
        <span className={textMutedOnSurface}>
          {" ("}
          {LEARNED_AT_WORDS[basis.learned_at.kind]} <Timestamp at={basis.learned_at.at} />)
        </span>
      )}
    </>
  );
}

/** The first fact of each result: where its commit stands against what its check compared
 *  with. At one of those commits, naming which; at another commit; or not known, with the
 *  reason Dispatch could not say. */
function CommitStands({ stands }: { stands: ResultCommitStands | undefined }): ReactNode {
  if (stands === undefined) {
    return undefined;
  }
  if (stands.answer === "at_basis") {
    return stands.basis === undefined ? (
      "At a commit this check compared with"
    ) : (
      <>
        At <Basis basis={stands.basis} />
      </>
    );
  }
  if (stands.answer === "other_commit") {
    return "At another commit";
  }
  // The reason is the server's to give and may be absent, so an unknown answer stands on its
  // own where there is none.
  return stands.reason === undefined ? "Not known." : `Not known. ${REASON_WORDS[stands.reason]}`;
}

/** The second fact of each result: whether the check's words moved after the result was
 *  recorded. The two versions are the reader's handles on the wording each one read. */
function checkWordsText(result: CheckResult, checkVersion: number): string | undefined {
  if (result.check_words_moved === undefined) {
    return undefined;
  }
  return result.check_words_moved
    ? `An older wording of this check (version ${result.check_version}, the check is at ${checkVersion})`
    : `This check's words (version ${result.check_version})`;
}

function Result({
  checkVersion,
  result,
}: {
  checkVersion: number;
  result: CheckResult;
}): ReactNode {
  const retracted = result.retracted;
  return (
    <li className="space-y-2" data-testid="verification-result">
      <div className={`flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurface}`}>
        <span
          className={`${pillShapeClassName} ${surfaceMutedBg} ${result.verdict === "pass" ? successText : dangerText}`}
          data-testid="verification-result-verdict"
        >
          {result.verdict === "pass" ? "Pass" : "Fail"}
        </span>
        {retracted === undefined ? null : (
          <span className={pillClassName("label")} data-testid="verification-result-retracted">
            Retracted
          </span>
        )}
        <span data-testid="verification-result-role">{result.role}</span>
        <span aria-hidden="true">·</span>
        <Timestamp at={result.created_at} />
      </div>
      <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-2">
        <Fact label="Surface" testId="verification-result-surface">
          {result.surface}
        </Fact>
        <Fact label="Run" testId="verification-result-run">
          {result.run}
        </Fact>
        <Fact label="Observed" testId="verification-result-observed">
          {result.observed === "" ? "This result carries no reading." : result.observed}
        </Fact>
        <Fact label="Commit" testId="verification-result-commit">
          {result.commit_sha === "" ? undefined : (
            <code className="break-all">{result.commit_sha}</code>
          )}
        </Fact>
        <Fact label="The commit stands" testId="verification-result-commit-stands">
          <CommitStands stands={result.commit_stands} />
        </Fact>
        <Fact label="Recorded against" testId="verification-result-check-words">
          {checkWordsText(result, checkVersion)}
        </Fact>
        <Fact label="Negative control" testId="verification-result-negative-control">
          {result.negative_control}
        </Fact>
        <Fact label="Failed" testId="verification-result-failed">
          {String(result.failed)}
        </Fact>
        <Fact label="Skipped" testId="verification-result-skipped">
          {String(result.skipped)}
        </Fact>
        <Fact label="Excluded" testId="verification-result-excluded">
          {String(result.excluded)}
        </Fact>
      </dl>
      {retracted === undefined ? null : (
        <p className={`text-sm ${dangerText}`} data-testid="verification-result-retraction">
          Retracted: {retracted.reason}{" "}
          <span className={textMutedOnSurface}>
            · <AgentName actor={retracted.actor} inline /> · <Timestamp at={retracted.created_at} />
          </span>
        </p>
      )}
      {(result.screenshots ?? []).length === 0 ? null : (
        <div className={`flex flex-wrap items-center gap-x-2 text-sm ${textMutedOnSurface}`}>
          <span className="text-xs font-medium">Screenshots</span>
          {(result.screenshots ?? []).map((reference) => (
            <Screenshot key={reference} reference={reference} />
          ))}
        </div>
      )}
    </li>
  );
}

/** What the server compared this check's results with, in the order it asked about them, or
 *  the reason it compared them with nothing. */
function ComparedWith({ check }: { check: CheckBlock }): ReactNode {
  const bases = check.compared_with ?? [];
  if (bases.length === 0) {
    // compared_nothing is the server's to give: where it holds none, the fact shows its dash.
    return check.compared_nothing === undefined ? "" : REASON_WORDS[check.compared_nothing];
  }
  return bases.map((basis, index) => (
    <span key={`${basis.kind}-${basis.commit ?? basis.branch ?? index}`}>
      {index === 0 ? null : ", "}
      <Basis basis={basis} />
    </span>
  ));
}

function Check({ check }: { check: CheckBlock }): ReactNode {
  const stands = check.pass_stands.answer;
  const roles = check.pass_stands.roles ?? [];
  const results = check.results ?? [];
  return (
    <li
      className={`space-y-3 rounded-lg p-3 ${card}`}
      data-pass-stands={stands}
      data-testid="verification-check"
    >
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="inline-flex flex-wrap items-baseline gap-x-2">
          <span
            className={`${pillShapeClassName} ${surfaceMutedBg} ${PASS_STANDS_TEXT[stands]}`}
            data-testid="verification-check-pass-stands"
          >
            {PASS_STANDS_LABELS[stands]}
          </span>
          {roles.length === 0 ? null : (
            <span
              className={`text-sm ${textMutedOnSurface}`}
              data-testid="verification-check-roles"
            >
              from {roles.join(", ")}
            </span>
          )}
        </span>
        <span className="font-medium">{check.text}</span>
      </div>
      <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-2">
        <Fact label="When" testId="verification-check-when">
          {check.when}
        </Fact>
        <Fact label="Surface" testId="verification-check-surface">
          {check.surface}
        </Fact>
        <Fact label="Run" testId="verification-check-run">
          {check.run}
        </Fact>
        {check.ci === undefined ? null : (
          <Fact label="CI check" testId="verification-check-ci">
            {check.ci.run}
          </Fact>
        )}
        <Fact label="Negative control" testId="verification-check-negative-control">
          {check.negative_control}
        </Fact>
        <Fact label="Compared with" testId="verification-check-compared-with">
          <ComparedWith check={check} />
        </Fact>
      </dl>
      <BlockReferences reference={check.ref} />
      <CheckResults check={check} results={results} />
    </li>
  );
}

/** A check's results, newest first. A check CI proves holds no recorded result, only the one the
 *  server derives from the CI run it names, drawn as a recorded one is; where that run proved
 *  nothing, the server's sentence saying why stands in its place. The tab reads that run as of
 *  its own load and asks GitHub nothing more. */
function CheckResults({
  check,
  results,
}: {
  check: CheckBlock;
  results: readonly CheckResult[];
}): ReactNode {
  if (results.length === 0) {
    return check.ci === undefined ? (
      <p className={`text-sm ${textMutedOnSurface}`} data-testid="verification-check-no-results">
        No result recorded against this check.
      </p>
    ) : (
      <p className={`text-sm ${textMutedOnSurface}`} data-testid="verification-check-ci-unproved">
        {check.ci.detail}
      </p>
    );
  }
  return (
    <ul
      aria-label={`Results of ${check.block_id}`}
      className="space-y-3"
      data-testid="verification-check-results"
    >
      {results.map((result) => (
        <Result checkVersion={check.version} key={result.id} result={result} />
      ))}
    </ul>
  );
}

function Checks({ issueKey }: { issueKey: string }): ReactNode {
  const results = useQuery(issueResultsQuery(issueKey));

  if (results.isPending) {
    return (
      <p className={textMutedOnCanvas} data-testid="verification-loading">
        Loading checks…
      </p>
    );
  }
  if (results.isError) {
    return (
      <p className={dangerText} data-testid="verification-error">
        Could not load the checks.
      </p>
    );
  }
  if ((results.data.checks ?? []).length === 0) {
    return (
      <div data-testid="verification-no-checks">
        <EmptyState
          label="Verification checks empty state"
          message="This Verification document marks no checks."
        />
      </div>
    );
  }

  return (
    <ul aria-label="Checks" className="space-y-3" data-testid="verification-checks">
      {(results.data.checks ?? []).map((check) => (
        <Check check={check} key={check.block_id} />
      ))}
    </ul>
  );
}

/**
 * The issue's Verification document and, under it, each `check` block of it with every result
 * recorded against that check, newest first. `children` is the document itself, which the page
 * renders, so this panel owns only what the checks say. An issue with no Verification document
 * shows the command that makes one.
 */
export function VerificationTab({
  artifact,
  children,
  issueKey,
}: {
  artifact: Artifact | undefined;
  children?: ReactNode;
  issueKey: string;
}): ReactNode {
  if (artifact === undefined) {
    return (
      <div data-testid="verification-empty">
        <EmptyState
          action={
            <>
              <code>{`dispatch artifact --issue ${issueKey} --name verification.md`}</code>
              <ReplacingUploadNote />
            </>
          }
          label="Verification empty state"
          message={`${issueKey} has no Verification document. Make one with:`}
        />
      </div>
    );
  }

  return (
    <section className="space-y-4" data-testid="verification-tab">
      {children}
      <Checks issueKey={issueKey} />
    </section>
  );
}
