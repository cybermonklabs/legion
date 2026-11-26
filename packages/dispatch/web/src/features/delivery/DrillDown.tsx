import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectDeployRunQuery } from "../../api/queries";
import type {
  DeliveryComponent,
  DeliveryPR,
  DeliveryRun,
  DeliveryRunJobDetail,
  DeliverySessionGroup,
  ProductionEvent,
} from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { useIsHuman } from "../../hooks/useIsHuman";
import {
  borderStrong,
  dangerText,
  dismissButtonText,
  inlineWarningText,
  inputClasses,
  linkText,
  secondaryButtonCompact,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { buildIssuePath } from "../refs/routes";
import { useDialog } from "../shell/useDialog";
import { refusedNote, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { NO_COMPONENT, NO_ISSUE, NO_PRIORITY, NO_SESSION, PLACEHOLDER_LABELS } from "./lib/facets";
import { parentAgentLabel } from "./lib/parentAgent";
import { leadTimeMinutes } from "./lib/prList";
import { storedSessionTitle } from "./lib/sessionTitles";
import type { TimelineSelection } from "./Timeline";

const sectionLabel = `mt-2 text-xs uppercase ${textMutedOnSurface}`;

/** A run the drill-down opens: a deploy or failed run row of the project's timeline, which names
 *  the repository the run is in, as its read does (dispatch://INFRA-2918, D14). */
type DrillDownRun = DeliveryRun & { readonly repo: string };

function fmt(iso: string | null): string {
  return iso === null ? "\u2014" : new Date(iso).toLocaleString();
}

function jobDurationMs(job: DeliveryRunJobDetail): number | null {
  if (job.started_at === null || job.completed_at === null) return null;
  return new Date(job.completed_at).getTime() - new Date(job.started_at).getTime();
}

function PRDetail({
  pr,
  components,
  sessionGroups,
  sessionTitles,
}: {
  pr: DeliveryPR;
  components: Readonly<Record<string, DeliveryComponent>>;
  sessionGroups: readonly DeliverySessionGroup[];
  sessionTitles: Readonly<Record<string, string>>;
}): ReactNode {
  const leadTime = leadTimeMinutes(pr);
  let componentText = PLACEHOLDER_LABELS[NO_ISSUE];
  if (pr.issue !== null) {
    componentText =
      (pr.components ?? []).length === 0
        ? PLACEHOLDER_LABELS[NO_COMPONENT]
        : (pr.components ?? []).map((id) => components[id]?.title ?? id).join(", ");
  }
  return (
    <div className="flex flex-col gap-2">
      <h3 className={`pr-6 text-lg font-semibold ${textPrimaryOnSurface}`}>
        {pr.id}{" "}
        {pr.rework ? (
          <span className={`ml-2 align-middle text-xs ${inlineWarningText}`}>rework</span>
        ) : null}
      </h3>
      <p className={textSecondaryOnSurface}>{pr.title}</p>
      <a className={`${linkText} underline`} href={pr.url} rel="noreferrer" target="_blank">
        Open on GitHub
      </a>
      <dl className={`grid grid-cols-2 gap-x-4 gap-y-1 text-sm ${textPrimaryOnSurface}`}>
        <dt className={textMutedOnSurface}>Author</dt>
        <dd>{pr.author}</dd>
        <dt className={textMutedOnSurface}>Dispatch issue</dt>
        <dd>
          {pr.issue === null ? (
            "\u2014"
          ) : (
            <Link
              className={`${linkText} underline`}
              to={buildIssuePath({ key: pr.issue, kind: "issue" })}
            >
              {pr.issue}
              {pr.issue_title === null ? "" : ` \u2014 ${pr.issue_title}`}
            </Link>
          )}
        </dd>
        <dt className={textMutedOnSurface}>Priority</dt>
        <dd>
          {pr.issue === null
            ? PLACEHOLDER_LABELS[NO_ISSUE]
            : (pr.priority ?? PLACEHOLDER_LABELS[NO_PRIORITY])}
        </dd>
        <dt className={textMutedOnSurface}>Components</dt>
        <dd>{componentText}</dd>
        <dt className={textMutedOnSurface}>Parent agent(s)</dt>
        <dd>
          {pr.parent_agent === null
            ? PLACEHOLDER_LABELS[NO_SESSION]
            : parentAgentLabel(pr.parent_agent, sessionGroups)}
        </dd>
        <dt className={textMutedOnSurface}>Sessions</dt>
        {/* Each session wraps onto its own line rather than being cut, so every one stays
            reachable; the delivery read's titles hold a session's id when it has no title. */}
        <dd className="flex min-w-0 flex-col items-start break-words">
          {(pr.sessions ?? []).length === 0
            ? "\u2014"
            : (pr.sessions ?? []).map((session) => (
                <AgentName
                  className="max-w-full"
                  key={session}
                  sessionId={session}
                  storedTitle={storedSessionTitle(sessionTitles, session)}
                />
              ))}
        </dd>
        <dt className={textMutedOnSurface}>Merged</dt>
        <dd>{fmt(pr.merged_at)}</dd>
        <dt className={textMutedOnSurface}>Deployed</dt>
        <dd>{fmt(pr.deployed_at)}</dd>
        <dt className={textMutedOnSurface}>Lead time (merge→prod)</dt>
        <dd>{leadTime === undefined ? "not yet deployed" : `${Math.round(leadTime)} min`}</dd>
      </dl>
      {pr.partial ? (
        <p className={`text-sm ${textMutedOnSurface}`}>
          Still completing: some fields await the next reconcile pass.
        </p>
      ) : null}
      {pr.unfetchable_reason === null ? null : (
        <p className={`text-sm ${textMutedOnSurface}`}>
          Can no longer be fetched from GitHub: {pr.unfetchable_reason}
        </p>
      )}
    </div>
  );
}

/** Every job of the run, with each result, read once the drill-down opens: the project's read of
 *  the run, from the repository its timeline row names (dispatch://INFRA-2918, D14). */
function RunJobs({
  durations,
  project,
  run,
}: {
  durations: boolean;
  project: string;
  run: DrillDownRun;
}): ReactNode {
  const detail = useQuery(projectDeployRunQuery(project, run.repo, run.id));
  if (detail.isPending) return <div className={`text-sm ${textMutedOnSurface}`}>Loading…</div>;
  if (detail.isError) {
    return <div className={`text-sm ${textMutedOnSurface}`}>The run's jobs could not be read.</div>;
  }
  return (detail.data.jobs ?? []).map((job) => {
    const durationMs = jobDurationMs(job);
    return (
      <div className="flex justify-between gap-2 text-sm" key={job.name}>
        <span className={textPrimaryOnSurface}>{job.name}</span>
        <span className={job.conclusion === "failure" ? dangerText : textSecondaryOnSurface}>
          {job.conclusion}
          {durations && durationMs !== null ? ` (${Math.round(durationMs / 1000)}s)` : ""}
        </span>
      </div>
    );
  });
}

/** One write of the deploy's record, through dispatch://INFRA-978's routes: a person's flag,
 *  posted to the deploy row's project, or a failure's restore or reject, written to the failure's
 *  own project. */
type RecordWrite =
  | {
      readonly kind: "flag";
      readonly project: string;
      readonly input: Parameters<typeof api.postProductionEvent>[1];
    }
  | { readonly kind: "restored"; readonly project: string; readonly id: string }
  | {
      readonly kind: "rejected";
      readonly project: string;
      readonly id: string;
      readonly reason: string;
    };

/** Each write of the record in the reader's words: what the shell's pending writes name it, and
 *  what its refusal says could not be done. */
const RECORD_WORDS: Record<RecordWrite["kind"], { label: string; refused: string }> = {
  flag: { label: "Flag of deploy run", refused: "flag this deploy" },
  rejected: { label: "Reject of a failure of deploy run", refused: "reject this failure" },
  restored: { label: "Restore of a failure of deploy run", refused: "mark this failure restored" },
};

/** The form the record has open: the flag, with the pull requests ticked and the note, or the
 *  reject of one failure, with its reason. One at a time, so no two writes are out at once. */
type RecordForm =
  | { readonly kind: "flag"; readonly ticked: readonly string[]; readonly note: string }
  | { readonly kind: "reject"; readonly id: string; readonly reason: string };

/** How each signal made its failure, in words. */
const SIGNAL_WORDS: Record<NonNullable<ProductionEvent["signal"]>, string> = {
  api: "Posted",
  check_fail: "A failed check",
  incident_issue: "An incident issue",
  person: "A person's flag",
  revert: "A revert",
};

const fieldClass = `rounded border px-2 py-1 text-sm ${inputClasses(true)} ${textPrimaryOnSurface}`;

/** One failure that blames the deploy, as the record holds it: its signal and writer, the pull
 *  requests it blames, its note, its restore and its reject. */
function FailureLines({ failure }: { failure: ProductionEvent }): ReactNode {
  const blamed = failure.pull_requests ?? [];
  let restorer = "\u2014";
  if (failure.restored_by !== null) restorer = failure.restored_by.id;
  else if (failure.restored_by_deploy !== null) restorer = `deploy ${failure.restored_by_deploy}`;
  return (
    <>
      <span className={failure.state === "rejected" ? textMutedOnSurface : dangerText}>
        {failure.signal === null ? "A failure" : SIGNAL_WORDS[failure.signal]} by {failure.actor.id}
        , {fmt(failure.detected_at)}
      </span>
      <span className={textSecondaryOnSurface} data-blamed>
        Pull requests: {blamed.length === 0 ? "none named" : blamed.join(", ")}
      </span>
      {failure.note === "" ? null : (
        <span className={textSecondaryOnSurface} data-note>
          Note: {failure.note}
        </span>
      )}
      {failure.restored_at === null ? null : (
        <span className={textSecondaryOnSurface} data-restored>
          Restored {fmt(failure.restored_at)} by {restorer}
        </span>
      )}
      {failure.state === "rejected" ? (
        <span className={textSecondaryOnSurface} data-rejected>
          Rejected by {failure.state_by?.id ?? "\u2014"}: {failure.state_reason}
        </span>
      ) : null}
    </>
  );
}

/**
 * The deploy's record and its controls (dispatch://INFRA-2918/spec R14, plan D14): every failure
 * that blames the deploy, whatever signal made it, in any state, from the run's read. A person
 * flags the deploy (Flag: the pull requests it shipped to tick, and a note), marks a person's
 * counted flag restored, and rejects any counted failure with a reason; a rejected failure stays
 * listed with its reason and writer. Each write goes to dispatch://INFRA-978's routes, and once
 * Dispatch answers it the run is read again, so the record shown is Dispatch's, never one of the
 * page's own. While a write is out every control is disabled and its form kept; a refusal is
 * shown under the record, and nothing is read again.
 */
function DeployRecord({ project, run }: { project: string; run: DrillDownRun }): ReactNode {
  const queryClient = useQueryClient();
  const human = useIsHuman();
  const runRead = projectDeployRunQuery(project, run.repo, run.id);
  const detail = useQuery(runRead);
  const [form, setForm] = useState<RecordForm | null>(null);
  const record = useKeyedWrite<ProductionEvent, RecordWrite>({
    inline: true,
    label: (write) => `${RECORD_WORDS[write.kind].label} #${run.id}`,
    method: (write) => (write.kind === "flag" ? "POST" : "PUT"),
    write: (write, bounds) => {
      switch (write.kind) {
        case "flag":
          return api.postProductionEvent(write.project, write.input, bounds);
        case "restored":
          return api.putProductionEventState(
            write.project,
            write.id,
            { state: "restored" },
            bounds
          );
        case "rejected":
          return api.putProductionEventState(
            write.project,
            write.id,
            { reason: write.reason, state: "rejected" },
            bounds
          );
      }
    },
    // The write stays pending until the run is read again, so its form and disabled controls
    // stay until the record shows what it wrote.
    onSuccess: async () => {
      await queryClient.invalidateQueries({ exact: true, queryKey: runRead.queryKey });
      setForm(null);
    },
  });
  if (detail.data === undefined) return null;
  const { deploy } = detail.data;
  const failures = detail.data.failures ?? [];
  const pending = record.isPending;
  const open = (next: RecordForm | null) => {
    record.reset();
    setForm(next);
  };
  let refusal: string | null = null;
  if (record.isError && record.variables !== undefined) {
    const words = RECORD_WORDS[record.variables.kind];
    refusal =
      stoppedNote(record, `the ${words.label.toLowerCase()} #${run.id}`, "made") ??
      refusedNote(words.refused, record.error);
  }
  const shipped = deploy?.pull_requests ?? [];
  let flagRefusal: string | null = null;
  if (form?.kind === "flag" && shipped.length > 0 && form.ticked.length === 0) {
    flagRefusal = "Tick the pull requests the failure blames.";
  }
  const cancel = (
    <button
      className={secondaryButtonCompact}
      disabled={pending}
      onClick={() => open(null)}
      type="button"
    >
      Cancel
    </button>
  );

  return (
    <fieldset className="m-0 flex min-w-0 flex-col gap-2 border-0 p-0" disabled={pending}>
      <legend className={sectionLabel}>Failed changes ({failures.length})</legend>
      {deploy === null ? (
        <p className={`text-sm ${textMutedOnSurface}`}>
          No deploy row records this run, so it cannot be flagged.
        </p>
      ) : null}
      {failures.length === 0 ? null : (
        <ul aria-label="Failures of this deploy" className="m-0 flex list-none flex-col gap-3 p-0">
          {failures.map((failure) => (
            <li
              className="flex flex-col gap-1 text-sm"
              data-failure={failure.id}
              data-signal={failure.signal ?? undefined}
              data-state={failure.state}
              key={failure.id}
            >
              <FailureLines failure={failure} />
              {human && form === null && failure.state === "counted" ? (
                <div className="flex flex-wrap gap-2">
                  {failure.signal === "person" && failure.restored_at === null ? (
                    <button
                      className={secondaryButtonCompact}
                      onClick={() =>
                        record.mutate({
                          id: failure.id,
                          kind: "restored",
                          project: failure.project_key,
                        })
                      }
                      type="button"
                    >
                      Mark restored
                    </button>
                  ) : null}
                  <button
                    className={secondaryButtonCompact}
                    onClick={() => open({ id: failure.id, kind: "reject", reason: "" })}
                    type="button"
                  >
                    Reject
                  </button>
                </div>
              ) : null}
              {form?.kind === "reject" && form.id === failure.id ? (
                <form
                  aria-label="Reject this failure"
                  className="flex flex-col gap-2"
                  onSubmit={(event) => {
                    event.preventDefault();
                    const reason = form.reason.trim();
                    if (reason === "") return;
                    record.mutate({
                      id: failure.id,
                      kind: "rejected",
                      project: failure.project_key,
                      reason,
                    });
                  }}
                >
                  <label className={`flex flex-col gap-1 ${textMutedOnSurface}`}>
                    Reason
                    <textarea
                      className={fieldClass}
                      onChange={(event) => setForm({ ...form, reason: event.target.value })}
                      rows={2}
                      value={form.reason}
                    />
                  </label>
                  <div className="flex flex-wrap gap-2">
                    <RefusableButton
                      busy={pending ? "Rejecting…" : undefined}
                      look="secondary-compact"
                      refusal={
                        form.reason.trim() === "" ? "Write why this failure is rejected." : null
                      }
                      type="submit"
                    >
                      Reject this failure
                    </RefusableButton>
                    {cancel}
                  </div>
                </form>
              ) : null}
            </li>
          ))}
        </ul>
      )}
      {human && deploy !== null && form === null ? (
        <div>
          <button
            className={secondaryButtonCompact}
            onClick={() => open({ kind: "flag", note: "", ticked: [] })}
            type="button"
          >
            Flag
          </button>
        </div>
      ) : null}
      {deploy !== null && form?.kind === "flag" ? (
        <form
          aria-label="Flag this deploy"
          className="flex flex-col gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            if (flagRefusal !== null) return;
            record.mutate({
              input: {
                "cicd.pipeline.run.id": String(run.id),
                kind: "failure",
                note: form.note.trim(),
                pull_requests: shipped.filter((id) => form.ticked.includes(id)),
                repo: run.repo,
              },
              kind: "flag",
              project: deploy.project_key,
            });
          }}
        >
          <fieldset className="m-0 flex min-w-0 flex-col gap-1 border-0 p-0">
            <legend className={`text-sm ${textMutedOnSurface}`}>
              Pull requests this deploy shipped
            </legend>
            {shipped.length === 0 ? (
              <span className={`text-sm ${textMutedOnSurface}`}>
                This deploy shipped no pull request.
              </span>
            ) : (
              shipped.map((id) => {
                const title = run.prs?.find((pr) => pr.id === id)?.title;
                return (
                  <label
                    className={`flex items-center gap-2 text-sm ${textPrimaryOnSurface}`}
                    key={id}
                  >
                    <input
                      checked={form.ticked.includes(id)}
                      onChange={(event) =>
                        setForm({
                          ...form,
                          ticked: event.target.checked
                            ? [...form.ticked, id]
                            : form.ticked.filter((ticked) => ticked !== id),
                        })
                      }
                      type="checkbox"
                    />
                    {title === undefined ? id : `${id} \u2014 ${title}`}
                  </label>
                );
              })
            )}
          </fieldset>
          <label className={`flex flex-col gap-1 text-sm ${textMutedOnSurface}`}>
            Note
            <textarea
              className={fieldClass}
              onChange={(event) => setForm({ ...form, note: event.target.value })}
              rows={3}
              value={form.note}
            />
          </label>
          <div className="flex flex-wrap gap-2">
            <RefusableButton
              busy={pending ? "Flagging…" : undefined}
              look="secondary-compact"
              refusal={flagRefusal}
              type="submit"
            >
              Flag this deploy
            </RefusableButton>
            {cancel}
          </div>
        </form>
      ) : null}
      {refusal === null ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {refusal}
        </p>
      )}
    </fieldset>
  );
}

