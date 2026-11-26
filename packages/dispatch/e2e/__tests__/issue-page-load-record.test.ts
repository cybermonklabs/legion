/** The pure parts of the recorder of the rows the base draws (`e2e/tools/issue-page-load-record.ts`)
 *  and of the comparison `e2e/issue-page-load-same.e2e.ts` makes with its record, on small
 *  hand-written readings, so each rule is held without a browser or a stack.
 *
 *  - The recorder names each row by what the seed returned and writes each day the page draws as
 *    `{day}`, a key the words after it follow with no space included; it refuses a record that
 *    still holds an id the server minted (a UUID, an issue's key the seed did not return) or an
 *    instant, and two rows with one key under one parent.
 *  - Each recorded row holds its key, its address, its words, its links by `href` in order, its
 *    tab stops and its accessible tree, each row inside it one line of that tree, each named as
 *    the row's key is, and the comparison reads each of them.
 *  - The record is one file for each view, each naming the base, its parts and the recorder's
 *    format, and holding the view at both widths.
 *  - Two seeds of `seedLargestShape` are not the same data where the seed writes several things
 *    at once: the server gives those writes their times and keys in the order it took them,
 *    which another run does not repeat. So each row carries its anchors, the writes its place
 *    depends on (its own, the ask or post a statement came from, those of the rows a row of its
 *    node stands under), and the comparison lets exactly two things differ: the order of two rows
 *    whose anchors stand in another order, or at one rank, by some measure on the two seeds (when
 *    the server made a write, when an ask closed, a child's key as text), and, under a parent
 *    whose rows past a page are held back, which anchored rows stand at the page's edge, with the
 *    rows under them, while the number drawn and the paging control's words are the same. Every
 *    other difference is one.
 */
import { expect, test } from "bun:test";

import {
  expectRowsNamedApart,
  type Naming,
  type PageRow,
  type PageView,
  RECORD_FORMAT,
  type RecordedRow,
  rowsRecord,
  type ServerTimes,
  type ViewRecord,
  viewDifferences,
  viewRecord,
  widthRecord,
  withoutNestedRows,
} from "../tools/issue-page-load-record";

/** The issue's key, and the ids the server minted for a post and its reply. */
const ISSUE = "LAY-1";
const POST = "6f1c2b3a-0d4e-4f5a-8b6c-7d8e9f0a1b2c";
const REPLY = "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f";
/** An ask's id no name is known for. */
const ASK = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d";

/** What the seed returned (the issue's key, a child's key and the post's id) and when the server
 *  took the post and its reply. */
