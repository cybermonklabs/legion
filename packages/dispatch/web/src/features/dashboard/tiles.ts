import { primarySpec } from "../../api/issue-cache";
import type {
  CheckBlock,
  IssueAskRow,
  IssueCheckResults,
  IssueChild,
  IssueCoverage,
  IssueDetails,
  RequirementCoverage,
} from "../../api/types";
import {
  blockNodeId,
  checkStanding,
  namedCommit,
  nodeIdOf,
  stateAtCommit,
  statementState,
} from "../issue-page/statement-rules";
import { issueStatuses } from "../project/board-model";
import { readersTurn } from "./useWaitingItems";

/**
 * Dashboard's four tiles (dispatch://INFRA-5702/spec#b-r-tiles), each a number of a whole: the
 * statements met, the checks passed, the children done and the open questions whose turn is not
 * the reader's. Pure: each reads the reads Dashboard holds and draws nothing. Each counts by the
 * rule the issue page counts by elsewhere, so a tile and the view that draws the same nodes give
 * one number.
 */

/** What one tile counts: the entries its number counts, in the read's order, which the list
 *  under the tile draws, and the whole its bar counts them of. */
export interface Tile<Entry> {
  readonly counted: readonly Entry[];
  readonly whole: number;
}

/** The children's tile, with the number of children in each status: the parts of its bar. */
export interface ChildrenTile extends Tile<IssueChild> {
  /** Each status a child stands in, in the board's order of the statuses, then any status the
   *  board does not hold, by its name. */
  readonly statuses: readonly { readonly status: string; readonly count: number }[];
}

/** The fields of the issue read the met tile addresses a statement by. */
export type TileIssue = Pick<IssueDetails, "artifacts" | "key" | "primary_artifact_id">;

/** How many entries the list under a tile draws at a time (R6). */
export const TILE_LIST_STEP = 20;

/**
 * The statements that read Met by root R17's rule (`statementState`), of all the statements the
 * coverage read holds (R6): a statement reads Met only where every check that serves it passed at
 * the one commit the page names (`namedCommit`), each check read there by `stateAtCommit`, as the
 * Verification view reads it. A statement's checks are the checks of the results read whose
 * `serves` names it, each reference read as the tree addresses a node (`nodeIdOf`), so the spec's
 * block written in full or under the spec's own slug name the same statement, and a reference to
 * another issue's statement - an ancestor's, as Verification documents write them - serves none
 * of this issue's.
 */
export function metTile(
  issue: TileIssue,
  coverage: IssueCoverage | undefined,
  results: IssueCheckResults | undefined
): Tile<RequirementCoverage> {
  const statements = coverage?.requirements ?? [];
  const specSlug = primarySpec(issue)?.slug ?? "";
  const nodeOf = nodeIdOf(issue);
  const read = results?.checks ?? [];
  const commit = namedCommit(read);
  const checks = read.map((check) => ({
    atCommit: stateAtCommit(checkStanding(check), commit),
    serves: new Set((check.serves ?? []).map(nodeOf)),
  }));
  return {
    counted: statements.filter((statement) => {
      const node = blockNodeId(specSlug, statement.block_id);
      return statementState(checks.filter((check) => check.serves.has(node))) === "met";
    }),
    whole: statements.length,
  };
}

/** The checks that read Passed at the one commit the page names (`namedCommit`), each read there
 *  by `stateAtCommit`, of all the checks of the results read: the rule the Met tile reads, so the
 *  two tiles count by one rule (R6). A check's own row reads it on its own (`checkStanding`,
 *  decision 152), so a check whose pass is older than that commit reads Passed on its row and is
 *  no check this tile counts. */
export function passedTile(results: IssueCheckResults | undefined): Tile<CheckBlock> {
  const checks = results?.checks ?? [];
  const commit = namedCommit(checks);
  return {
    counted: checks.filter((check) => stateAtCommit(checkStanding(check), commit) === "passed"),
    whole: checks.length,
  };
}

/** The children done, of all the issue's children, with the number in each status. */
export function childrenTile(issue: Pick<IssueDetails, "children">): ChildrenTile {
  const children = issue.children ?? [];
  const present = new Set(children.map((child) => child.status));
  const known: readonly string[] = issueStatuses;
  const statuses = [
    ...known.filter((status) => present.has(status)),
    ...[...present].filter((status) => !known.includes(status)).sort(),
  ];
  return {
    counted: children.filter((child) => child.status === "done"),
    statuses: statuses.map((status) => ({
      count: children.filter((child) => child.status === status).length,
      status,
    })),
    whole: children.length,
  };
}

/** The open asks whose turn is not the reader's, of all the issue's open asks (`asks?state=open`,
 *  approval requests among them). An ask's turn is the reader's by the rule what waits on the
 *  reader reads (`readersTurn`), so no ask counts on both. */
export function questionsTile(
  asks: readonly IssueAskRow[] | undefined,
  viewer: string | undefined
): Tile<IssueAskRow> {
  const open = (asks ?? []).filter((ask) => ask.state === "open");
  const readers = new Set(readersTurn(open, viewer));
  return { counted: open.filter((ask) => !readers.has(ask)), whole: open.length };
}

/** The entries the list under a tile draws once it draws `pages` steps of `TILE_LIST_STEP`, and
 *  how many it leaves for its control to draw. */
export function tileList<Entry>(
  entries: readonly Entry[],
  pages: number
): { readonly shown: readonly Entry[]; readonly left: number } {
  const shown = entries.slice(0, pages * TILE_LIST_STEP);
  return { left: entries.length - shown.length, shown };
}
