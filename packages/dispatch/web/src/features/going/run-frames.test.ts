import { expect, test } from "bun:test";

import { type RunFrames, STARTED_RUN, withRunFrame } from "./run-frames";

// A run's steps as its frames say them, keyed by their step numbers: each call's words while it
// runs, from its call frame or from the reply that made it, then from its own tool frame, with
// the kind of step it is, how many results it answered and the items it named; one line for each
// call whatever order its call, tool and reply frames came in, and none left running once the run
// ends; the prose beside calls is the words of the run while they run and the header of those
// calls, never part of its answer; the answer's reply says how many tool results were withheld
// from the model; and the frame that ends the answer says whether it waits for the person.

/** The run once each frame of `frames` arrived, in order. */
function framed(frames: readonly (readonly [string, unknown])[]): RunFrames {
  return frames.reduce((run, [name, data]) => withRunFrame(run, name, data), STARTED_RUN);
}

test("each call runs with its reply's present words, and ends with its frame's past words, kind and items", () => {
  const replied = framed([
    [
      "reply",
      {
        labels: ["Reading DEMO-15", "Reading the messages of DEMO-7"],
        step: 1,
        text: "Checking the issue.",
        tools: ["read", "call_tool"],
      },
    ],
  ]);
  expect(replied.steps.map((step) => [step.label, step.running, step.kind])).toEqual([
    ["Reading DEMO-15", true, ""],
    ["Reading the messages of DEMO-7", true, ""],
  ]);
  const ended = withRunFrame(replied, "tool", {
    error: true,
    items: ["DEMO-7"],
    kind: "read",
    label: "Could not read the messages of DEMO-7: you have no access",
    note: "call_tool {}: refused",
    step: 3,
    tool: "call_tool",
  });
  expect(ended.steps[1]).toEqual({
    count: 0,
    header: "Checking the issue.",
    id: 1,
    items: ["DEMO-7"],
    kind: "read",
    label: "Could not read the messages of DEMO-7: you have no access",
    refused: true,
    running: false,
    slip: false,
    step: 3,
    tool: "call_tool",
  });
  expect(ended.steps[0]?.running).toBe(true);
});

test("the prose beside calls heads them, and the answer is the reply that called none", () => {
  const run = framed([
    [
      "reply",
      { labels: ["Reading DEMO-1"], text: "Checking the children of DEMO-1.", tools: ["read"] },
    ],
    ["tool", { kind: "read", label: "Read DEMO-1", step: 2, tool: "read" }],
    ["reply", { labels: [], text: "DEMO-1 has two children.", tools: [], withheld: 2 }],
  ]);
  expect(run.replies).toEqual(["DEMO-1 has two children."]);
  expect(run.callsHeader).toBe("");
  expect(run.withheld).toBe(2);
  const calling = framed([
    [
      "reply",
      { labels: ["Reading DEMO-1"], text: "Checking the children of DEMO-1.", tools: ["read"] },
    ],
  ]);
  expect(calling.replies).toEqual([]);
  expect(calling.callsHeader).toBe("Checking the children of DEMO-1.");
  expect(calling.withheld).toBe(0);
});