const NAMING: Naming = {
  anchors: new Map(),
  issue: ISSUE,
  names: new Map([
    [ISSUE, "key"],
    ["LAY-2", "children[3]"],
    [POST, "posts[0]"],
  ]),
  times: new Map([
    [POST, { made: "2026-11-26T08:00:00.000001Z" }],
    [REPLY, { made: "2026-11-26T08:00:01.5Z" }],
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

  // A child's key the words after it follow with no space, a tag and a title read as one text.
  const glued = viewRecord(
    view([row({ id: "LAY-2", type: "child_issue", words: "LAY-2Child 4: the outline" })]),
    NAMING,
    "Conversation at 1440 px"
  );
  expect(glued.rows.map((one) => [one.key, one.words])).toEqual([
    ["{children[3]}", "{children[3]}Child 4: the outline"],
  ]);

  // An ask whose id the seed did not return, linked from the post's words.
  const minted = viewRecord(
    view([{ ...POST_ROW, links: [...POST_ROW.links, `/issues/${ISSUE}/asks/${ASK}`] }]),
    NAMING,
    "Conversation at 1440 px"
  );
  expect(() => widthRecord(1440, { conversation: minted })).toThrow(
    `the record.views.conversation.rows[0].links[2] holds a UUID, ${ASK}, which belongs to this run alone`
  );

  // A child's key the server numbered and the seed did not return, glued to its title or not.
  for (const words of ["The tree read draws every node, as LAY-7 does.", "LAY-7Child 9"]) {
    const numbered = viewRecord(view([{ ...POST_ROW, words }]), NAMING, "Conversation at 1440 px");
    expect(() => widthRecord(390, { conversation: numbered })).toThrow(
      "the record.views.conversation.rows[0].words holds an issue's key, LAY-7"
    );
  }

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
        anchors: [{ name: "posts[0]", ranks: { made: 0 } }],
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
        tabStops: [
          { name: "The tree read draws every node", role: "treeitem", tag: "div" },
          { name: "{key}", role: null, tag: "a" },
        ],
        words: "The tree read draws every node, as {key} does. alice · {day}",
      },
      {
        address: "/issues/{key}/comments/{posts[0]/comment@0}",
        // Its own write, and its parent's, which it stands under.
        anchors: [
          { name: "posts[0]", ranks: { made: 0 } },
          { name: "posts[0]/comment@0", ranks: { made: 1 } },
        ],
        aria: '- treeitem "The outline keeps it"',
        hidden: [],
        key: "{key}>{posts[0]}>{posts[0]/comment@0}",
        kind: "comment",
        level: 2,
        links: [],
        parentKey: "{key}>{posts[0]}",
        tabStops: [],
        words: "The outline keeps it. bob · {day}",
      },
    ],
  } satisfies ViewRecord);

  // A row inside a row (a check inside a Verification card) is one line of the tree around it,
  // since it is read as a row of its own.
  expect(
    withoutNestedRows(
      [
        '- treeitem "The tree read" [level=1]:',
        "  - text: Met",
        "  - group:",
        `    - 'treeitem "Check 8: a folded branch" [level=2]':`,
        "      - text: browser",
        '    - treeitem "Check 39" [level=2]',
        '  - link "Also under"',
      ].join("\n")
    )
  ).toBe(
    [
      '- treeitem "The tree read" [level=1]:',
      "  - text: Met",
      "  - group:",
      "    - treeitem",
      "    - treeitem",
      '  - link "Also under"',
    ].join("\n")
  );

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

test("the record is one file for each view, each naming the base, its parts and both widths", () => {
  const conversation = viewRecord(view([POST_ROW]), NAMING, "Conversation");
  const spec = viewRecord(view([row({ id: "spec#b-r1", type: "requirement" })]), NAMING, "Spec");
  const base = { parts: ["b8651fc5", "f458f1b1"], recordedOn: "f458f1b1" };
  const files = rowsRecord(base, [
    widthRecord(390, { conversation, spec }),
    widthRecord(1440, { conversation, spec }),
  ]);
  expect(files.map((file) => [file.view, file.commit, file.parts, file.format])).toEqual([
    ["conversation", "f458f1b1", ["b8651fc5", "f458f1b1"], RECORD_FORMAT],
    ["spec", "f458f1b1", ["b8651fc5", "f458f1b1"], RECORD_FORMAT],
  ]);
  expect(files[1]?.widths).toEqual({ "390": spec, "1440": spec });

  // Both widths, the same views at each, and a line of this recorder's format.
  expect(() => rowsRecord(base, [widthRecord(1440, { conversation })])).toThrow(
    "no LOAD-RECORD line records 390 px"
  );
  expect(() =>
    rowsRecord(base, [widthRecord(1440, { conversation, spec }), widthRecord(390, { spec })])
  ).toThrow("the line of 1440 px records conversation, spec and the line of 390 px spec");
  expect(() =>
    rowsRecord(base, [
      widthRecord(1440, { "../conversation": conversation }),
      widthRecord(390, { "../conversation": conversation }),
    ])
  ).toThrow('records a view "../conversation", which names no file');
});

