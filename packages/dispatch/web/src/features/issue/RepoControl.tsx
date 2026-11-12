import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import {
  dangerText,
  focusVisibleRing,
  inputClasses,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ownerRepoPattern } from "../settings/classes";
import { useIssueRepo } from "./useIssueRepo";

/** `ownerRepoPattern` as a test of a whole value: the check this control makes before it writes,
 *  so a typo shows at the box rather than coming back as the server's 400 a round trip later. */
const ownerRepo = new RegExp(`^${ownerRepoPattern}$`);

/** What the box says about a value that is not `owner/name`. The server says the same thing in
 *  its own words for anything that reaches it. */
const NOT_OWNER_NAME = "Write the repository as owner/name.";

/**
 * The issue's repository in the details rail: `Repository:` and one combobox, labelled
 * `Repository of <KEY>`, holding the issue's own repository, with a `<datalist>` of the
 * repositories Dispatch maps to this issue's project. The mappings are suggestions rather than the set
 * of values: any `owner/name` may be typed, one this project maps, one another project maps, or
 * one no project maps, which is what the server takes, and that is why this is a text box with a
 * list and not a `<select>`, which cannot hold a value its options lack.
 *
 * The box shows the issue's own repository and nothing else, so the repository it resolves to -
 * its nearest ancestor's, else the project's default - is its placeholder, which is what an empty
 * box stands for; the description (`aria-describedby`, an `sr-only` span) says the same in words,
 * with `, inherited or the project's default` after it when it is not the issue's own, and
 * `No repository` when none resolves. A clear control inside the box, shown only while the issue
 * has a repository of its own, puts it back to inheriting; emptying the box does the same.
 *
 * What is typed is written when the box is left or Enter is pressed, never per keystroke, since
 * `owner/name` is only a repository once the whole of it is there; Escape restores what the issue
 * holds. A value that is not `owner/name` is refused here, at the box, with what was typed still
 * in it, and the write path (`useIssueRepo`, `useIssueFieldWrite`) is unchanged, so the optimistic
 * write, the rollback and the server's own refusal - `INVALID_ISSUE` for anything that reaches it
 * - show as they did.
 *
 * The item renders whenever the issue can carry a repository, which is always: a project that
 * maps none has nothing to suggest, and its issues still take any repository. The mappings are
 * read once the reader reaches for the control (pointer over it, a touch on it, or focus), and at
 * once while the issue resolves to no repository at all, which is the issue most likely to be
 * given one. A read that fails costs the suggestions and nothing else: the failure and a Retry
 * show beside the box, which takes a typed repository as ever.
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
  /** What the reader has typed since the box was last written, left or restored; null while the
   *  box shows the issue's own repository. */
  const [draft, setDraft] = useState<string | null>(null);
  /** Whether the draft was refused here, for not being `owner/name`. */
  const [refused, setRefused] = useState(false);
  const mappings = useQuery({
    enabled: wanted || resolvedRepo === null,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const want = () => setWanted(true);
  const descriptionId = useId();
  const suggestionsId = useId();
  const write = useIssueRepo(issueKey);
  const mapped = (mappings.data ?? [])
    .filter((mapping) => mapping.project === project)
    .map((mapping) => mapping.repo)
    .sort();
  const description =
    resolvedRepo === null
      ? "No repository"
      : repo === null
        ? `${resolvedRepo}, inherited or the project's default`
        : resolvedRepo;
  const restore = () => {
    setDraft(null);
    setRefused(false);
  };
  // The draft goes back to the issue's own repository as it is written, so the optimistic value
  // is what the box shows and a rollback puts the previous one back under it. A refused draft
  // stays in the box: it is what the reader has to correct.
  const commit = () => {
    if (draft === null) {
      return;
    }
    const typed = draft.trim();
    if (typed !== "" && !ownerRepo.test(typed)) {
      setRefused(true);
      return;
    }
    restore();
    if (typed !== (repo ?? "")) {
      write.submit(typed === "" ? null : typed);
    }
  };
  return (
    <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
      <span className="font-medium">Repository:</span>
      {/* `relative` is load-bearing: the description is `sr-only`, which is absolutely
          positioned, and with no positioned ancestor it is laid out against the page and makes it
          as wide as its own text at a phone width. */}
      <span className="relative inline-flex shrink-0 items-center">
        <input
          aria-describedby={descriptionId}
          aria-invalid={refused || undefined}
          aria-label={`Repository of ${issueKey}`}
          autoComplete="off"
          className={`min-h-11 w-[200px] rounded-sm border py-0.5 pl-1.5 font-mono text-xs outline-none focus-visible:ring-2 disabled:opacity-50 md:min-h-7 ${repo === null ? "pr-1.5" : "pr-8"} ${inputClasses(false)} ${focusVisibleRing}`}
          disabled={disabled}
          list={suggestionsId}
          onBlur={commit}
          onChange={(event) => {
            setDraft(event.target.value);
            setRefused(false);
          }}
          onFocus={want}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              commit();
            } else if (event.key === "Escape") {
              restore();
            }
          }}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          pattern={ownerRepoPattern}
          placeholder={resolvedRepo ?? "owner/name"}
          spellCheck={false}
          value={draft ?? repo ?? ""}
        />
        {repo === null ? null : (
          <button
            aria-label={`Clear the repository of ${issueKey}`}
            className={`absolute inset-y-0 right-0 inline-flex w-8 items-center justify-center rounded-sm text-base outline-none focus-visible:ring-2 disabled:opacity-50 ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
            disabled={disabled}
            onClick={() => {
              restore();
              write.submit(null);
            }}
            title="Clear it, and the issue inherits a repository again"
            type="button"
          >
            <span aria-hidden="true">×</span>
          </button>
        )}
        <datalist id={suggestionsId}>
          {mapped.map((candidate) => (
            <option key={candidate} value={candidate} />
          ))}
        </datalist>
        <span className="sr-only" id={descriptionId}>
          {description}
        </span>
      </span>
      {refused ? (
        <p className={dangerText} role="alert">
          {NOT_OWNER_NAME}
        </p>
      ) : null}
      {mappings.isError ? (
        <QueryError
          message={apiErrorMessage(
            mappings.error,
            `Could not load the repositories mapped to ${project}.`
          )}
          onRetry={() => void mappings.refetch()}
          retrying={mappings.isFetching}
        />
      ) : null}
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
