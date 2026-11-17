import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { usePageWords, useTooltips } from "../../components/tooltips";
import {
  badgeLow,
  badgePrimary,
  pageInkText,
  pageLabelChip,
  pageMutedText,
  pagePaneBg,
  pageQuietText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import {
  badgeSelectBadge,
  badgeSelectOverlay,
  badgeSelectPageBadge,
  badgeSelectPageRailWrapper,
  badgeSelectRailWrapper,
} from "./badge-select";
import { useIssueRepo } from "./useIssueRepo";

/** The name the repository picker answers to, in its words on every page: the property it sets
 *  and the issue it is on, so a voice user saying what they read presses it (WCAG 2.5.3). The
 *  new issue page's add menu reaches this picker by this name rather than drawing a second
 *  one, so a word changed here changes both sides at once. */
export function repoControlName(issueKey: string): string {
  return `Repository of ${issueKey}`;
}

/**
 * The issue's repository in the details rail: `Repository:` and a badge naming the repository the
 * issue resolves to (its own, else its nearest ancestor's, else the project's default, muted when
 * not its own; "No repository" when none resolves) with a native `<select>` laid over it, as
 * `AssigneeControl` does. The select lists Inherit first, then the repositories mapped to the
 * issue's project in Settings; Inherit clears the issue's own repository. Labelled
 * `Repository of <KEY>`. While the issue resolves to a repository the mappings are read only once
 * the reader reaches for the control (pointer over it, a touch on it, or focus). While it resolves
 * to none, they are read at once, because the item shows only when the project maps a repository:
 * until the read answers nothing shows, a project that maps none has nothing to pick and gets no
 * item, and a read that fails shows `Repository:` with the failure and a Retry, never a hidden
 * item. A failed read beside a resolved repository shows the same failure next to the picker. The
 * badge is hidden from assistive technology; the select's description names what it shows: the
 * repository, `, inherited or the project's default` when it is not the issue's own, or
 * `No repository`.
 *
 * Inside `NoTooltips` (`components/tooltips.tsx`, the new issue page) it says that page's words
 * with their marks and carries no `title`, so the words "inherited or the project's default"
 * are drawn beside the badge, where the `title` said them. There `inRow` draws it as the value
 * of a row of Details, which draws the label: no `Repository:` of its own, a line that wraps,
 * and None where no item shows.
 */
export function RepoControl({
  disabled = false,
  inRow = false,
  issueKey,
  project,
  repo,
  resolvedRepo,
}: {
  disabled?: boolean;
  /** Whether the control is the value of a row of the new issue page's Details. It acts inside
   *  `NoTooltips` alone. */
  inRow?: boolean;
  issueKey: string;
  project: string;
  repo: string | null;
  resolvedRepo: string | null;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const inDetails = page !== null && inRow;
  const [wanted, setWanted] = useState(false);
  const mappings = useQuery({
    enabled: wanted || resolvedRepo === null,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const want = () => setWanted(true);
  const descriptionId = useId();
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
  if (resolvedRepo === null && !mappings.isError && (mappings.isPending || mapped.length === 0)) {
    return page !== null && inRow ? (
      // The prototype's `.none` is the bullet grey, which no page pair registers for words
      // (2.5:1 on the paper); the page's quiet words take it.
      <span className={pageQuietText} {...page.pageChrome}>
        {page.CONTROL_LABELS.none}
      </span>
    ) : null;
  }
  // The issue's own repository stays selectable before the list loads, so the control never
  // shows a value its options lack.
  const options = repo === null || mapped.includes(repo) ? mapped : [repo, ...mapped];
  const tone = repo === null ? badgeLow : badgePrimary;
  const description =
    resolvedRepo === null
      ? "No repository"
      : repo === null
        ? `${resolvedRepo}, inherited or the project's default`
        : resolvedRepo;
  // Inside `NoTooltips` each colour is one of the new issue page's: the issue's own repository
  // is the page's label chip, one it inherits or none is the quiet words on the same ground; the
  // line is the ink in a row of Details (the prototype's `.frow .fv`) and the muted grey
  // elsewhere (its `#hdr-props`).
  const badgeTone =
    page === null
      ? `${tone.bg} ${tone.text}`
      : repo === null
        ? `${pagePaneBg} ${pageQuietText}`
        : pageLabelChip;
  const lineText = page === null ? textSecondaryOnSurface : inDetails ? pageInkText : pageMutedText;
  const shownDescription =
    page === null ? (
      description
    ) : resolvedRepo === null ? (
      page.CONTROL_LABELS.no_repository
    ) : (
      // The repository is the issue's value, and the words around it are the page's own.
      <>
        <span {...page.authorText}>{resolvedRepo}</span>
        {repo === null ? `, ${page.CONTROL_LABELS.repository_inherited}` : null}
      </>
    );
  // Nothing resolved and no list to pick from: the failure is all there is to show. Inside
  // `NoTooltips` the repository is a value, so it keeps `max-w-full` and breaks anywhere, and
  // neither its chip nor anything around it keeps its width: a 44-character name with no space
  // ran 101 px past Details' row at 1440 px.
  const picker =
    resolvedRepo === null && mappings.isError ? null : (
      <span
        className={`${page === null ? badgeSelectRailWrapper : badgeSelectPageRailWrapper} ${disabled ? "opacity-50" : ""}`}
        title={
          tooltips && repo === null && resolvedRepo !== null
            ? "Inherited or the project's default"
            : undefined
        }
      >
        <span
          aria-hidden="true"
          className={`${page === null ? badgeSelectBadge : badgeSelectPageBadge} font-mono ${badgeTone}`}
          {...(resolvedRepo === null ? page?.pageChrome : page?.authorText)}
        >
          {resolvedRepo ?? page?.CONTROL_LABELS.no_repository ?? "No repository"}
        </span>
        <select
          aria-describedby={descriptionId}
          aria-label={repoControlName(issueKey)}
          className={badgeSelectOverlay}
          disabled={disabled}
          onFocus={want}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : value);
          }}
          value={repo ?? ""}
        >
          <option value="">Inherit</option>
          {options.map((candidate) => (
            <option key={candidate} value={candidate}>
              {candidate}
            </option>
          ))}
        </select>
        <span className="sr-only" id={descriptionId} {...page?.pageChrome}>
          {shownDescription}
        </span>
      </span>
    );
  return (
    <div
      className={`flex ${page === null ? "shrink-0" : "min-w-0 max-w-full flex-wrap"} items-center gap-2 text-sm ${lineText}`}
    >
      {inDetails ? null : (
        <span className="font-medium" {...page?.pageChrome}>
          {`${page?.PROPERTY_LABELS.repository ?? "Repository"}:`}
        </span>
      )}
      {picker}
      {/* Inside `NoTooltips`, why the repository shown may not be the issue's own, in words
          where the `title` said it. Hidden from assistive technology because the select's own
          description says the same thing, and nothing is said twice. */}
      {page === null || resolvedRepo === null || repo !== null ? null : (
        <span aria-hidden="true" className="text-xs" {...page.pageChrome}>
          {page.CONTROL_LABELS.repository_inherited}
        </span>
      )}
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
