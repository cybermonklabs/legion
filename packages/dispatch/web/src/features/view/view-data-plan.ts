/**
 * What a `view` block gives its page, as one pure function over what the block's reads have
 * answered (dispatch://INFRA-682, "Data grants"): the entry each named data file goes to the
 * page as, the lines the reader is shown in their place, the set of grants that covers them,
 * the signature a change of versions is noticed by, and whether anything may go at all.
 *
 * It is a function rather than part of `ViewBlock.tsx`'s render body because the rule is the
 * whole of R9 and of the 16 MiB cap: which grant covers a reference, which version that grant
 * gives the page, and what a file no grant covers is shown as. The block reads two of those
 * rules before the plan runs - `versionsToWeigh`, for the version whose size and type it asks
 * for, and `contentLengthBytes`, for what that size answer weighs - and both live here, so the
 * version the block weighs is the version the plan sends. Nothing here touches the network,
 * React or the document, so every coverage case is a unit test (`view-data-plan.test.ts`)
 * rather than a browser row.
 */
import { ApiError } from "../../api/client";
import type {
  ArtifactGrant,
  DocumentReferenceSummary,
  IssueReferenceSummary,
} from "../../api/types";
import type { ViewFile } from "./view-attributes";

/** The most bytes one block's granted data may hold together; over it Dispatch sends no data. */
export const MAX_DATA_BYTES = 16 * 1024 * 1024;

/** What a summary read of one file has answered so far. */
export interface SummaryResult {
  readonly data: DocumentReferenceSummary | IssueReferenceSummary | undefined;
  readonly error: unknown;
  readonly isError: boolean;
}

/** What one version a pin holds back from the latest is: the bytes it weighs against the 16 MiB
 *  cap, and the type it is stored under, null where the row records none or the read of the
 *  artifact's versions could not be made. */
export interface PinnedVersion {
  readonly mime: string | null;
  readonly size: number;
}

/** What the read of one pinned version has answered so far, or the failure the line names. A
 *  version the summary already carries the size and the type of is never read, and its result
 *  stands unasked (`data` undefined, `isError` false). */
export interface PinnedVersionResult {
  readonly data: PinnedVersion | undefined;
  readonly error: unknown;
  readonly isError: boolean;
}

/** One file of a block as its summary read answers: the artifact's latest version, that
 *  version's size and stored type, the version the reference asks for, or why the file cannot
 *  show. Neither `cause` nor `id` means the read has not answered yet. The summary carries the
 *  latest version's size and type alone, so `size` and `mime` stand for the version the block
 *  sends only where that version is `latest`; any older version is one `pinnedVersion` read. */
export interface FileState {
  readonly cause?: string;
  readonly id?: string;
  readonly latest?: number;
  /** The type the latest version is stored under, as the summary answers it, and undefined
   *  where the artifact has no version or the row records no type. */
  readonly mime?: string;
  readonly name: string;
  /** The version the block's own reference pins with `@vN`, which a grant must cover as it is,
   *  and undefined for a reference that follows the newest version. */
  readonly pinned?: number;
  readonly ref: string;
  readonly size?: number;
  readonly version?: number;
}

/** One entry of a `dispatch-view:data` message, and the file it was built from. */
export interface DataEntry {
  /** The type the version this entry carries is stored under, as the summary or the read of
   *  that version answered it, and absent where the row records none. It is never the
   *  `Content-Type` of the read of the bytes: that answer calls every type a browser draws as
   *  a document an octet stream. */
  readonly contentType?: string;
  readonly error?: string;
  readonly id?: string;
  readonly name?: string;
  readonly ref: string;
  readonly status?: number;
  readonly text?: string;
  readonly version?: number;
}

/** One line a reader sees in place of, or beside, the frame: the reference it names, why, and
 *  whether it links to the panel where a person grants the file. */
export interface ViewLine {
  readonly cause: string;
  readonly panel?: boolean;
  readonly ref: string;
}

