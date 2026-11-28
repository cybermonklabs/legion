import { type UseQueryResult, useQueries, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { EVALS_VIEW_LIMIT, itemsViewQuery, projectDeliverySettingsQuery } from "../../api/queries";
import type { EvalItemRow, EvalListItem } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  linkText,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { pct } from "../delivery/lib/format";
import { utcDate } from "../delivery/lib/window";
import { buildProjectPath } from "../refs/routes";
import type { EvalRange, EvalSeriesValues } from "./chart-model";
import { plural } from "./charts/graph";
import { EvalTitle } from "./EvalPanel";
import { textOf } from "./EvalRows";
import { NarrowedChart, narrowingOf } from "./narrowing";
import { type EvalsScope, type EvalsViewState, laterOffsets } from "./useEvalsViews";

/** What a code card's face names beside its values (spec R14, control 10 of
 *  dispatch://INFRA-3117/artifact/method-md): the replay's reading, not blame's, and GitHub's
 *  diff. */
const FACE_NAMES = "replay reading, not blame \u00b7 GitHub diff";

/** A kind of removal a card draws beside each group's headline share: the base of its series
 *  (`<base>:<group>`) and what the card calls it. */
interface CodeKind {
  readonly base: string;
  readonly label: string;
}

/** One code card: its eval, its title, the base of each group's headline series, the kinds it
 *  draws beside the headline, and what its line counts say of the lines (`alive` at their marks,
 *  or `removed` in the headline's kind). */
export interface CodeCardSpec {
  readonly slug: string;
  readonly title: string;
  readonly headline: string;
  readonly kinds: readonly CodeKind[];
  readonly lines: string;
}

/** `item`'s series `name`, undefined where it holds none. */
function seriesOf(item: EvalListItem, name: string): EvalSeriesValues | undefined {
  return item.series?.find((series) => series.series === name);
}

/** A figure of `series` over the window, null where it states none. */
function figure(series: EvalSeriesValues | undefined, name: string): number | null {
  return series?.window?.[name] ?? null;
}

/** Why a code card's window reads no number, each said on a line of its own: no code group
 *  (spec R10); a window that starts before the eval's first day; a day a force-push marked
 *  (R8); a commit GitHub did not give whole (R9); a default repository the App cannot read, or
 *  none where the eval holds no day row (R11); and days that all still wait (R7), those of a
 *  window that holds no day row. The absent rows name the commit and the repository
 *  (`AbsentReasons`). */
type Reason =
  | { readonly kind: "no-group" }
  | { readonly kind: "before"; readonly day: string }
  | { readonly kind: "marked"; readonly day: string | null }
  | { readonly kind: "blocked" }
  | { readonly kind: "unreadable" }
  | { readonly kind: "no-default-repo" }
  | { readonly kind: "waiting"; readonly days: number };

/** The causes (`cols.cause`) of the eval's absent rows that the card reads. */
type AbsentCause = "blocked" | "repo_unreadable";

/** The cause (`cols.cause`) of the eval's absent rows that name what each reason is of, null for
 *  a reason the settings or the status series state in full, drawn as its line. The status names
 *  a repository the App cannot read only through the window's day rows, so a window that holds
 *  none reads its days as still waiting only where its absent rows name no such repository; where
 *  they name one, its line takes the place of the days' (R11). The card reads it only through
 *  `causeOf`. */
const CAUSES: Readonly<Record<Reason["kind"], AbsentCause | null>> = {
  before: null,
  blocked: "blocked",
  marked: null,
  "no-default-repo": null,
  "no-group": null,
  unreadable: "repo_unreadable",
  waiting: "repo_unreadable",
};

/** The one statement of where the card reads `reason`, for a project whose default repository is
 *  `defaultRepo`: the cause of the absent rows that name what it is of (CAUSES), or null where the
 *  settings or the status state it in full. A project with no default repository reads no absent
 *  row for days still waiting: the items route answers a repo_unreadable row only of a stored
 *  default repository, and a day row reads its repository unreadable only where the project has
 *  one (R11), so no row could take the days' place, and the status states them in full. */
function causeOf(reason: Reason, defaultRepo: string | null): AbsentCause | null {
  return reason.kind === "waiting" && defaultRepo === null ? null : CAUSES[reason.kind];
}

/** What a code eval's window reads, from its `status` series (`fold.FoldStatus`): each reason it
 *  reads no number, none where it reads one; and how many of its days still wait, those after
 *  the newest day with a row (every day, where the eval holds none), null where the window
 *  states none or names a default repository the App cannot read, or where the eval holds no day
 *  row of a project with no default repository, since no run then measures them for the project
 *  (R7, R11). */
interface CodeWindow {
  readonly reasons: readonly Reason[];
  readonly waiting: number | null;
}

/** What `item`'s window reads (CodeWindow), for a project of `groups` code groups whose default
 *  repository is `defaultRepo`. A project with no default repository takes its days from each
 *  repository holding a change of its issues, so only an eval that holds no day row of it reads
 *  "no default repository"; one that holds day rows reads its window as any other (R11). A window
 *  starting before the status series' first day reads "no data before" that day, and the server
 *  states no figure of it; a window with no day row at all waits every day, unless its absent
 *  rows name a repository the App cannot read (causeOf). */
function codeWindowOf(
  item: EvalListItem,
  window: EvalRange,
  groups: number,
  defaultRepo: string | null
): CodeWindow {
  if (groups === 0) return { reasons: [{ kind: "no-group" }], waiting: null };
  const status = seriesOf(item, "status");
  const since = status?.data_since ?? null;
  if (since === null && defaultRepo === null) {
    return { reasons: [{ kind: "no-default-repo" }], waiting: null };
  }
  if (since !== null && utcDate(window.from) < since) {
    return { reasons: [{ day: since, kind: "before" }], waiting: null };
  }
  const measured = figure(status, "measured_days") ?? 0;
  const reasons: Reason[] = [];
  if ((figure(status, "marked_days") ?? 0) > 0) {
    const first = figure(status, "first_marked_day");
    reasons.push({
      day: first === null ? null : new Date(first * 1000).toISOString().slice(0, 10),
      kind: "marked",
    });
  }
  if ((figure(status, "blocked_days") ?? 0) > 0) reasons.push({ kind: "blocked" });
  if ((figure(status, "repo_unreadable") ?? 0) > 0) {
    reasons.push({ kind: "unreadable" });
    return { reasons, waiting: null };
  }
  const waiting = since === null ? window.days : (figure(status, "waiting_days") ?? 0);
  if (reasons.length === 0 && measured === 0) reasons.push({ days: waiting, kind: "waiting" });
  return { reasons, waiting };
}

/** A "no data" line of a card (`data-no-data`). */
function NoData({ children }: { children: ReactNode }): ReactNode {
  return (
    <p className={`text-sm ${textMutedOnSurface}`} data-no-data>
      {children}
    </p>
  );
}

/** A link to the project's settings page, where its default repository and its code groups are
 *  set, in words that say what to do there. */
function SettingsLink({ project, words }: { project: string; words: string }): ReactNode {
  return (
    <Link
      className={`hover:underline ${linkText}`}
      to={buildProjectPath({ kind: "settings", project })}
    >
      {words}
    </Link>
  );
}

/** A reason the status series states in full, as its line. */
function reasonLine(reason: Reason, project: string): ReactNode {
  switch (reason.kind) {
    case "no-group":
      return (
        <>
          No data: <SettingsLink project={project} words="name this project's code groups" />
        </>
      );
    case "before":
      return `No data before ${reason.day}`;
    case "marked":
      return reason.day === null
        ? "No data: rows marked after a force-push"
        : `No data: rows marked after a force-push found on ${reason.day}`;
    case "waiting":
      return `No data: ${plural(reason.days, "day")} still waiting`;
    case "blocked":
      return "No data: GitHub did not give the whole patch of a commit";
    case "unreadable":
      return "No data: Dispatch's GitHub App cannot read this project's default repository";
    case "no-default-repo":
      return (
        <>
          No data: <SettingsLink project={project} words="set this project's default repository" />
        </>
      );
  }
}

/** What a blocking commit's absent row says of its files (R9): how many GitHub listed with no
 *  patch (`cols.files`); where its files[] listing may not hold the whole commit (`cols.unlisted`:
 *  it listed GitHub's limit of 3,000 files, named no stats, or its entries change fewer lines than
 *  the commit's stats count), at least that many, or, where it listed none with no patch, that the
 *  list may not hold the whole commit, in words that claim no cause, since files[] can list every
 *  file and its lines still fall short where GitHub serves a text file with no patch and changes 0;
 *  nothing where the row states no count. */
function blockedFiles(row: EvalItemRow): string {
  const files = row.cols?.files;
  if (row.cols?.unlisted !== true) {
    return typeof files === "number" ? ` (${plural(files, "file")})` : "";
  }
  return typeof files === "number" && files > 0
    ? ` (at least ${plural(files, "file")})`
    : " (GitHub's file list may not hold the whole commit)";
}

/** The line of an absent row, of its own `cause`: the commit that blocks days of the window,
 *  linked to it, with what the row says of its files (`blockedFiles`); the repository the App
 *  cannot read. */
function absentLine(row: EvalItemRow, cause: AbsentCause): ReactNode {
  switch (cause) {
    case "blocked":
      return (
        <>
          No data: GitHub did not give the whole patch of commit{" "}
          <a
            className={`hover:underline ${linkText}`}
            href={row.href}
            rel="noreferrer noopener"
            target="_blank"
          >
            {textOf(row, "sha").slice(0, 7)}
          </a>
          {blockedFiles(row)}
        </>
      );
    case "repo_unreadable":
      return `No data: Dispatch's GitHub App cannot read ${textOf(row, "repo")}`;
    default: {
      // A cause with no case of its own fails the typecheck here, not as an empty line.
      const unread: never = cause;
      return unread;
    }
  }
}

/** The lines the absent rows of `reason`'s cause say (causeOf, for a project whose default
 *  repository is `defaultRepo`; dispatch://INFRA-3206, the items route's `absent` rows), each
 *  keyed by its row and drawn from the row's own cause (`absentLine`): the reason only selects the
 *  rows. None where the rows hold no row of that cause. */
function absentLines(
  rows: readonly EvalItemRow[],
  reason: Reason,
  defaultRepo: string | null
): { key: string; line: ReactNode }[] {
  const wanted = causeOf(reason, defaultRepo);
  return rows.flatMap((row) => {
    const cause = textOf(row, "cause");
    return cause === wanted ? [{ key: row.row_key, line: absentLine(row, cause) }] : [];
  });
}

/**
 * The reasons a code card's window reads "no data" that its eval's absent rows name: the commit
 * that blocks days of it, and the repository the App cannot read, which nothing else names where
 * the window holds no day row. It reads them through the eval's items view of its absent rows,
 * which it holds only while it is drawn, so a window whose days are measured again lets that view
 * go. It reads every page of that view, EVALS_VIEW_LIMIT rows each (the items route's most), since
 * the rows come by day and a force-push marks a row per repository and day, which can fill a page
 * before the blocking commit's row. Until every page lands the card says "No data", or the
 * reasons the status states (`stated`), and that it is reading why.
 */
function AbsentReasons({
  defaultRepo,
  project,
  reasons,
  scope,
  slug,
  stated,
}: {
  defaultRepo: string | null;
  project: string;
  reasons: readonly Reason[];
  scope: EvalsScope;
  slug: string;
  /** How many reasons the status states in full, drawn before these. */
  stated: number;
}): ReactNode {
  const view = (offset: number) =>
    itemsViewQuery({
      compare: null,
      limit: EVALS_VIEW_LIMIT,
      offset,
      project,
      repos: scope.repos,
      rowKind: "absent",
      slug,
      window: scope.window,
    });
  const first = useQuery(view(0));
  const rest = useQueries({
    queries: laterOffsets(first.data?.total ?? 0).map((offset) => view(offset)),
  });
  const pages = [first, ...rest];
  if (pages.some((page) => page.data === undefined && !page.isError)) {
    return (
      <>
        {stated === 0 ? <NoData>No data</NoData> : null}
        <p className={`text-xs ${textMutedOnSurface}`} role="status">
          Reading why…
        </p>
      </>
    );
  }
  const rows = pages.flatMap((page) => page.data?.items ?? []);
  const refused = pages.filter((page) => page.isError);
  return (
    <>
      {reasons.flatMap((reason) => {
        const named = absentLines(rows, reason, defaultRepo);
        return named.length > 0
          ? named.map(({ key, line }) => <NoData key={key}>{line}</NoData>)
          : [<NoData key={reason.kind}>{reasonLine(reason, project)}</NoData>];
      })}
      {refused.length === 0 ? null : (
        <QueryError
          error={refused[0]?.error}
          message="Could not read why the window reads no data."
          onRetry={() => {
            for (const page of refused) void page.refetch();
          }}
        />
      )}
    </>
  );
}

/**
 * One group's line on a code card (`data-group`), in four groups, in this order: its name; its
 * headline share (`data-share`), "no data" where its changes added no line; the kinds of removal
 * the card draws beside it (`data-kind`), each with its share; and its counts (`data-counts`):
 * the lines alive or removed of the lines its changes added, and its changes.
 */
function GroupLine({
  group,
  item,
  spec,
}: {
  group: string;
  item: EvalListItem;
  spec: CodeCardSpec;
}): ReactNode {
  const headline = seriesOf(item, `${spec.headline}:${group}`);
  const share = figure(headline, "share");
  const lines = figure(headline, "lines");
  const added = figure(headline, "added");
  const changes = figure(headline, "changes");
  return (
    <li
      className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs"
      data-group={group}
    >
      <span className={`min-w-0 truncate ${textPrimaryOnSurface}`} title={group}>
        {group}
      </span>
      <span
        className={
          share === null ? textMutedOnSurface : `text-[15px] font-semibold ${textPrimaryOnSurface}`
        }
        data-share
      >
        {share === null ? "no data" : pct(share)}
      </span>
      {spec.kinds.length === 0 ? null : (
        <span className={`flex flex-wrap gap-x-2 ${textMutedOnSurface}`}>
          {spec.kinds.map((kind) => {
            const value = figure(seriesOf(item, `${kind.base}:${group}`), "share");
            return (
              <span data-kind={kind.base} key={kind.base}>
                {`${kind.label} ${value === null ? "no data" : pct(value)}`}
              </span>
            );
          })}
        </span>
      )}
      {lines === null || added === null || changes === null ? null : (
        <span className={textMutedOnSurface} data-counts>
          {`${lines} of ${plural(added, "line")} ${spec.lines} \u00b7 ${plural(changes, "change")}`}
        </span>
      )}
    </li>
  );
}

/**
 * What a code card draws under its face names: a line saying it reads the project's code groups,
 * or the refusal of that read, until they land; each group's line (`GroupLine`) where the window
 * reads a number; and else each reason it reads none, those the settings or the status state in
 * full first, then those its absent rows name more of (`AbsentReasons`), mounted only while the
 * window reads a blocked commit, a repository the App cannot read, or no day row of a project
 * with a default repository (causeOf).
 */
function CardBody({
  defaultRepo,
  groups,
  item,
  project,
  read,
  scope,
  settings,
  spec,
}: {
  defaultRepo: string | null;
  groups: readonly string[];
  item: EvalListItem;
  project: string;
  read: CodeWindow | null;
  scope: EvalsScope;
  settings: UseQueryResult<unknown>;
  spec: CodeCardSpec;
}): ReactNode {
  if (read === null) {
    return settings.isError ? (
      <QueryError
        error={settings.error}
        message="Could not read this project's code groups."
        onRetry={() => void settings.refetch()}
      />
    ) : (
      <p className={`text-xs ${textMutedOnSurface}`} role="status">
        Reading the code groups…
      </p>
    );
  }
  if (read.reasons.length === 0) {
    return (
      <ul aria-label="Code groups" className="flex min-w-0 flex-col gap-1">
        {groups.map((group) => (
          <GroupLine group={group} item={item} key={group} spec={spec} />
        ))}
      </ul>
    );
  }
  const asked = read.reasons.filter((reason) => causeOf(reason, defaultRepo) !== null);
  const stated = read.reasons.filter((reason) => causeOf(reason, defaultRepo) === null);
  return (
    <>
      {stated.map((reason) => (
        <NoData key={reason.kind}>{reasonLine(reason, project)}</NoData>
      ))}
      {asked.length === 0 ? null : (
        <AbsentReasons
          defaultRepo={defaultRepo}
          project={project}
          reasons={asked}
          scope={scope}
          slug={spec.slug}
          stated={stated.length}
        />
      )}
    </>
  );
}

/**
 * A code card of Deploys and lead time (dispatch://INFRA-3206 R7 to R11, R14), drawn from the
 * delivery view the section reads, so a measurement that moves the eval's day rows reaches it in
 * that view's delta, with no read of its own. Its title (`EvalTitle`) opens the number's panel,
 * with the eval's method word for word. On its face, beside the values: the reading and the diff
 * (`data-reading`); then each of the project's code groups, in the order its delivery settings
 * name them, as a line (`GroupLine`); how many days of the window still wait (`data-waiting`);
 * and the note counting the changes with no issue that reached the project through its default
 * repository (`data-note`). A window that reads no number says why instead, each reason a line of
 * its own, and never 0 (`CardBody`): no code group, linked to the settings; a window before the
 * eval's first day; a marked day; every day still waiting, from the status alone for a project
 * with no default repository, else where the eval's absent rows name no repository the App cannot
 * read; a blocked commit or a repository the App cannot read, read from the eval's absent rows
 * only while the window is in that state (`AbsentReasons`); and no default repository, where the
 * eval holds no day row. A window that names a repository the App cannot read, or an eval that
 * holds no day row of a project with no default repository, counts no day as waiting (R7). It
 * draws nothing until the view holds its eval.
 */
export function CodeEvalCard({
  project,
  scope,
  spec,
  view,
}: {
  project: string;
  scope: EvalsScope;
  spec: CodeCardSpec;
  view: EvalsViewState;
}): ReactNode {
  const settings = useQuery(projectDeliverySettingsQuery(project));
  const item = view.items.find((candidate) => candidate.definition.slug === spec.slug);
  const answer = view.answer;
  if (item === undefined || answer === undefined) return null;
  const groups = (settings.data?.code_groups ?? []).map((group) => group.name);
  const defaultRepo = settings.data?.default_repo ?? null;
  const read =
    settings.data === undefined
      ? null
      : codeWindowOf(item, answer.window, groups.length, defaultRepo);
  const via = groups.reduce(
    (sum, group) =>
      sum + (figure(seriesOf(item, `${spec.headline}:${group}`), "via_repository") ?? 0),
    0
  );
  const waitingLine =
    read !== null &&
    read.waiting !== null &&
    read.waiting > 0 &&
    !read.reasons.some((reason) => reason.kind === "waiting");
  return (
    <article
      aria-label={spec.title}
      className={`flex min-w-0 flex-col gap-1 rounded border px-2.5 py-2 ${surfaceBg} ${borderDefault}`}
      data-code-card={spec.slug}
      data-eval={spec.slug}
    >
      <NarrowedChart narrowing={narrowingOf(view.facets, spec.slug, undefined)}>
        <span className={`text-xs ${textMutedOnSurface}`}>
          <EvalTitle slug={spec.slug} title={spec.title} />
        </span>
        <p className={`text-[11px] ${textMutedOnSurface}`} data-reading>
          {FACE_NAMES}
        </p>
        <CardBody
          defaultRepo={defaultRepo}
          groups={groups}
          item={item}
          project={project}
          read={read}
          scope={scope}
          settings={settings}
          spec={spec}
        />
        {waitingLine ? (
          <p className={`text-xs ${textMutedOnSurface}`} data-waiting>
            {`${plural(read.waiting ?? 0, "day")} still waiting`}
          </p>
        ) : null}
        {read === null || read.reasons.length > 0 || defaultRepo === null ? null : (
          <p className={`text-[11px] ${textMutedOnSurface}`} data-note="repository">
            {`${plural(via, "change")} with no issue reached this project through ${defaultRepo}`}
          </p>
        )}
      </NarrowedChart>
    </article>
  );
}
