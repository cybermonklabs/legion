/** The pure parts of the recorder of the rows the base draws (`e2e/tools/issue-page-load-record.ts`)
 *  and of the comparison `e2e/issue-page-load-same.e2e.ts` makes with its record, on small
 *  hand-written readings, so each rule is held without a browser or a stack.
 *
 *  - The recorder names each row by what the seed returned and writes each day the page draws as
 *    `{day}`; it refuses a record that still holds an id the server minted (a UUID, an issue's
 *    key the seed did not return) or an instant, and two rows with one key under one parent.
 *  - Each recorded row holds its key, its address, its words, its links by `href` in order, its
 *    tab stops and its accessible tree, each named as the row's key is, and the comparison reads
 *    each of them.
 *  - Two seeds of `seedLargestShape` are not the same data where the seed writes several things
 *    at once: the server gives those writes their times and keys in the order it took them, which
 *    another run does not repeat. So the comparison lets exactly two things differ: the order of
 *    two rows the server orders by the same measure (when it took the write, or the number of the
 *    key it gave) where the two seeds' server order of them differs, and, under a parent whose
 *    rows past a page are held back, which of its server-ordered children stand at the page's
 *    edge, with the rows under them, while the number drawn and the paging control's words are
 *    the same. Every other difference is one.
 */
import { expect, test } from "bun:test";

import {
  expectRowsNamedApart,
  type Naming,
  type PageRow,
  type PageView,
  type RecordedRow,
  type ViewRecord,
  viewDifferences,
  viewRecord,
  widthRecord,
} from "../tools/issue-page-load-record";

/** The issue's key, and the ids the server minted for a post and its reply. */
const ISSUE = "LAY-1";
const POST = "6f1c2b3a-0d4e-4f5a-8b6c-7d8e9f0a1b2c";
const REPLY = "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f";
/** An ask's id no name is known for. */
const ASK = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d";

/** What the seed returned (the issue's key and the post's id) and when the server took the post
 *  and its reply. */
const NAMING: Naming = {
  issue: ISSUE,
  names: new Map([
    [ISSUE, "key"],
    [POST, "posts[0]"],
  ]),
  order: new Map([
    [POST, { at: "2026-11-26T08:00:00.000001Z", by: "made" }],
    [REPLY, { at: "2026-11-26T08:00:01.5Z", by: "made" }],
  ]),
};

/** A row as the browser reads it, with nothing in it but `fields`. */
function row(fields: Partial<PageRow>): PageRow {
  return {
    aria: "",
    child: null,
    hidden: [],
    id: null,
    level: 1,
    links: [],
    rowKey: null,
    tabStops: [],
    type: null,
    words: "",
    ...fields,
  };
}

/** A view of `rows` with the focus on the row `focus` and no paging control. */
function view(rows: readonly PageRow[], focus: number | null = null): PageView {
  return { focus, keys: null, paging: [], rows };
}

/** A post's row: its link to the issue and to itself, two tab stops, and the day it was made. */
const POST_ROW = row({
  aria: [
    '- treeitem "The tree read draws every node":',
    '  - link "LAY-1":',
    "    - /url: /issues/LAY-1",
    "  - text: alice · 26 Nov",
  ].join("\n"),
  id: POST,
  links: ["/issues/LAY-1", `/issues/LAY-1/comments/${POST}`],
  rowKey: `${ISSUE}>${POST}`,
  tabStops: [
    { name: "The tree read draws every node", role: "treeitem", tag: "div" },
    { name: "LAY-1", role: null, tag: "a" },
  ],
  type: "comment",
  words: "The tree read draws every node, as LAY-1 does. alice · 26 Nov",
});

/** The reply under it, which the seed returned no stable id of. */
const REPLY_ROW = row({
  aria: '- treeitem "The outline keeps it"',
  id: REPLY,
  level: 2,
  rowKey: `${ISSUE}>${POST}>${REPLY}`,
  type: "comment",
  words: "The outline keeps it. bob · 26 Nov",
});