/** What a block sends and shows for its data files, as `planViewData` works it out. */
export interface ViewDataPlan {
  /** The grants that cover the named files, as one string: a new one is a new frame, so a page
   *  that lost a grant loses the data it held. */
  readonly cover: string;
  /** One entry per data file, in the order of the `data` attribute, as the page is given them
   *  (`dispatch-view:data`); the bytes of a covered file are read when the message goes out. */
  readonly entries: DataEntry[];
  /** Nothing goes to the page: a granted file's size is still unknown, or the sizes are in and
   *  they pass `MAX_DATA_BYTES`. */
  readonly holdData: boolean;
  /** The lines the reader sees for the data files: what could not be read, what no grant covers
   *  (with the link to the panel) and, last, the cap. */
  readonly lines: ViewLine[];
  /** The versions these entries carry, as one string: a new one is a send. */
  readonly sendSignature: string;
}

/** What a read that failed says: the status Dispatch answered, or that it could not be reached
 *  at all. One wording, for the page and for a data file alike. */
export function answerCause(status: number | undefined, what: string): string {
  const answer =
    status === undefined ? "Dispatch could not be reached" : `Dispatch answered ${status}`;
  return `${answer} for this ${what}`;
}

/** The file one summary read names, resolved to what the block needs of it. An issue's summary
 *  carries every document of that issue, so one read serves every file of one issue. */
export function resolveFile(file: ViewFile, result: SummaryResult): FileState {
  const base = { name: file.slug, pinned: file.version, ref: file.reference };
  const summary = result.data;
  if (summary === undefined) {
    // Either the read has not answered yet, or it failed and the line says how.
    if (!result.isError) {
      return base;
    }
    const status = result.error instanceof ApiError ? result.error.status : undefined;
    return { ...base, cause: answerCause(status, "file") };
  }
  const record =
    "documents" in summary
      ? summary.documents.find((document) => document.slug === file.slug)
      : summary;
  if (record === undefined) {
    return { ...base, cause: "this file is not found" };
  }
  const named = { ...base, name: record.name };
  if (record.kind === "doc") {
    return { ...named, cause: "a document is not a file a view reads" };
  }
  const latest = record.latest_version;
  const pinned = file.version;
  if (latest === null || (pinned !== undefined && pinned > latest)) {
    return {
      ...named,
      cause:
        pinned === undefined ? "this file has no version" : `this file has no version ${pinned}`,
    };
  }
  // The summary's size and type are the latest version's, and the version the block sends is
  // the grant's to decide, so the two are weighed against each other where that version is
  // known.
  return {
    ...named,
    id: record.id,
    latest,
    mime: record.mime ?? undefined,
    size: record.size ?? 0,
    version: pinned ?? latest,
  };
}

/**
 * The bytes a `HEAD` answer weighs, as its `Content-Length` says, and undefined where the
 * answer carries no size the block can weigh. `Number(null)` and `Number("")` are both 0 and
 * pass `Number.isFinite`, so an answer with no `Content-Length` weighed nothing and let any
 * version through the 16 MiB cap unread; an unknown size is a failed read
 * (dispatch://INFRA-682, R6). Only digits are a size: a negative would subtract from the total
 * a block weighs against the cap, and `Number` reads `-5` and `1e9` as numbers.
 */
export function contentLengthBytes(header: string | null): number | undefined {
  if (header === null || !/^\d+$/.test(header.trim())) {
    return undefined;
  }
  return Number(header.trim());
}

/**
 * The grant that covers one data file, and the version it gives the page: the newest version
 * under a grant without a version, the pinned version under a grant that pins one, and for a
 * reference that pins `@vN` (`state.pinned`), only a grant without a version or a grant pinned
 * to the same N (dispatch://INFRA-682, "Data grants").
 */
function coveringGrant(
  grants: readonly ArtifactGrant[],
  state: FileState
): { id: string; version: number } | undefined {
  const newest = state.version;
  const pinned = state.pinned;
  if (state.id === undefined || newest === undefined) {
    return undefined;
  }
  let best: { id: string; version: number } | undefined;
  for (const grant of grants) {
    if (grant.artifact_id !== state.id) {
      continue;
    }
    if (pinned !== undefined) {
      if (grant.version === null || grant.version === pinned) {
        return { id: grant.id, version: pinned };
      }
      continue;
    }
    if (grant.version === null) {
      return { id: grant.id, version: newest };
    }
    if (best === undefined || grant.version > best.version) {
      best = { id: grant.id, version: grant.version };
    }
  }
  return best;
}

