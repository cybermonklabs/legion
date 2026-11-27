/**
 * Every form the server reads bare, again in code: the body that writes it inside a code span or
 * a code block, beside the body of the row of `DISPATCH_TEXT_REFERENCES` it repeats. The server
 * indexes each one there, as it indexes a `dispatch://` reference in code, so a twin cites
 * exactly what its row cites, and no renderer links inside a code span or a code block, so a
 * twin links nothing. A form added to the written rows is twinned by one line here; its facts
 * are never written a second time, since `dispatch-text-references.ts` builds each twin's row
 * from the row it repeats (`inCodeRow`).
 */
export const IN_CODE: readonly (readonly [written: string, inCode: string])[] = [
  ["CORE-1 is blocked.", "`CORE-1` is blocked."],
  ["Blocked on CORE-1 today.", "Blocked on `CORE-1` today."],
  ["Blocked (CORE-1) today.", "Blocked `(CORE-1)` today."],
  ["Blocked [CORE-1] today.", "Blocked `[CORE-1]` today."],
  ['Blocked on "CORE-1" today.', 'Blocked on `"CORE-1"` today.'],
  ["Blocked on 'CORE-1' today.", "Blocked on `'CORE-1'` today."],
  ["Blocked on **CORE-1** today.", "Blocked on `**CORE-1**` today."],
  ["Blocked on _CORE-1_ today.", "Blocked on `_CORE-1_` today."],
  [">CORE-1 is blocked.", "`>CORE-1` is blocked."],
  ["Blocked on ~~CORE-1~~ today.", "Blocked on `~~CORE-1~~` today."],
  ["|issue|state|\n|---|---|\n|CORE-1|open|", "Cells are written `|CORE-1|open|`."],
  ["Blocked on&#32;CORE-1 today.", "Blocked on `&#32;CORE-1` today."],
  ["Blocked on&#x20;CORE-1 today.", "Blocked on `&#x20;CORE-1` today."],
  ["Blocked on&#9;CORE-1 today.", "Blocked on `&#9;CORE-1` today."],
  ["Blocked on&#10;CORE-1 today.", "Blocked on `&#10;CORE-1` today."],
  ["See XCORE-1 here.", "See `XCORE-1` here."],
  ["Both CORE-1 and ACME-12 are blocked.", "Both `CORE-1` and `ACME-12` are blocked."],
  ["CORE-1's spec is done.", "`CORE-1's` spec is done."],
  ["Done in CORE-1.", "Done in `CORE-1.`"],
  ["Blocked on CORE-1, then shipped.", "Blocked on `CORE-1,` then shipped."],
  ["See [the blocker](CORE-1).", "See `[the blocker](CORE-1)`."],
  // The same key again, in a fenced code block rather than a span.
  ["CORE-1 is blocked.", "```\nCORE-1 is blocked.\n```"],
  ["See #q2 for the rollout.", "See `#q2` for the rollout."],
  ["As agreed, _see #q2_.", "As agreed, `_see #q2_`."],
  ["Answered in #q2.", "Answered in `#q2.`"],
  ["See [the question](#q2).", "See `[the question](#q2)`."],
  ["See #7b2c9d04 for the rollout.", "See `#7b2c9d04` for the rollout."],
  ["See dispatch://CORE-1/ask/2a7f1c90 here.", "See `dispatch://CORE-1/ask/2a7f1c90` here."],
  [
    "See dispatch://CORE-1/ask/2a7f1c90-1f4e here.",
    "See `dispatch://CORE-1/ask/2a7f1c90-1f4e` here.",
  ],
  ["See dispatch://CORE-1/ask/2A7F1C90 here.", "See `dispatch://CORE-1/ask/2A7F1C90` here."],
  [
    "See dispatch://CORE-1/comment/3b8e2d01 here.",
    "See `dispatch://CORE-1/comment/3b8e2d01` here.",
  ],
  [
    "See dispatch://CORE-1/comment/3b8e2d01-2a5f here.",
    "See `dispatch://CORE-1/comment/3b8e2d01-2a5f` here.",
  ],
  [
    "See dispatch://CORE-1/comment/3B8E2D01 here.",
    "See `dispatch://CORE-1/comment/3B8E2D01` here.",
  ],
  [
    "See dispatch://CORE-1/message/4c9f3e12 here.",
    "See `dispatch://CORE-1/message/4c9f3e12` here.",
  ],
  [
    "See dispatch://CORE-1/message/4c9f3e12-3b60 here.",
    "See `dispatch://CORE-1/message/4c9f3e12-3b60` here.",
  ],
  [
    "See dispatch://CORE-1/message/4C9F3E12 here.",
    "See `dispatch://CORE-1/message/4C9F3E12` here.",
  ],
  ["See dispatch://CORE-1/spec#b-5da04f23 here.", "See `dispatch://CORE-1/spec#b-5da04f23` here."],
  [
    "See dispatch://CORE-1/spec#b-5da04f23-4c71 here.",
    "See `dispatch://CORE-1/spec#b-5da04f23-4c71` here.",
  ],
  ["See dispatch://CORE-1/spec#b-5DA04F23 here.", "See `dispatch://CORE-1/spec#b-5DA04F23` here."],
  // The three parties, each in a code span: cited there as every other address is, linked
  // nowhere, so a person's twin links nothing for the second reason as well.
  [
    "Both dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551 and its dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png here.",
    "Both `dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551` and its `dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png` here.",
  ],
  ["Waiting on dispatch://role/reviewer today.", "Waiting on `dispatch://role/reviewer` today."],
  [
    "Asked dispatch://person/alice@example.com today.",
    "Asked `dispatch://person/alice@example.com` today.",
  ],
];