test("the recorder refuses a server id or an instant, and two rows with one key and one parent", () => {
  // What the seed returned is written by its name, and the day the page draws as `{day}`.
  const named = viewRecord(view([POST_ROW]), NAMING, "Conversation at 1440 px");
  expect(named.rows.map((one) => one.words)).toEqual([
    "The tree read draws every node, as {key} does. alice · {day}",
  ]);
  expect(() => widthRecord(1440, { conversation: named })).not.toThrow();

  // An ask whose id the seed did not return, linked from the post's words.
  const minted = viewRecord(
    view([{ ...POST_ROW, links: [...POST_ROW.links, `/issues/${ISSUE}/asks/${ASK}`] }]),
    NAMING,
    "Conversation at 1440 px"
  );
  expect(() => widthRecord(1440, { conversation: minted })).toThrow(
    `the record.views.conversation.rows[0].links[2] holds a UUID, ${ASK}, which belongs to this run alone`
  );

  // A child's key the server numbered and the seed did not return.
  const numbered = viewRecord(
    view([{ ...POST_ROW, words: "The tree read draws every node, as LAY-7 does." }]),
    NAMING,
    "Conversation at 1440 px"
  );
  expect(() => widthRecord(390, { conversation: numbered })).toThrow(
    "the record.views.conversation.rows[0].words holds an issue's key, LAY-7"
  );

  // An instant, in a tab stop's name and in the accessible tree.
  const instant = viewRecord(
    view([
      {
        ...POST_ROW,
        aria: `${POST_ROW.aria}\n  - text: edited 2026-11-26T08:43:10Z`,
        tabStops: [{ name: "Edited 2026-11-26 08:43", role: null, tag: "time" }],
      },
    ]),
    NAMING,
    "Conversation at 1440 px"
  );
  expect(() => widthRecord(1440, { conversation: instant })).toThrow(
    "the record.views.conversation.rows[0].aria holds an instant, 2026-11-26T08:43"
  );
  expect(() =>
    widthRecord(1440, {
      conversation: { ...instant, rows: instant.rows.map((one) => ({ ...one, aria: "" })) },
    })
  ).toThrow("the record.views.conversation.rows[0].tabStops[0].name holds an instant");

  // Two rows of one block under one parent, which a comparison by key could not tell apart.
  const statement = row({ id: "spec#b-r1", type: "requirement", words: "The tree read." });
  expect(() => viewRecord(view([statement, statement]), NAMING, "Spec at 1440 px")).toThrow(
    "Spec at 1440 px, two rows are named spec#b-r1 at the top, so a comparison by key and parent key could not tell them apart"
  );
  // One check under two statements is two rows, each under its own parent.
  const check = row({ id: "verification-md#b-c1", level: 2, type: "check", words: "Check 1." });
  const second = row({ id: "spec#b-r2", type: "requirement", words: "The outline." });
  const served = viewRecord(view([statement, check, second, check]), NAMING, "Verification");
  expect(served.rows.map((one) => [one.parentKey, one.key])).toEqual([
    [null, "spec#b-r1"],
    ["spec#b-r1", "verification-md#b-c1"],
    [null, "spec#b-r2"],
    ["spec#b-r2", "verification-md#b-c1"],
  ]);
  expect(() => expectRowsNamedApart(served.rows, "Verification")).not.toThrow();
});

test("each recorded row holds its links, its tab stops and its accessible tree", () => {
  const recorded = viewRecord(view([POST_ROW, REPLY_ROW], 1), NAMING, "Conversation at 1440 px");
  expect(recorded).toEqual({
    focus: { key: "{key}>{posts[0]}>{posts[0]/comment@0}", parentKey: "{key}>{posts[0]}" },
    keys: null,
    paging: [],
    rows: [
      {
        address: "/issues/{key}/comments/{posts[0]}",
        aria: [
          '- treeitem "The tree read draws every node":',
          '  - link "{key}":',
          "    - /url: /issues/{key}",
          "  - text: alice · {day}",
        ].join("\n"),
        hidden: [],
        key: "{key}>{posts[0]}",
        kind: "comment",
        level: 1,
        links: ["/issues/{key}", "/issues/{key}/comments/{posts[0]}"],
        parentKey: null,
        serverPlace: { by: "made", rank: 0 },
        tabStops: [
          { name: "The tree read draws every node", role: "treeitem", tag: "div" },
          { name: "{key}", role: null, tag: "a" },
        ],
        words: "The tree read draws every node, as {key} does. alice · {day}",
      },
      {
        address: "/issues/{key}/comments/{posts[0]/comment@0}",
        aria: '- treeitem "The outline keeps it"',
        hidden: [],
        key: "{key}>{posts[0]}>{posts[0]/comment@0}",
        kind: "comment",
        level: 2,
        links: [],
        parentKey: "{key}>{posts[0]}",
        serverPlace: { by: "made", rank: 0 },
        tabStops: [],
        words: "The outline keeps it. bob · {day}",
      },
    ],
  } satisfies ViewRecord);

  // The comparison reads what the record holds: the same rows hold no difference, and a row that
  // lost a link, a tab stop and a line of its accessible tree differs on those three clauses.
  expect(viewDifferences(recorded, recorded)).toEqual([]);
  const changed = viewRecord(
    view(
      [
        {
          ...POST_ROW,
          aria: POST_ROW.aria.replace('link "LAY-1"', 'link "the issue"'),
          links: POST_ROW.links.slice(1),
          tabStops: POST_ROW.tabStops.slice(0, 1),
        },
        REPLY_ROW,
      ],
      1
    ),
    NAMING,
    "Conversation at 1440 px"
  );
  expect(viewDifferences(recorded, changed)).toEqual([
    {
      clause: "links",
      line: '{key}>{posts[0]}: the base links ["/issues/{key}","/issues/{key}/comments/{posts[0]}"], the page ["/issues/{key}/comments/{posts[0]}"]',
    },
    {
      clause: "tab stops",
      line: '{key}>{posts[0]}: the base stops at [{"name":"The tree read draws every node","role":"treeitem","tag":"div"},{"name":"{key}","role":null,"tag":"a"}], the page at [{"name":"The tree read draws every node","role":"treeitem","tag":"div"}]',
    },
    {
      clause: "accessible tree",
      line: `{key}>{posts[0]}: line 2 is ${JSON.stringify('  - link "{key}":')} in the base and ${JSON.stringify('  - link "the issue":')} on the page`,
    },
  ]);

  // A word the page hides that the base showed differs on the visibility clause, and a word the
  // base hid too (a glyph's title, say) does not.
  const hiddenAtBase = {
    ...recorded,
    rows: recorded.rows.map((one) => ({ ...one, hidden: ["Comment"] })),
  };
  const hiddenOnPage = {
    ...recorded,
    rows: recorded.rows.map((one, at) => ({
      ...one,
      hidden: at === 0 ? ["Comment", "alice"] : ["Comment"],
    })),
  };
  expect(viewDifferences(hiddenAtBase, hiddenOnPage)).toEqual([
    {
      clause: "visibility",
      line: '{key}>{posts[0]}: the page hides "alice", which the base shows',
    },
  ]);
});

