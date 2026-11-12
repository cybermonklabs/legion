import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import {
  badgeLow,
  badgePrimary,
  focusRing,
  inputClasses,
  secondaryButtonCompact,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { badgeSelectBadge } from "./badge-select";
import { useIssueRepo } from "./useIssueRepo";

/**
 * The issue's repository in the details rail: `Repository:`, a badge naming the repository the
 * issue resolves to (its own, else its nearest ancestor's, else the project's default, muted when
 * not its own; "No repository" when none resolves), a text box holding the issue's own repository
 * and an `Inherit` button that clears it. The text box takes any `owner/name`, whichever project
 * maps that repository and whether any project maps it, and its `<datalist>` offers the
 * repositories the issue's project maps, so a mapped one is one keystroke away. It is
 * labelled `Repository of <KEY>`; the value is written on Enter or on leaving the box, and Escape
 * puts back what the issue holds. The item shows whenever the issue can carry a repository,
 * including in a project that maps none. While the issue resolves to a repository the mappings are
 * read only once the reader reaches for the control (pointer over it, a touch on it, or focus),
 * and while it resolves to none they are read at once. A read that fails shows the failure and a
 * Retry beside the box, which still takes a repository. The badge is hidden from assistive
 * technology; the box's description names what the badge shows: the repository,
 * `, inherited or the project's default` when it is not the issue's own, or `No repository`.
 */
export function RepoControl({
  disabled = false,
  issueKey,
  project,
  repo,
  resolvedRepo,
}: {
  disabled?: boolean;
  issueKey: string;
  project: string;
  repo: string | null;
  resolvedRepo: string | null;
}): ReactNode {
  const [wanted, setWanted] = useState(false);
  // What the reader has typed and not yet written; `null` while the box shows the issue's own
  // repository, so an answer, a rollback or another tab's change moves the box with it.
  const [draft, setDraft] = useState<string | null>(null);
  const mappings = useQuery({
    enabled: wanted || resolvedRepo === null,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const want = () => setWanted(true);
  const descriptionId = useId();
  const listId = useId();
  const write = useIssueRepo(issueKey);
  const mapped = (mappings.data ?? [])
    .filter((mapping) => mapping.project === project)
    .map((mapping) => mapping.repo)
    .sort();
  const loadFailure = mappings.isError ? (
    <QueryError
      message={apiErrorMessage(
        mappings.error,
        `Could not load the repositories mapped to ${project}.`
      )}
      onRetry={() => void mappings.refetch()}
      retrying={mappings.isFetching}
    />
  ) : null;
  const shown = draft ?? repo ?? "";
  const commit = () => {
    const value = shown.trim();
    setDraft(null);
    if (value === (repo ?? "")) {
      return;
    }
    write.submit(value === "" ? null : value);
  };
  const tone = repo === null ? badgeLow : badgePrimary;
  const description =
    resolvedRepo === null
      ? "No repository"
      : repo === null
        ? `${resolvedRepo}, inherited or the project's default`
        : resolvedRepo;
  return (
    <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
      <span className="font-medium">Repository:</span>
      {/* `relative`: the description below is `sr-only`, which is absolutely positioned, so
          without a positioned ancestor it is laid out against the page and widens it. */}
      <span
        className={`relative flex shrink-0 items-center gap-1 ${disabled ? "opacity-50" : ""}`}
        title={
          repo === null && resolvedRepo !== null ? "Inherited or the project's default" : undefined
        }
      >
        <span
          aria-hidden="true"
          className={`${badgeSelectBadge} font-mono ${tone.bg} ${tone.text}`}
        >
          {resolvedRepo ?? "No repository"}
        </span>
        <input
          aria-describedby={descriptionId}
          aria-label={`Repository of ${issueKey}`}
          className={`min-h-11 w-40 rounded-sm border px-1.5 py-0.5 font-mono text-xs md:min-h-7 ${inputClasses(false)} focus:ring-2 focus:outline-none ${focusRing}`}
          disabled={disabled}
          list={listId}
          onBlur={commit}
          onChange={(event) => setDraft(event.target.value)}
          onFocus={want}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              commit();
            }
            if (event.key === "Escape") {
              setDraft(null);
            }
          }}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          placeholder="Inherit"
          value={shown}
        />
        <datalist id={listId}>
          {mapped.map((candidate) => (
            <option key={candidate} value={candidate} />
          ))}
        </datalist>
        <button
          className={secondaryButtonCompact}
          disabled={disabled || (repo === null && shown === "")}
          // The press must not blur the box first: that would write the typed value and then
          // clear it, two writes where the reader asked for one.
          onMouseDown={(event) => event.preventDefault()}
          onClick={() => {
            setDraft(null);
            if (repo !== null) {
              write.submit(null);
            }
          }}
          type="button"
        >
          Inherit
        </button>
        <span className="sr-only" id={descriptionId}>
          {description}
        </span>
      </span>
      {loadFailure}
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the repository of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </div>
  );
}