test("a run recorded before calls had words has steps with no words", () => {
  const run = framed([
    ["reply", { text: "", tools: ["call_tool"] }],
    ["tool", { note: "call_tool dispatch_read: CORE-1", step: 2, tool: "call_tool" }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  expect(run.steps.map((step) => [step.label, step.kind, step.items])).toEqual([["", "", []]]);
});

test("a row an approved plan's runner made, which no reply named, is a write step of its own", () => {
  const run = framed([
    [
      "tool",
      {
        items: ["DEMO-7"],
        kind: "write",
        label: "Commented on DEMO-7",
        step: 6,
        tool: "dispatch_comment",
      },
    ],
  ]);
  expect(run.steps).toEqual([
    {
      count: 0,
      header: "",
      id: 0,
      items: ["DEMO-7"],
      kind: "write",
      label: "Commented on DEMO-7",
      refused: false,
      running: false,
      slip: false,
      step: 6,
      tool: "dispatch_comment",
    },
  ]);
});

/** A call frame of `step`: `read` of DEMO-15 where `step` is 2, else `call_tool` of DEMO-7's
 *  messages; `text` where the frame is its model call's first. */
function call(step: number, text?: string): readonly [string, unknown] {
  const [tool, label] =
    step === 2 ? ["read", "Reading DEMO-15"] : ["call_tool", "Reading the messages of DEMO-7"];
  return ["call", { kind: "read", label, step, tool, ...(text === undefined ? {} : { text }) }];
}

/** The tool frame that ends the call of `step` that `call` makes. */
function ended(step: number): readonly [string, unknown] {
  return step === 2
    ? ["tool", { items: ["DEMO-15"], kind: "read", label: "Read DEMO-15", step, tool: "read" }]
    : [
        "tool",
        {
          items: ["DEMO-7"],
          kind: "read",
          label: "Read the messages of DEMO-7",
          step,
          tool: "call_tool",
        },
      ];
}

/** The reply frame of step 1 that made the calls of steps 2 and 3. */
const REPLY: readonly [string, unknown] = [
  "reply",
  {
    labels: ["Reading DEMO-15", "Reading the messages of DEMO-7"],
    step: 1,
    text: "Checking DEMO-15 and DEMO-7.",
    tools: ["read", "call_tool"],
  },
];

/** Each step's words and whether it runs. */
function lines(run: RunFrames): readonly (readonly [string, boolean])[] {
  return run.steps.map((step) => [step.label, step.running]);
}

test("a call frame shows its call running with its present words and kind, and its tool frame ends the step of its number", () => {
  const started = framed([call(2, "Checking DEMO-15"), call(3)]);
  expect(started.steps.map((step) => [step.step, step.label, step.kind, step.running])).toEqual([
    [2, "Reading DEMO-15", "read", true],
    [3, "Reading the messages of DEMO-7", "read", true],
  ]);
  // The first call frame's prose heads the calls; the second carries none, and keeps it.
  expect(started.callsHeader).toBe("Checking DEMO-15");
  const ending = withRunFrame(started, ...ended(3));
  expect(lines(ending)).toEqual([
    ["Reading DEMO-15", true],
    ["Read the messages of DEMO-7", false],
  ]);
  expect(ending.steps.map((step) => step.id)).toEqual([0, 1]);
});

test("a reply after its calls' call and tool frames adds no line, and its prose heads them", () => {
  const run = framed([call(2, "Checking"), call(3), ended(2), ended(3), REPLY]);
  expect(lines(run)).toEqual([
    ["Read DEMO-15", false],
    ["Read the messages of DEMO-7", false],
  ]);
  expect(run.callsHeader).toBe("Checking DEMO-15 and DEMO-7.");
  expect(run.replies).toEqual([]);
  // The next model call's first call frame heads its own calls, with no prose where it wrote none.
  expect(
    withRunFrame(run, "call", {
      kind: "look",
      label: "Looking",
      step: 5,
      text: "",
      tool: "find_tools",
    }).callsHeader
  ).toBe("");
});

test("a tool frame before its call frame ends the call, and the late call frame changes nothing", () => {
  const run = framed([ended(2), call(2, "Checking")]);
  expect(lines(run)).toEqual([["Read DEMO-15", false]]);
  expect(run.callsHeader).toBe("Checking");
});

test("a call frame of a step a cut call still runs takes its line, as the model call made again does", () => {
  // The agent died in the call of step 3; the next agent makes the model call again, whose call
  // of step 3 reads something else.
  const run = framed([
    call(3),
    ["call", { kind: "search", label: "Searching DEMO", step: 3, tool: "search" }],
  ]);
  expect(
    run.steps.map((step) => [step.id, step.step, step.tool, step.label, step.running])
  ).toEqual([[0, 3, "search", "Searching DEMO", true]]);
});

test("a run recorded before call frames, whose tool frames came before their reply, has one line per call", () => {
  const run = framed([
    ended(2),
    ended(3),
    REPLY,
    ["reply", { labels: [], step: 4, text: "DEMO-15 waits on DEMO-7.", tools: [] }],
    ["end", { offers: [], reason: "", state: "done" }],
  ]);
  expect(lines(run)).toEqual([
    ["Read DEMO-15", false],
    ["Read the messages of DEMO-7", false],
  ]);
  expect(run.replies).toEqual(["DEMO-15 waits on DEMO-7."]);
});

test("a reply's call that no frame named, such as a tool the model made up, runs until the end, which drops it", () => {
  const run = framed([
    call(2, ""),
    ended(2),
    [
      "reply",
      { labels: ["", "Reading DEMO-15"], step: 1, text: "", tools: ["made_up_tool", "read"] },
    ],
  ]);
  expect(run.steps.map((step) => [step.tool, step.running])).toEqual([
    ["read", false],
    ["made_up_tool", true],
  ]);
  const over = withRunFrame(run, "end", { offers: [], reason: "", state: "done" });
  expect(lines(over)).toEqual([["Read DEMO-15", false]]);
});

test("once the run ends, no call stays running: a call no tool frame ended draws no line", () => {
  const run = framed([
    call(2, "Checking"),
    call(3),
    ended(2),
    ["end", { offers: [], reason: "STOPPED: its person stopped it", state: "stopped" }],
  ]);
  expect(lines(run)).toEqual([["Read DEMO-15", false]]);
  // A step after the end takes an id that none of the run's steps holds.
  const after = withRunFrame(run, "tool", {
    items: ["DEMO-7"],
    kind: "write",
    label: "Commented on DEMO-7",
    step: 6,
    tool: "dispatch_comment",
  });
  expect(after.steps.map((step) => step.id)).toEqual([0, 1]);
  // A run recorded before call frames, cut after its reply, drops the call its reply named.
  const cut = framed([REPLY, ended(2), ["end", { offers: [], reason: "", state: "failed" }]]);
  expect(lines(cut)).toEqual([["Read DEMO-15", false]]);
});

/** Each step's words and the header it is drawn under. */
function headed(run: RunFrames): readonly (readonly [string, string])[] {
  return run.steps.map((step) => [step.label, step.header]);
}

test("each call is headed by the prose its model call wrote beside it, whichever frame brought the prose", () => {
  // Call frames: the first one's prose heads every call of its model call; the next model call's
  // calls are headed by its own prose, or by none.
  const called = framed([
    call(2, "Checking DEMO-15 and DEMO-7."),
    call(3),
    ended(2),
    ended(3),
    REPLY,
    ["call", { kind: "look", label: "Looking for a way", step: 5, text: "", tool: "find_tools" }],
  ]);
  expect(headed(called)).toEqual([
    ["Read DEMO-15", "Checking DEMO-15 and DEMO-7."],
    ["Read the messages of DEMO-7", "Checking DEMO-15 and DEMO-7."],
    ["Looking for a way", ""],
  ]);
  // A run recorded before call frames: its reply's prose heads the calls whose tool frames came
  // first, and the calls it adds.
  expect(headed(framed([ended(2), REPLY]))).toEqual([
    ["Read DEMO-15", "Checking DEMO-15 and DEMO-7."],
    ["Reading the messages of DEMO-7", "Checking DEMO-15 and DEMO-7."],
  ]);
  // A tool frame before its call frame: the late call frame's prose heads the ended call.
  expect(headed(framed([ended(2), call(2, "Checking")]))).toEqual([["Read DEMO-15", "Checking"]]);
});

test("a tool frame's count is the step's, and a step that answered no list counts none", () => {
  const run = framed([
    [
      "call",
      { kind: "search", label: 'Searching DEMO for "three things"', step: 2, tool: "search" },
    ],
    [
      "tool",
      {
        count: 3,
        items: ["DEMO-1", "DEMO-2", "DEMO-3"],
        kind: "search",
        label: 'Searched DEMO for "three things" · 3 hits',
        step: 2,
        tool: "search",
      },
    ],
    ended(3),
  ]);
  expect(run.steps.map((step) => [step.label, step.count])).toEqual([
    ['Searched DEMO for "three things" · 3 hits', 3],
    ["Read the messages of DEMO-7", 0],
  ]);
});

test("an answer waits for the person only where the frame that ends it says so, never for a question mark", () => {
  const asked = (reply: Record<string, unknown>, end: Record<string, unknown> = {}) =>
    framed([
      ["reply", { labels: [], tools: [], ...reply }],
      ["end", { offers: [], reason: "", state: "done", ...end }],
    ]).waitsForPerson;
  expect(asked({ text: "Which changes hand it over?" })).toBe(false);
  expect(asked({ text: "Which changes hand it over", waits_for_person: true })).toBe(true);
  expect(asked({ text: "Which changes hand it over" }, { waits_for_person: true })).toBe(true);
  // A reply that makes calls ends no turn, and says nothing of it.
  expect(
    framed([
      ["reply", { labels: ["Reading DEMO-1"], text: "", tools: ["read"], waits_for_person: true }],
    ]).waitsForPerson
  ).toBe(false);
});

test("the end of a new-issue run names the issue it filed, and any other end names none", () => {
  const filed = withRunFrame(STARTED_RUN, "end", {
    issue: "DEMO-46",
    offers: ["undo"],
    reason: "",
    state: "done",
  });
  expect(filed.end).toEqual({ issue: "DEMO-46", offers: ["undo"], reason: "", state: "done" });
  const answered = withRunFrame(STARTED_RUN, "end", { offers: [], reason: "", state: "done" });
  expect(answered.end?.issue).toBe("");
});

// A slip - a step the model guessed that Dispatch does not have, which it mends itself - is no
// failure: its tool frame says so, and the step keeps it beside its words, never refused.
test("a tool frame of a slip keeps the slip on its step, which is not refused", () => {
  const run = framed([
    ["call", { kind: "read", label: "Trying a step Dispatch does not have", step: 2, tool: "x" }],
    [
      "tool",
      {
        error: false,
        items: [],
        kind: "read",
        label: "Tried a step Dispatch does not have",
        note: "x {}: no such tool",
        slip: true,
        step: 2,
        tool: "x",
      },
    ],
    [
      "tool",
      {
        error: true,
        items: ["DEMO-12"],
        kind: "read",
        label: "Could not read DEMO-12: you have no access",
        step: 3,
        tool: "read",
      },
    ],
  ]);
  expect(run.steps.map((step) => [step.label, step.slip, step.refused, step.running])).toEqual([
    ["Tried a step Dispatch does not have", true, false, false],
    ["Could not read DEMO-12: you have no access", false, true, false],
  ]);
  // A step that still runs, and one of a frame from before frames said slips, is no slip.
  expect(framed([call(2)]).steps[0]?.slip).toBe(false);
  expect(framed([ended(2)]).steps[0]?.slip).toBe(false);
});

// A call that drew a block is a chart as its frames name it (the label's kind), and the block
// frame the agent stores right after that call's tool frame says what it drew: the step then says
// so, so its fold names a table a table. Two drawings that run at once each take their own block.
test("the block a call drew gives its step the block's kind: a table, a chart or one number", () => {
  const drawing = (step: number): readonly [string, unknown] => [
    "call",
    { kind: "chart", label: "Drawing a block", step, tool: "call_tool" },
  ];
  const drew = (step: number, kind: string): readonly [string, unknown] => [
    "tool",
    { items: [], kind: "chart", label: `Drew a ${kind} of the work`, step, tool: "call_tool" },
  ];
  const block = (kind: string): readonly [string, unknown] => [
    "block",
    { kind, rows: [], spec: {}, title: "The work" },
  ];
  const run = framed([
    drawing(3),
    drawing(4),
    drew(4, "table"),
    block("table"),
    drew(3, "chart"),
    block("chart"),
    drawing(5),
    drew(5, "metric"),
    block("metric"),
  ]);
  expect(run.steps.map((step) => [step.step, step.kind])).toEqual([
    [3, "chart"],
    [4, "table"],
    [5, "metric"],
  ]);
  expect(run.blocks.map((drawn) => drawn.kind)).toEqual(["table", "chart", "metric"]);
  // A block no drawing step came right before changes no step's kind.
  expect(framed([ended(2), block("table")]).steps.map((step) => step.kind)).toEqual(["read"]);
});