function DeployDetail({ project, run }: { project: string; run: DrillDownRun }): ReactNode {
  return (
    <div className="flex flex-col gap-2">
      <h3 className={`pr-6 text-lg font-semibold ${textPrimaryOnSurface}`}>
        Deploy: run #{run.id}
      </h3>
      <a className={`${linkText} underline`} href={run.url} rel="noreferrer" target="_blank">
        Open run
      </a>
      <div>
        <div className={sectionLabel}>PRs shipped ({(run.prs ?? []).length})</div>
        {(run.prs ?? []).map((pr) => (
          <div className={`text-sm ${textPrimaryOnSurface}`} key={pr.id}>
            {pr.id} {`\u2014 ${pr.title}`}
          </div>
        ))}
      </div>
      <div>
        <div className={sectionLabel}>Jobs</div>
        <RunJobs durations project={project} run={run} />
      </div>
      <DeployRecord project={project} run={run} />
    </div>
  );
}

function FailureDetail({ project, run }: { project: string; run: DrillDownRun }): ReactNode {
  return (
    <div className="flex flex-col gap-2">
      <h3 className={`pr-6 text-lg font-semibold ${dangerText}`}>
        Pipeline failure: run #{run.id}
      </h3>
      <a className={`${linkText} underline`} href={run.url} rel="noreferrer" target="_blank">
        Open run
      </a>
      <div>
        <div className={sectionLabel}>Failed jobs</div>
        {(run.failed_jobs ?? []).map((job) => (
          <div className={`text-sm ${dangerText}`} key={job.name}>
            {job.name} at {fmt(job.completed_at)}
          </div>
        ))}
        {(run.failed_jobs ?? []).length === 0 && run.production?.conclusion === "failure" ? (
          <div className={`text-sm ${dangerText}`}>
            the production job at {fmt(run.production.completed_at)}
          </div>
        ) : null}
      </div>
      <div>
        <div className={sectionLabel}>All jobs</div>
        <RunJobs durations={false} project={project} run={run} />
      </div>
    </div>
  );
}

