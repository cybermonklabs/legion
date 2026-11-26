import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { itemsViewQuery, projectDeliverySettingsQuery } from "../../api/queries";
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
import type { EvalRange, EvalSeriesValues } from "./chart-model";
import { EvalTitle } from "./EvalPanel";
import { NarrowedChart, narrowingOf } from "./narrowing";
import type { EvalsScope, EvalsViewState } from "./useEvalsViews";

/** The code survival eval of dispatch://INFRA-3206, which its card draws by name. */
export const CODE_SURVIVAL = "code-survival-30d";

/** How many of a code eval's absent rows its card reads: one per reason a window reads "no data"
 *  and per commit that blocks days of it, which a window holds a few of. */
const ABSENT_ROWS = 20;

/** What a code card's face names beside its values (spec R14, control 10 of
 *  dispatch://INFRA-3117/artifact/method-md): the replay's reading, not blame's, and GitHub's
 *  diff. */
const FACE_NAMES = "replay reading, not blame \u00b7 GitHub diff";

/** A kind of removal a card draws beside each group's headline share: the base of its series
 *  (`<base>:<group>`) and what the card calls it. */
export interface CodeKind {
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

/** A figure of `series` over the window, null where it states none. */
function figure(series: EvalSeriesValues | undefined, name: string): number | null {
  return series?.window?.[name] ?? null;
}

/** `count` and its noun, in the singular for 1. */
function counted(count: number, one: string, many: string): string {
  return `${count} ${count === 1 ? one : many}`;
}

/** Why a code card's window reads no number, each said on a line of its own: no code group
 *  (spec R10); a window that starts before the eval's first day; a day a force-push marked
 *  (R8); a commit GitHub did not give whole (R9); a default repository the App cannot read, or
 *  none (R11); and days that all still wait (R7). The absent rows name the commit and the
 *  repository (`asks`). */
type Reason =
  | { readonly kind: "no-group" }
  | { readonly kind: "before"; readonly day: string }
  | { readonly kind: "marked"; readonly day: string | null }
  | { readonly kind: "blocked" }
  | { readonly kind: "unreadable" }
  | { readonly kind: "no-default-repo" }
  | { readonly kind: "waiting"; readonly days: number };

/** The cause (`cols.cause`) of the eval's absent rows that name what each reason is of; null for
 *  a reason the status series states in full. */
const CAUSES: Readonly<Record<Reason["kind"], string | null>> = {
  before: null,
  blocked: "blocked",
  marked: null,
  "no-default-repo": "no_default_repo",
  "no-group": null,
  unreadable: "repo_unreadable",
  waiting: null,
};

/** What a code eval's window reads, from its `status` series (`fold.FoldStatus`): each reason it
 *  reads no number, none where it reads one; and how many of its days still wait, those after
 *  the newest day with a row (every day, where the eval holds none), null where the window
 *  states none. */
interface CodeWindow {
  readonly reasons: readonly Reason[];
  readonly waiting: number | null;
}

/** What `item`'s window reads (CodeWindow), for a project of `groups` code groups whose default
 *  repository is `defaultRepo`. A window starting before the status series' first day reads
 *  "no data before" that day, and the server states no figure of it; a window with no day row at
 *  all waits every day. */
function codeWindowOf(
  item: EvalListItem,
  window: EvalRange,
  groups: number,
  defaultRepo: string | null
): CodeWindow {
  if (groups === 0) return { reasons: [{ kind: "no-group" }], waiting: null };
  const status = item.series?.find((series) => series.series === "status");
  const since = status?.data_since ?? null;
  if (since !== null && utcDate(window.from) < since) {
    return { reasons: [{ day: since, kind: "before" }], waiting: null };
  }
  const measured = figure(status, "measured_days") ?? 0;
  const waiting = since === null ? window.days : (figure(status, "waiting_days") ?? 0);
  const reasons: Reason[] = [];
  if ((figure(status, "marked_days") ?? 0) > 0) {
    const first = figure(status, "first_marked_day");
    reasons.push({
      day: first === null ? null : new Date(first * 1000).toISOString().slice(0, 10),
      kind: "marked",
    });
  }
  if ((figure(status, "blocked_days") ?? 0) > 0) reasons.push({ kind: "blocked" });
  if ((figure(status, "repo_unreadable") ?? 0) > 0) reasons.push({ kind: "unreadable" });
  if (measured === 0 && defaultRepo === null) reasons.push({ kind: "no-default-repo" });
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
    <Link className={`hover:underline ${linkText}`} to={`/projects/${project}/settings`}>
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
      return `No data: ${counted(reason.days, "day", "days")} still waiting`;
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

/** A text column of an absent row, "" where it holds none. */
function textOf(row: EvalItemRow, name: string): string {
  const value = row.cols?.[name];
  return typeof value === "string" ? value : "";
}

/** The lines the absent rows of `reason`'s cause say (dispatch://INFRA-3206, the items route's
 *  `absent` rows), each keyed by its row: each commit that blocks days of the window with its
 *  count of files, linked to it; the repository the App cannot read; the missing default
 *  repository. None where the rows hold no row of that cause. */
function absentLines(
  rows: readonly EvalItemRow[],
  reason: Reason,
  project: string
): { key: string; line: ReactNode }[] {
  return rows
    .filter((row) => textOf(row, "cause") === CAUSES[reason.kind])
    .map((row) => {
      if (reason.kind !== "blocked") {
        return {
          key: row.row_key,
          line:
            reason.kind === "unreadable"
              ? `No data: Dispatch's GitHub App cannot read ${textOf(row, "repo")}`
              : reasonLine(reason, project),
        };
      }
      const files = row.cols?.files;
      return {
        key: row.row_key,
        line: (
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
            {typeof files === "number" ? ` (${counted(files, "file", "files")})` : ""}
          </>
        ),
      };
    });
}

/**
 * The reasons a code card's window reads "no data" that its eval's absent rows name: the commit
 * that blocks days of it, the repository the App cannot read, the missing default repository. It
 * reads them through the eval's items view of its absent rows, which it holds only while it is
 * drawn, so a window whose days are measured again lets that view go. Until the rows land the
 * card says "No data", or the reasons the status states (`stated`), and that it is reading why.
 */
function AbsentReasons({
  project,
  reasons,
  scope,
  slug,
  stated,
}: {
  project: string;
  reasons: readonly Reason[];
  scope: EvalsScope;
  slug: string;
  /** How many reasons the status states in full, drawn before these. */
  stated: number;
}): ReactNode {
  const absent = useQuery(
    itemsViewQuery({
      compare: null,
      limit: ABSENT_ROWS,
      project,
      repos: scope.repos,
      rowKind: "absent",
      slug,
      window: scope.window,
    })
  );
  if (absent.data === undefined && !absent.isError) {
    return (
      <>
        {stated === 0 ? <NoData>No data</NoData> : null}
        <p className={`text-xs ${textMutedOnSurface}`} role="status">
          Reading why…
        </p>
      </>
    );
  }
  const rows = absent.data?.items ?? [];
  return (
    <>
      {reasons.flatMap((reason) => {
        const named = absentLines(rows, reason, project);
        return named.length > 0
          ? named.map(({ key, line }) => <NoData key={key}>{line}</NoData>)
          : [<NoData key={reason.kind}>{reasonLine(reason, project)}</NoData>];
      })}
      {absent.isError ? (
        <QueryError
          error={absent.error}
          message="Could not read why the window reads no data."
          onRetry={() => void absent.refetch()}
        />
      ) : null}
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
  const seriesOf = (base: string) =>
    item.series?.find((series) => series.series === `${base}:${group}`);
  const headline = seriesOf(spec.headline);
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
            const value = figure(seriesOf(kind.base), "share");
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
          {`${lines} of ${counted(added, "line", "lines")} ${spec.lines} \u00b7 ${counted(changes, "change", "changes")}`}
        </span>
      )}
    </li>
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
 * its own, and never 0: no code group, linked to the settings; a window before the eval's first
 * day; a marked day; every day still waiting; and, read from the eval's absent rows only while
 * the window is in that state (`AbsentReasons`), a blocked commit, a repository the App cannot
 * read, or no default repository. It draws nothing until the view holds its eval.
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
  const asked = read?.reasons.filter((reason) => CAUSES[reason.kind] !== null) ?? [];
  const stated = read?.reasons.filter((reason) => CAUSES[reason.kind] === null) ?? [];
  const via = groups.reduce(
    (sum, group) =>
      sum +
      (figure(
        item.series?.find((series) => series.series === `${spec.headline}:${group}`),
        "via_repository"
      ) ?? 0),
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
      data-state={view.status}
    >
      <NarrowedChart narrowing={narrowingOf(view.facets, spec.slug, undefined)}>
        <span className={`text-xs ${textMutedOnSurface}`}>
          <EvalTitle slug={spec.slug} title={spec.title} />
        </span>
        <p className={`text-[11px] ${textMutedOnSurface}`} data-reading>
          {FACE_NAMES}
        </p>
        {read === null ? (
          settings.isError ? (
            <QueryError
              error={settings.error}
              message="Could not read this project's code groups."
              onRetry={() => void settings.refetch()}
            />
          ) : (
            <p className={`text-xs ${textMutedOnSurface}`} role="status">
              Reading the code groups…
            </p>
          )
        ) : read.reasons.length === 0 ? (
          <ul aria-label="Code groups" className="flex min-w-0 flex-col gap-1">
            {groups.map((group) => (
              <GroupLine group={group} item={item} key={group} spec={spec} />
            ))}
          </ul>
        ) : (
          <>
            {stated.map((reason) => (
              <NoData key={reason.kind}>{reasonLine(reason, project)}</NoData>
            ))}
            {asked.length === 0 ? null : (
              <AbsentReasons
                project={project}
                reasons={asked}
                scope={scope}
                slug={spec.slug}
                stated={stated.length}
              />
            )}
          </>
        )}
        {waitingLine ? (
          <p className={`text-xs ${textMutedOnSurface}`} data-waiting>
            {`${counted(read.waiting ?? 0, "day", "days")} still waiting`}
          </p>
        ) : null}
        {read === null || read.reasons.length > 0 || defaultRepo === null ? null : (
          <p className={`text-[11px] ${textMutedOnSurface}`} data-note="repository">
            {`${counted(via, "change", "changes")} with no issue reached this project through ${defaultRepo}`}
          </p>
        )}
      </NarrowedChart>
    </article>
  );
}

/** The code survival card: each group's share of the lines its changes added still on the
 *  default branch at each change's 30-day mark (`survival:<g>`, `fold.FoldSurvival`). */
const SURVIVAL: CodeCardSpec = {
  headline: "survival",
  kinds: [],
  lines: "alive",
  slug: CODE_SURVIVAL,
  title: "Code survival at 30 days",
};

/** The code survival card of Deploys and lead time (CodeEvalCard). */
export function CodeSurvivalCard({
  project,
  scope,
  view,
}: {
  project: string;
  scope: EvalsScope;
  view: EvalsViewState;
}): ReactNode {
  return <CodeEvalCard project={project} scope={scope} spec={SURVIVAL} view={view} />;
}