/**
 * The version each file goes to the page at, by the grant that covers it: one entry per state,
 * in the order of `states`, undefined for the page file at index 0, which no grant covers, for
 * a file no grant covers, and for every file while the grants read has not answered.
 *
 * The block's reads of a pinned version and `planViewData` both read this one call. They have
 * to agree on a version - the plan weighs the size that was read for the entry it sends, and
 * tells the page the type that was read with it - and those reads are asked for before the
 * plan runs, so the rule cannot be written once in the order they run (dispatch://INFRA-682,
 * R6).
 */
export function versionsToWeigh(
  states: readonly FileState[],
  grants: readonly ArtifactGrant[] | undefined
): ({ id: string; version: number } | undefined)[] {
  return states.map((state, index) =>
    index === 0 || grants === undefined ? undefined : coveringGrant(grants, state)
  );
}

/**
 * One pass over a block's data files: what goes to the page, what the reader is told, how many
 * bytes the grants cover and which grants cover them.
 *
 * `states` is the block's files as their summary reads answered, the page file first, which no
 * grant covers and which this never sends. `grants` is the page file's own list, undefined
 * while that read has not answered - until it does the block shows no "not granted" line and
 * sends nothing. `pinned` is one entry per state, in the same order, carrying the bytes and the
 * stored type of the version `versionsToWeigh` names where the summary does not (an older
 * version a reference or a grant pinned). `readCauses` is why the bytes of a file did not
 * arrive on the newest send, by reference.
 */
export function planViewData(
  states: readonly FileState[],
  grants: readonly ArtifactGrant[] | undefined,
  pinned: readonly PinnedVersionResult[],
  readCauses: ReadonlyMap<string, string>
): ViewDataPlan {
  const entries: DataEntry[] = [];
  const lines: ViewLine[] = [];
  let cover = "";
  let sendSignature = "";
  let bytes = 0;
  /** A granted file whose pinned version has not answered yet: until it does the block cannot
   *  say whether its data passes the cap, nor what type the page is being given, so it sends
   *  none, as it sends none before the grants answer. */
  let weighing = false;
  // The versions the block's pinned reads were asked for, so `pinned[index]` below answers for
  // the version this entry sends.
  const covers = versionsToWeigh(states, grants);
  for (let index = 1; index < states.length; index += 1) {
    const state = states[index];
    const older = pinned[index];
    const grant = covers[index];
    let entry: DataEntry;
    const cause =
      state.cause ??
      (older.isError
        ? answerCause(older.error instanceof ApiError ? older.error.status : undefined, "file")
        : undefined);
    if (cause !== undefined) {
      lines.push({ cause, ref: state.ref });
      entry = { error: "unreadable", ref: state.ref };
    } else if (grant === undefined) {
      if (grants !== undefined) {
        lines.push({ cause: "not granted to this page", panel: true, ref: state.ref });
      }
      entry = { error: "not-granted", ref: state.ref };
    } else {
      // The version the page is sent, as the summary carries it for the latest version and the
      // read above for any other: its bytes against the cap, and the type it is stored under.
      const sent =
        grant.version === state.latest
          ? { mime: state.mime ?? null, size: state.size }
          : older.data;
      weighing ||= sent?.size === undefined;
      bytes += sent?.size ?? 0;
      entry = {
        contentType: sent?.mime ?? undefined,
        id: state.id,
        name: state.name,
        ref: state.ref,
        version: grant.version,
      };
      const failure = readCauses.get(state.ref);
      if (failure !== undefined) {
        lines.push({ cause: failure, ref: state.ref });
      }
    }
    entries.push(entry);
    cover += grant === undefined ? " none" : ` ${grant.id}`;
    sendSignature += ` ${entry.ref}@${entry.error ?? entry.version}`;
  }
  const overBudget = grants !== undefined && !weighing && bytes > MAX_DATA_BYTES;
  if (overBudget) {
    lines.push({
      cause: `the data files together are larger than ${MAX_DATA_BYTES / 1024 ** 2} MiB, so Dispatch sends no data`,
      ref: "data",
    });
  }
  return { cover, entries, holdData: weighing || overBudget, lines, sendSignature };
}