/** The timeline and the list's shared drill-down: a PR, a deploy, or a pipeline failure, in a
 *  full-height panel at the right edge with a close button. It is a dialog (`useDialog`): focus
 *  starts on the close button and stays inside, Escape closes it, and closing hands focus back to
 *  whatever opened it. */
export function DrillDown({
  project,
  prs,
  runs,
  components,
  sessionGroups,
  sessionTitles,
  selection,
  onClose,
}: {
  /** The project whose tab opened the drill-down, which a run is read under (D14). */
  project: string;
  prs: readonly DeliveryPR[];
  runs: readonly DrillDownRun[];
  components: Readonly<Record<string, DeliveryComponent>>;
  sessionGroups: readonly DeliverySessionGroup[];
  /** The delivery read's session titles: each session's stored title, or its id when none. */
  sessionTitles: Readonly<Record<string, string>>;
  selection: TimelineSelection | null;
  onClose: () => void;
}): ReactNode {
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLElement>({
    initialFocusRef: closeButtonRef,
    onClose,
    open: selection !== null,
  });
  if (selection === null) return null;

  let body: ReactNode;
  if (selection.kind === "pr") {
    const pr = prs.find((candidate) => candidate.id === selection.id);
    body =
      pr === undefined ? (
        <div>PR {selection.id} not found.</div>
      ) : (
        <PRDetail
          components={components}
          pr={pr}
          sessionGroups={sessionGroups}
          sessionTitles={sessionTitles}
        />
      );
  } else {
    const run = runs.find((candidate) => candidate.id === selection.id);
    if (run === undefined) body = <div>Run {selection.id} not found.</div>;
    else if (selection.kind === "deploy") body = <DeployDetail project={project} run={run} />;
    else body = <FailureDetail project={project} run={run} />;
  }

  return (
    <aside
      aria-label="Details"
      aria-modal="true"
      className={`fixed top-0 right-0 z-30 h-full w-96 max-w-full overflow-y-auto border-l p-4 shadow-xl ${borderStrong} ${surfaceBg}`}
      ref={dialog.containerRef}
      role="dialog"
    >
      <button
        aria-label="Close details"
        className={`float-right ${dismissButtonText}`}
        onClick={onClose}
        ref={closeButtonRef}
        type="button"
      >
        ✕
      </button>
      {body}
    </aside>
  );
}
