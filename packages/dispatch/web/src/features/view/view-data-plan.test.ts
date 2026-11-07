/**
 * What a `view` block gives its page, case by case (dispatch://INFRA-682, "Data grants", R9 and
 * R6): every coverage case of the spec's list - a grant without a version, a grant pinned to
 * one, a reference pinned with `@vN` against each, a file no grant covers, and a page file with
 * no grants at all - and the 16 MiB cap, which only granted files count toward, the type each
 * entry carries, and the two rules the block reads before the plan runs: which version each
 * file is weighed at (`versionsToWeigh`) and what a `HEAD` answer weighs
 * (`contentLengthBytes`).
 *
 * These are the rules a browser row could only reach through a whole document, a frame and a
 * socket; they are tested here because `planViewData` is pure (`view-data-plan.ts`).
 */
import { expect, test } from "bun:test";

import { ApiError } from "../../api/client";
import type { ArtifactGrant, IssueReferenceSummary } from "../../api/types";
import type { ViewFile } from "./view-attributes";
import {
  contentLengthBytes,
  type FileState,
  MAX_DATA_BYTES,
  type PinnedVersionResult,
  planViewData,
  resolveFile,
  versionsToWeigh,
} from "./view-data-plan";

const PAGE: FileState = {
  id: "page-1",
  latest: 1,
  name: "chart.html",
  ref: "dispatch://CORE-1/artifact/chart-html",
  size: 100,
  version: 1,
};

/** One data file whose summary read answered: three versions, the newest 10 bytes. */
function dataFile(over: Partial<FileState> = {}): FileState {
  return {
    id: "data-1",
    latest: 3,
    name: "loop.json",
    ref: "dispatch://CORE-1/artifact/loop-json",
    size: 10,
    version: 3,
    ...over,
  };
}

/** A grant of `data-1` to the page file: every version of it, or the one `version` pins. */
function grantOf(version: number | null, id = "grant-1", artifact = "data-1"): ArtifactGrant {
  return {
    artifact_id: artifact,
    created_at: "2026-11-01T00:00:00Z",
    granted_by: "alice",
    id,
    name: "loop.json",
    ref: "dispatch://CORE-1/artifact/loop-json",
    version,
  };
}

/** A read of a pinned version that was never asked for, which is every file the block sends
 *  the latest version of: the summary carries that version's size and type. */
const unasked: PinnedVersionResult = { data: undefined, error: null, isError: false };

/** A read of a pinned version that answered: the bytes it weighs, and the type its row
 *  records. */
function answered(size: number, mime: string | null = null): PinnedVersionResult {
  return { data: { mime, size }, error: null, isError: false };
}

function pins(...answers: PinnedVersionResult[]): PinnedVersionResult[] {
  return [unasked, ...answers];
}

const noReadCauses: ReadonlyMap<string, string> = new Map();

function plan(
  states: FileState[],
  grants: ArtifactGrant[] | undefined,
  pinned: PinnedVersionResult[] = pins(unasked),
  readCauses: ReadonlyMap<string, string> = noReadCauses
) {
  return planViewData(states, grants, pinned, readCauses);
}

test("a reference without @vN under a grant without a version gets the newest version", () => {
  const { entries, holdData, lines } = plan([PAGE, dataFile()], [grantOf(null)]);

  expect(entries).toEqual([
    {
      id: "data-1",
      name: "loop.json",
      ref: "dispatch://CORE-1/artifact/loop-json",
      version: 3,
    },
  ]);
  expect(lines).toEqual([]);
  expect(holdData).toBe(false);
});

// The page is told what each file is, and the read of the bytes cannot tell it: a version
// stored as SVG, XML or HTML is downloaded as `application/octet-stream`, so that no uploaded
// file is drawn as a page on Dispatch's own origin. The latest version's type is the summary's;
// any other version's is the read that weighed it.
test("an entry carries the stored type of the version it sends, from the summary or the read", () => {
  const drawing = dataFile({ mime: "image/svg+xml", name: "chart.svg" });

  expect(plan([PAGE, drawing], [grantOf(null)]).entries[0].contentType).toBe("image/svg+xml");
  // Version 1 is not the latest, so its own type is the one the read of that version answered.
  expect(
    plan([PAGE, drawing], [grantOf(1)], pins(answered(40, "text/xml"))).entries[0].contentType
  ).toBe("text/xml");
  // A version whose row records no type is sent with none rather than with a type of the
  // block's invention.
  expect(plan([PAGE, dataFile()], [grantOf(null)]).entries[0].contentType).toBeUndefined();
  expect(
    plan([PAGE, drawing], [grantOf(1)], pins(answered(40))).entries[0].contentType
  ).toBeUndefined();
});