/** A recorded row with nothing in it but its key, its parent's key, its level and its anchors. */
function recordedRow(
  key: string,
  parentKey: string | null,
  anchors: RecordedRow["anchors"],
  level: number | null = 1
): RecordedRow {
  return {
    address: null,
    anchors,
    aria: "",
    hidden: [],
    key,
    kind: anchors.length === 0 ? "requirement" : "comment",
    level,
    links: [],
    parentKey,
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

/** An anchor made at rank `rank`. */
function made(name: string, rank: number) {
  return { name, ranks: { made: rank } };
}

test("two seeds' rows differ only where the server ordered them differently, and at a page's edge", () => {
  // The base drew posts A, B and C, each with a reply, then a statement, and held two posts back.
  const base = paged([
    recordedRow("A", null, [made("A", 0)]),
    recordedRow("A/reply", "A", [made("A", 0), made("A/reply", 3)], 2),
    recordedRow("B", null, [made("B", 1)]),
    recordedRow("B/reply", "B", [made("B", 1), made("B/reply", 4)], 2),
    recordedRow("C", null, [made("C", 2)]),
    recordedRow("spec#b-r1", null, []),
  ]);

  // This seed's server took B before A, and D before C, so D stands at the page's edge where C
  // did: neither is a difference, and neither are the rows under them.
  const otherSeed = paged([
    recordedRow("B", null, [made("B", 0)]),
    recordedRow("B/reply", "B", [made("B", 0), made("B/reply", 3)], 2),
    recordedRow("A", null, [made("A", 1)]),
    recordedRow("A/reply", "A", [made("A", 1), made("A/reply", 4)], 2),
    recordedRow("D", null, [made("D", 2)]),
    recordedRow("D/reply", "D", [made("D", 2), made("D/reply", 5)], 2),
    recordedRow("spec#b-r1", null, []),
  ]);
  expect(viewDifferences(base, otherSeed)).toEqual([]);

  // Where both seeds' server took A before B, the page must draw A first.
  const swapped = paged([
    recordedRow("B", null, [made("B", 1)]),
    recordedRow("B/reply", "B", [made("B", 1), made("B/reply", 4)], 2),
    recordedRow("A", null, [made("A", 0)]),
    recordedRow("A/reply", "A", [made("A", 0), made("A/reply", 3)], 2),
    recordedRow("C", null, [made("C", 2)]),
    recordedRow("spec#b-r1", null, []),
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
  const more = paged([...base.rows, recordedRow("E", null, [made("E", 3)])], "1 more row");
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

test("a statement is ordered by the ask it came from, a check by the statements it serves, an ask by when it closed", () => {
  // Two asks of one burst: ask 1 was made before ask 3 on both seeds, and closed first on the
  // base's seed only. Statement r2 came from ask 1 and r6 from ask 3; r5 from ask 1 too, and r4
  // from none. Check 8 serves r5 and r6, check 39 r6 alone, check 40 r6 alone.
  const times = (closed: readonly [string, string]): ReadonlyMap<string, ServerTimes> =>
    new Map([
      ["ask-1", { closed: closed[0], made: "2026-11-26T08:00:01Z" }],
      ["ask-3", { closed: closed[1], made: "2026-11-26T08:00:02Z" }],
    ]);
  const naming = (closed: readonly [string, string]): Naming => ({
    anchors: new Map([
      ["spec#b-r2", ["ask-1"]],
      ["spec#b-r5", ["ask-1"]],
      ["spec#b-r6", ["ask-3"]],
    ]),
    issue: ISSUE,
    names: new Map([
      ["ask-1", 'ask "Should one?"'],
      ["ask-3", 'ask "Should three?"'],
    ]),
    times: times(closed),
  });
  const before = naming(["2026-11-26T08:00:03Z", "2026-11-26T08:00:04Z"]);
  const after = naming(["2026-11-26T08:00:04Z", "2026-11-26T08:00:03Z"]);
  const statement = (block: string) => row({ id: `spec#b-${block}`, type: "requirement" });
  const check = (block: string) =>
    row({ id: `verification-md#b-${block}`, level: 2, type: "check" });
  const ask = (id: string) => row({ id, type: "ask" });

  // The base: r4 (from no ask), r2 (ask 1), r5 (ask 1) with check 8, r6 (ask 3) with checks 8,
  // 39 and 40; then the two asks, ask 1 first. The other seed draws ask 3's rows before ask 1's,
  // as a server that orders by closing would where ask 3 closed first.
  const atBase = viewRecord(
    view([
      statement("r4"),
      statement("r2"),
      statement("r5"),
      check("c8"),
      statement("r6"),
      check("c8"),
      check("c39"),
      check("c40"),
      ask("ask-1"),
      ask("ask-3"),
    ]),
    before,
    "Verification"
  );
  const onPage = viewRecord(
    view([
      statement("r4"),
      statement("r6"),
      check("c39"),
      check("c8"),
      check("c40"),
      statement("r2"),
      statement("r5"),
      check("c8"),
      ask("ask-3"),
      ask("ask-1"),
    ]),
    after,
    "Verification"
  );
  // Check 8 stands by both statements it serves, check 39 by r6's ask alone.
  expect(onPage.rows.find((one) => one.key === "verification-md#b-c8")?.anchors).toEqual([
    { name: 'ask "Should one?"', ranks: { closed: 1, made: 0 } },
    { name: 'ask "Should three?"', ranks: { closed: 0, made: 1 } },
  ]);
  // r6 and r2 or r5 may trade places, since their asks closed in another order, and so may the
  // asks; check 39 and check 8 under r6 too, since check 8 stands by ask 1 as well.
  expect(viewDifferences(atBase, onPage)).toEqual([]);
  // r4, which came from no ask, keeps its place against each of them, and check 39 and check 40,
  // which both stand by ask 3 alone, keep theirs.
  const moved = viewRecord(
    view([
      statement("r2"),
      statement("r4"),
      statement("r6"),
      check("c40"),
      check("c8"),
      check("c39"),
      statement("r5"),
      check("c8"),
      ask("ask-3"),
      ask("ask-1"),
    ]),
    after,
    "Verification"
  );
  expect(viewDifferences(atBase, moved)).toEqual([
    {
      clause: "order",
      line: "at the top, the base draws spec#b-r4 before spec#b-r2, and the page draws spec#b-r4 after it",
    },
    {
      clause: "order",
      line: "under spec#b-r6, the base draws verification-md#b-c39 before verification-md#b-c40, and the page draws verification-md#b-c39 after it",
    },
  ]);

  // Where the asks closed in the same order on both seeds, the asks keep their order too.
  const sameClosing = viewRecord(
    view([statement("r2"), statement("r4"), statement("r5"), ask("ask-3"), ask("ask-1")]),
    before,
    "Decisions"
  );
  const baseClosing = viewRecord(
    view([statement("r2"), statement("r4"), statement("r5"), ask("ask-1"), ask("ask-3")]),
    before,
    "Decisions"
  );
  expect(viewDifferences(baseClosing, sameClosing)).toEqual([
    {
      clause: "order",
      line: "at the top, the base draws ask-1 before ask-3, and the page draws ask-1 after it",
    },
  ]);
});

test("a task's chips may name its statements otherwise where the asks they came from stand otherwise", () => {
  // Task 16 serves r10, which came from ask 5, and r11, from ask 3: the task stands by both asks.
  const naming = (made: readonly [string, string]): Naming => ({
    anchors: new Map([["plan-md#b-t16", ["ask-5", "ask-3"]]]),
    issue: ISSUE,
    names: new Map([
      [ISSUE, "key"],
      ["ask-3", 'ask "Should three?"'],
      ["ask-5", 'ask "Should five?"'],
    ]),
    times: new Map([
      ["ask-3", { made: made[0] }],
      ["ask-5", { made: made[1] }],
    ]),
  });
  const earlier = naming(["2026-11-26T08:00:01Z", "2026-11-26T08:00:02Z"]);
  const later = naming(["2026-11-26T08:00:02Z", "2026-11-26T08:00:01Z"]);
  const task = (statements: readonly string[], words: string) =>
    viewRecord(
      view([
        row({
          aria: `- treeitem "${words}"`,
          id: "plan-md#b-t16",
          links: statements.map((block) => `/issues/${ISSUE}/spec#b-${block}`),
          type: "plan_task",
          words,
        }),
      ]),
      earlier,
      "Plan"
    );
  const both = task(["r10", "r11"], "Task 16. The reply line counts… The tree read pages…");
  const once = task(["r10", "r10"], "Task 16. The reply line counts… The reply line counts…");
  expect(both.rows[0]?.anchors.map((anchor) => anchor.name)).toEqual([
    'ask "Should five?"',
    'ask "Should three?"',
  ]);
  // Where the two asks stand in another order on the page's seed, the chips may name the
  // statements otherwise: links block for block, and the words and the tree with them.
  const onOtherSeed = viewRecord(
    view([
      row({
        aria: `- treeitem "${once.rows[0]?.words ?? ""}"`,
        id: "plan-md#b-t16",
        links: [`/issues/${ISSUE}/spec#b-r10`, `/issues/${ISSUE}/spec#b-r10`],
        type: "plan_task",
        words: once.rows[0]?.words ?? "",
      }),
    ]),
    later,
    "Plan"
  );
  expect(viewDifferences(both, onOtherSeed)).toEqual([]);
  // Where they stand in the same order, every such difference is one.
  expect(viewDifferences(both, once).map((one) => one.clause)).toEqual([
    "words",
    "links",
    "accessible tree",
  ]);
  // And a link that is no block's is never excused, nor a chip more.
  const toAnIssue = viewRecord(
    view([
      row({
        aria: `- treeitem "${both.rows[0]?.words ?? ""}"`,
        id: "plan-md#b-t16",
        links: [`/issues/${ISSUE}/spec#b-r10`, "/issues/LAY-2"],
        type: "plan_task",
        words: both.rows[0]?.words ?? "",
      }),
    ]),
    later,
    "Plan"
  );
  expect(viewDifferences(both, toAnIssue).map((one) => one.clause)).toEqual(["links"]);
});

test("the children's section may hold other children at its page's edge, ordered by their keys", () => {
  // Children are named by the seed's place and ordered by the key the server gave them, as text.
  const naming = (keys: readonly string[]): Naming => ({
    anchors: new Map(),
    issue: ISSUE,
    names: new Map(keys.map((key, at) => [key, `children[${at}]`])),
    times: new Map(keys.map((key) => [key, { key }])),
  });
  const section = (keys: readonly string[], shown: readonly number[]): PageView => ({
    focus: null,
    keys: null,
    paging: [{ after: shown.length - 1, kind: "children", level: null, words: "1 more child" }],
    rows: shown.map((at) => row({ child: keys[at] ?? "", level: null })),
  });
  // The base's seed gave child 0 the key LAY-10, child 1 LAY-2 and child 2 LAY-3; the section
  // lists the first two by key as text: LAY-10, LAY-2. The other seed gave child 0 LAY-3, child
  // 1 LAY-10, child 2 LAY-2, so it lists child 1, then child 2.
  const baseKeys = ["LAY-10", "LAY-2", "LAY-3"];
  const pageKeys = ["LAY-3", "LAY-10", "LAY-2"];
  const atBase = viewRecord(section(baseKeys, [0, 1]), naming(baseKeys), "Spec");
  const onPage = viewRecord(section(pageKeys, [1, 2]), naming(pageKeys), "Spec");
  expect(atBase.rows.map((one) => one.key)).toEqual(["{children[0]}", "{children[1]}"]);
  expect(onPage.rows.map((one) => one.key)).toEqual(["{children[1]}", "{children[2]}"]);
  expect(viewDifferences(atBase, onPage)).toEqual([]);
  // A child more before the control is a difference.
  const longer = viewRecord(section(pageKeys, [1, 2, 0]), naming(pageKeys), "Spec");
  expect(viewDifferences(atBase, longer)).toEqual([
    {
      clause: "paging",
      line: "at the top the base draws 2 rows before the control that pages them, and the page 3",
    },
  ]);
});
