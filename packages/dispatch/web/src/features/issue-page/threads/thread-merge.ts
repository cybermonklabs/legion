/**
 * R3's and R4's merge (dispatch://INFRA-5704/spec#b-r-all, #b-r-mine): the pages of the three
 * routes, `comments`, `messages` and `asks` with `order=activity`, merged into the rows of All or
 * of Mine, 20 at a time. This is a k-way merge of sorted runs (the spec's Design): each route
 * keeps its own position, its `before`, and before each next 20 rows the merge names each route
 * that holds fewer rows than the page needs, whose next page the view reads first.
 *
 * It makes no request and holds no state: the view hands in the pages its reads hold and how
 * many rows the reader has shown, and draws what it answers.
 */
import type { AskThreadRow, CommentThreadRow, MessageThreadRow } from "@legion/contracts";

import { compareTimestamps } from "../../../lib/timestamps";
import { isViewer } from "../../refs/actor";
import { THREADS_PAGE } from "./thread-reads";
import { type ThreadRecord, threadRecords } from "./thread-records";

/** One page of a route, as INFRA-5703's page reads answer it. */
export interface ThreadPage {
  readonly threads: readonly (AskThreadRow | CommentThreadRow | MessageThreadRow)[] | null;
  readonly before: string | null;
  readonly remaining: number;
}

/** What a choice that merges routes draws. */
export interface MergedRows {
  /** The rows drawn, in All's order: the newest `last_activity_at` of a thread first, the larger
   *  id first on the same time, and a thread's records together at its place, the newest
   *  first. */
  readonly rows: readonly ThreadRecord[];
  /** The number the control under the rows says is left: the rows read and not drawn, and the
   *  threads each route's `remaining` counts, each of which gives at least one row. */
  readonly left: number;
  /** The routes, by their place in `routes`, whose next page is read before the rows that
   *  `shown` asks for past the pages drawn are drawn. While one is named, those rows wait. */
  readonly refill: readonly number[];
}

/** One record read, with the route it came from and its thread's `last_activity_at`. */
interface Row {
  readonly route: number;
  readonly at: string;
  readonly record: ThreadRecord;
}

/**
 * The rows a choice draws from `routes`, each the pages of one route read so far, first to last,
 * once the reader has asked for `shown` rows (20 when the choice opens, 20 more at each press of
 * the control under the rows). With `mine` the reader's login, only the records that hold a write
 * of the reader's are rows, and the others are left out before any row is taken (R4).
 *
 * The first 20 rows are the first 20 of the first pages' order. Before each next 20, the routes
 * whose rows read and not drawn are fewer than 20 and whose `before` names a next page are named
 * in `refill`, and until the view has read them the rows drawn are the pages drawn before, so
 * while no write lands each page draws the next 20 rows in All's order and no row is on two pages
 * (R3). A route whose first page has not answered draws no row of the choice at all.
 */
export function mergeThreads({
  mine,
  routes,
  shown,
}: {
  readonly mine?: string;
  readonly routes: readonly (readonly ThreadPage[])[];
  readonly shown: number;
}): MergedRows {
  if (routes.some((pages) => pages.length === 0)) return { left: 0, refill: [], rows: [] };
  const rows: Row[] = [];
  for (const [route, pages] of routes.entries()) {
    for (const source of pages.flatMap((page) => page.threads ?? [])) {
      for (const record of threadRecords(source)) {
        if (mine === undefined || record.writes.some((write) => isViewer(write.author, mine))) {
          rows.push({ at: source.last_activity_at, record, route });
        }
      }
    }
  }
  // A stable sort keeps each thread's records in the newest-first order `threadRecords` gives.
  rows.sort(
    (one, two) =>
      compareTimestamps(two.at, one.at) ||
      (two.record.thread === one.record.thread ? 0 : two.record.thread > one.record.thread ? 1 : -1)
  );
  // A thread that a write moved up while its route was paged can stand on two pages: it is drawn
  // once, at its newest place.
  const seen = new Set<string>();
  const read = rows.filter((row) => {
    if (seen.has(row.record.id)) return false;
    seen.add(row.record.id);
    return true;
  });
  const before = read.slice(0, Math.max(0, shown - THREADS_PAGE));
  const refill = routes.flatMap((pages, route) => {
    const last = pages.at(-1);
    if (shown <= THREADS_PAGE || last === undefined || last.before === null) return [];
    const waiting =
      read.filter((row) => row.route === route).length -
      before.filter((row) => row.route === route).length;
    return waiting < THREADS_PAGE ? [route] : [];
  });
  const drawn = refill.length === 0 ? read.slice(0, shown) : before;
  return {
    left:
      read.length -
      drawn.length +
      routes.reduce((sum, pages) => sum + (pages.at(-1)?.remaining ?? 0), 0),
    refill,
    rows: drawn.map((row) => row.record),
  };
}