test("a reference without @vN under a grant pinned to a version gets that version", () => {
  const { cover, entries, sendSignature } = plan(
    [PAGE, dataFile()],
    [grantOf(1)],
    // Version 1 is not the latest, so its size and type are the ones that read answered.
    pins(answered(40))
  );

  expect(entries[0].version).toBe(1);
  expect(cover).toBe(" grant-1");
  expect(sendSignature).toBe(" dispatch://CORE-1/artifact/loop-json@1");
});

test("a reference without @vN under two pinned grants gets the newest version they cover", () => {
  const { entries } = plan(
    [PAGE, dataFile()],
    [grantOf(1), grantOf(2, "grant-2")],
    pins(answered(40))
  );

  expect(entries[0].version).toBe(2);
});

test("a reference pinned @v2 under a grant without a version gets version 2", () => {
  const { entries } = plan(
    [PAGE, dataFile({ pinned: 2, version: 2 })],
    [grantOf(null)],
    pins(answered(40))
  );

  expect(entries[0].version).toBe(2);
  expect(entries[0].error).toBeUndefined();
});

test("a reference pinned @v2 under a grant pinned to the same version gets version 2", () => {
  const { entries } = plan(
    [PAGE, dataFile({ pinned: 2, version: 2 })],
    [grantOf(2)],
    pins(answered(40))
  );

  expect(entries[0].version).toBe(2);
});

test("a reference pinned @v2 under a grant pinned to another version is not granted", () => {
  const { cover, entries, lines } = plan([PAGE, dataFile({ pinned: 2, version: 2 })], [grantOf(1)]);

  expect(entries).toEqual([{ error: "not-granted", ref: "dispatch://CORE-1/artifact/loop-json" }]);
  expect(lines).toEqual([
    {
      cause: "not granted to this page",
      panel: true,
      ref: "dispatch://CORE-1/artifact/loop-json",
    },
  ]);
  expect(cover).toBe(" none");
});

test("a file a grant of another file does not cover is not granted", () => {
  const { entries, lines } = plan([PAGE, dataFile()], [grantOf(null, "grant-1", "data-9")]);

  expect(entries[0].error).toBe("not-granted");
  expect(entries[0].text).toBeUndefined();
  expect(lines[0].panel).toBe(true);
});

test("a page file with no grants gets an entry for every file, all not-granted and with no text", () => {
  const other = dataFile({ id: "data-2", ref: "dispatch://CORE-1/artifact/more-json" });
  const { entries, lines, sendSignature } = plan(
    [PAGE, dataFile(), other],
    [],
    pins(unasked, unasked)
  );

  expect(entries).toEqual([
    { error: "not-granted", ref: "dispatch://CORE-1/artifact/loop-json" },
    { error: "not-granted", ref: "dispatch://CORE-1/artifact/more-json" },
  ]);
  expect(lines).toHaveLength(2);
  expect(sendSignature).toBe(
    " dispatch://CORE-1/artifact/loop-json@not-granted" +
      " dispatch://CORE-1/artifact/more-json@not-granted"
  );
});

test("a block whose grants read has not answered shows no line and names no grant", () => {
  const { cover, entries, holdData, lines } = plan([PAGE, dataFile()], undefined);

  expect(entries[0].error).toBe("not-granted");
  expect(lines).toEqual([]);
  expect(cover).toBe(" none");
  expect(holdData).toBe(false);
});

test("granted data over 16 MiB holds every file back and says so", () => {
  const second = dataFile({ id: "data-2", ref: "dispatch://CORE-1/artifact/huge-json" });
  const grants = [grantOf(null), grantOf(null, "grant-2", "data-2")];
  // Both files' newest versions are what the summary carries, so both are weighed from there.
  const under = plan([PAGE, dataFile(), second], grants, pins(unasked, unasked));
  expect(under.holdData).toBe(false);
  expect(under.lines).toEqual([]);

  const over = plan(
    [PAGE, dataFile(), { ...second, size: MAX_DATA_BYTES }],
    grants,
    pins(unasked, unasked)
  );
  expect(over.holdData).toBe(true);
  expect(over.lines).toEqual([
    {
      cause: "the data files together are larger than 16 MiB, so Dispatch sends no data",
      ref: "data",
    },
  ]);
  // The entries are built all the same; the hold is what keeps them from going out.
  expect(over.entries.map((entry) => entry.version)).toEqual([3, 3]);
});