/** A recorded row with nothing in it but its key, its parent's key, its level and its place in
 *  the server's order. */
function recordedRow(
  key: string,
  parentKey: string | null,
  serverPlace: RecordedRow["serverPlace"],
  level = 1
): RecordedRow {
  return {
    address: null,
    aria: "",
    hidden: [],
    key,
    kind: serverPlace === null ? "requirement" : "comment",
    level,
    links: [],
    parentKey,
    serverPlace,
    tabStops: [],
    words: key,
  };
}

/** A view of `rows` whose top level holds its rows past the first page behind one control. */
function paged(rows: readonly RecordedRow[], words = "2 more rows"): ViewRecord {
  return {
    focus: null,
    keys: null,
    paging: [{ kind: "more", level: 1, parentKey: null, words }],
    rows,
  };
}

test("two seeds' rows differ only where the server ordered them differently, and at a page's edge", () => {
  const made = (rank: number) => ({ by: "made" as const, rank });
  // The base drew posts A, B and C, each with a reply, then a statement, and held two posts back.
  const base = paged([
    recordedRow("A", null, made(0)),
    recordedRow("A/reply", "A", made(0), 2),
    recordedRow("B", null, made(1)),
    recordedRow("B/reply", "B", made(0), 2),
    recordedRow("C", null, made(2)),
    recordedRow("spec#b-r1", null, null),
  ]);

  // This seed's server took B before A, and D before C, so D stands at the page's edge where C
  // did: neither is a difference, and neither are the rows under them.
  const otherSeed = paged([
    recordedRow("B", null, made(0)),
    recordedRow("B/reply", "B", made(0), 2),
    recordedRow("A", null, made(1)),
    recordedRow("A/reply", "A", made(0), 2),
    recordedRow("D", null, made(2)),
    recordedRow("D/reply", "D", made(0), 2),
    recordedRow("spec#b-r1", null, null),
  ]);
  expect(viewDifferences(base, otherSeed)).toEqual([]);

  // Where both seeds' server took A before B, the page must draw A first.
  const swapped = paged([
    recordedRow("B", null, made(1)),
    recordedRow("B/reply", "B", made(0), 2),
    recordedRow("A", null, made(0)),
    recordedRow("A/reply", "A", made(0), 2),
    recordedRow("C", null, made(2)),
    recordedRow("spec#b-r1", null, null),
  ]);
  expect(viewDifferences(base, swapped)).toEqual([
    {
      clause: "order",
      line: "at the top, the base draws A before B, and the page draws A after it",
    },
  ]);

  // A statement the seed places is no row at a page's edge: its absence is a difference, and so
  // is a row more under the paged parent, which stands past its first page.
  const lost = paged(base.rows.filter((one) => one.key !== "spec#b-r1"));
  expect(viewDifferences(base, lost)).toEqual([
    { clause: "rows", line: "the base draws spec#b-r1 at the top, and the page does not" },
    {
      clause: "paging",
      line: "at the top the base draws 4 rows before the control that pages them, and the page 3",
    },
  ]);
  const more = paged([...base.rows, recordedRow("E", null, made(3))], "1 more row");
  expect(viewDifferences(base, more)).toEqual([
    {
      clause: "paging",
      line: "at the top the base draws 4 rows before the control that pages them, and the page 5",
    },
    {
      clause: "paging",
      line: 'at the top the base holds the rows past its page behind "2 more rows", and the page behind "1 more row"',
    },
  ]);
});