test("a file no grant covers does not count toward 16 MiB", () => {
  const huge = dataFile({
    id: "data-2",
    ref: "dispatch://CORE-1/artifact/huge-json",
    size: MAX_DATA_BYTES + 1,
  });
  const { entries, holdData } = plan(
    [PAGE, dataFile(), huge],
    [grantOf(null)],
    pins(unasked, unasked)
  );

  expect(entries[1].error).toBe("not-granted");
  expect(holdData).toBe(false);
});

test("the version a grant pins is weighed by its own size read, not the newest version's", () => {
  // The newest version is 10 bytes; version 1, which the grant pins, is over the cap.
  const { holdData, lines } = plan(
    [PAGE, dataFile()],
    [grantOf(1)],
    pins(answered(MAX_DATA_BYTES + 1))
  );

  expect(holdData).toBe(true);
  expect(lines[0].ref).toBe("data");
});

test("a granted file whose pinned version has not answered holds the data back with no line", () => {
  const { holdData, lines } = plan([PAGE, dataFile()], [grantOf(1)], pins(unasked));

  expect(holdData).toBe(true);
  expect(lines).toEqual([]);
});

test("a read of a pinned version that failed names its status and the file goes as unreadable", () => {
  const { entries, lines } = plan(
    [PAGE, dataFile()],
    [grantOf(1)],
    pins({ data: undefined, error: new ApiError(503, { error: "no" }), isError: true })
  );

  expect(entries[0].error).toBe("unreadable");
  expect(lines).toEqual([
    {
      cause: "Dispatch answered 503 for this file",
      ref: "dispatch://CORE-1/artifact/loop-json",
    },
  ]);
});

test("a read of a granted file that failed is one line beside the file's own entry", () => {
  const { entries, lines } = plan(
    [PAGE, dataFile()],
    [grantOf(null)],
    pins(unasked),
    new Map([["dispatch://CORE-1/artifact/loop-json", "this file is not UTF-8 text"]])
  );

  expect(entries[0].version).toBe(3);
  expect(lines).toEqual([
    {
      cause: "this file is not UTF-8 text",
      ref: "dispatch://CORE-1/artifact/loop-json",
    },
  ]);
});

test("resolveFile carries the version a reference pins, which is what a grant must cover", () => {
  const file: ViewFile = {
    owner: { issue: "CORE-1" },
    reference: "dispatch://CORE-1/artifact/loop-json@v2",
    route: { kind: "artifact", key: "CORE-1", slug: "loop-json", version: 2 },
    slug: "loop-json",
    version: 2,
  };
  const summary: IssueReferenceSummary = {
    documents: [
      {
        id: "data-1",
        kind: "file",
        latest_version: 3,
        mime: "application/json",
        name: "loop.json",
        primary: false,
        size: 10,
        slug: "loop-json",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "todo",
    title: "Kernel boot loop",
  };

  const state = resolveFile(file, { data: summary, error: null, isError: false });

  expect(state.pinned).toBe(2);
  expect(state.version).toBe(2);
  expect(state.latest).toBe(3);

  // A grant pinned to the newest version does not cover a reference pinned to an older one.
  expect(plan([PAGE, state], [grantOf(3)]).entries[0].error).toBe("not-granted");
  expect(plan([PAGE, state], [grantOf(2)], pins(answered(7))).entries[0].version).toBe(2);
});

test("versionsToWeigh names the version the plan sends, and nothing for the page file", () => {
  const data = dataFile();
  const grants = [grantOf(1)];

  const weighed = versionsToWeigh([PAGE, data], grants);

  // The read of a pinned version asks for this version, and the plan sends it: one call
  // answers both, so the block cannot weigh a version other than the one it sends.
  expect(weighed).toEqual([undefined, { id: "grant-1", version: 1 }]);
  expect(plan([PAGE, data], grants, pins(answered(40))).entries[0].version).toBe(
    weighed[1]?.version
  );
  // Before the grants read answers there is no version to weigh, and nothing is asked for.
  expect(versionsToWeigh([PAGE, data], undefined)).toEqual([undefined, undefined]);
});

test("a HEAD answer with no Content-Length weighs nothing readable, not nothing", () => {
  expect(contentLengthBytes("40")).toBe(40);
  expect(contentLengthBytes("0")).toBe(0);
  // `Number(null)` and `Number("")` are both 0, which would pass the 16 MiB cap unweighed.
  expect(contentLengthBytes(null)).toBeUndefined();
  expect(contentLengthBytes("")).toBeUndefined();
  expect(contentLengthBytes("  ")).toBeUndefined();
  expect(contentLengthBytes("a lot")).toBeUndefined();
  // `Number` reads both of these, and a negative would subtract from the total a block weighs.
  expect(contentLengthBytes("-5")).toBeUndefined();
  expect(contentLengthBytes("1e9")).toBeUndefined();
});
