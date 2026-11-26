// The unit tests of the count of returns (C7 of the forward-only design): the classification of
// `merge-blocked`'s record, what is an answer from GitHub and what is not, the records of the
// other stated causes, how a return resolves from its records and its readings, and the verdict of
// a batch.
//
// Every case here is made: no test reaches Dispatch, GitHub or the transcript store. The two
// fixtures of `fixtures/` are entries copied from real sessions the store holds, each naming the
// session, the line and the store's load time it came from.
import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import {
  type Answer,
  type MergeBlockedRead,
  type Reading,
  type ReturnFacts,
  type ReturnLine,
  type TranscriptEntry,
  causeRecords,
  classifyMergeBlocked,
  parseSession,
  readerTwoFaults,
  resolveReturn,
  statedCauses,
  summaryOf,
  transcriptAnswers,
} from "./forward-count";

const HEAD = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678";
const OTHER_HEAD = "99887766554433221100ffeeddccbbaa99887766";
const API_HOSTS = ["api.github.com"];

/** A transcript answer: its value, the time it was read, and the head it is about. */
function transcriptAnswer(value: Answer["value"], at: string | null, head = HEAD): Answer {
  return { source: "transcript", place: "gh pr view", at, value, head };
}

/** A timeline item that counts: a removal from the merge queue, which is always `blocked`. */
function removal(at: string | null, head = HEAD): Answer {
  return { source: "timeline", place: "REMOVED_FROM_MERGE_QUEUE_EVENT", at, value: "blocked", head };
}

/** The read of one return's two sources, with the parts a case does not name left read and empty. */
function read(parts: Partial<MergeBlockedRead> = {}): MergeBlockedRead {
  return {
    head: HEAD,
    transcript: { state: "read", answers: [] },
    timeline: { state: "read", items: [] },
    ...parts,
  };
}

describe("the classification of merge-blocked's record", () => {
  // One row for each case of the plan's table: the read, the state its steps give, and the step
  // that gives it. The steps run in order and the first that gives a state decides, so each case
  // names its own step.
  const rows: { name: string; read: MergeBlockedRead; state: string; step: number }[] = [
    {
      name: "the head cannot be determined",
      read: read({ head: null, transcript: { state: "read", answers: [transcriptAnswer("blocked", "2026-11-20T10:00:00Z")] } }),
      state: "unread",
      step: 1,
    },
    {
      name: "the transcript is unread, and the timeline holds a removal from the queue",
      read: read({ transcript: { state: "unread", answers: [] }, timeline: { state: "read", items: [removal("2026-11-20T10:00:00Z")] } }),
      state: "unread",
      step: 2,
    },
    {
      name: "the transcript holds a blocked answer, and the timeline is unread",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("blocked", "2026-11-20T10:00:00Z")] },
        timeline: { state: "unread", items: [] },
      }),
      state: "established",
      step: 3,
    },
    {
      name: "a clear answer with a readable time, then a blocked answer whose time cannot be read, and the timeline unread",
      read: read({
        transcript: {
          state: "read",
          answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z"), transcriptAnswer("blocked", null)],
        },
        timeline: { state: "unread", items: [] },
      }),
      state: "established",
      step: 3,
    },
    {
      name: "the same two answers, and the timeline read, holding a removal after the clear answer's time",
      read: read({
        transcript: {
          state: "read",
          answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z"), transcriptAnswer("blocked", null)],
        },
        timeline: { state: "read", items: [removal("2026-11-20T10:30:00Z")] },
      }),
      state: "established",
      step: 3,
    },
    {
      name: "a blocked, then a clear, then a blocked answer, in transcript order, the timeline read and empty",
      read: read({
        transcript: {
          state: "read",
          answers: [
            transcriptAnswer("blocked", "2026-11-20T10:00:00Z"),
            transcriptAnswer("clear", "2026-11-20T10:10:00Z"),
            transcriptAnswer("blocked", "2026-11-20T10:20:00Z"),
          ],
        },
      }),
      state: "established",
      step: 3,
    },
    {
      name: "a clear, then a blocked answer, in transcript order",
      read: read({
        transcript: {
          state: "read",
          answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z"), transcriptAnswer("blocked", "2026-11-20T10:10:00Z")],
        },
      }),
      state: "established",
      step: 3,
    },
    {
      name: "the transcript holds no eligible answer, and the timeline is unread",
      read: read({ timeline: { state: "unread", items: [] } }),
      state: "unread",
      step: 4,
    },
    {
      name: "the transcript holds no eligible answer, and the timeline holds a removal from the queue",
      read: read({ timeline: { state: "read", items: [removal("2026-11-20T10:00:00Z")] } }),
      state: "established",
      step: 4,
    },
    {
      name: "the transcript holds no eligible answer, and the timeline is read and empty",
      read: read(),
      state: "undetermined",
      step: 4,
    },
    {
      name: "only an indeterminate answer, both sources read",
      read: read({ transcript: { state: "read", answers: [transcriptAnswer("indeterminate", "2026-11-20T10:00:00Z")] } }),
      state: "undetermined",
      step: 4,
    },
    {
      name: "a clear answer about another head, and nothing else, both sources read",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z", OTHER_HEAD)] },
      }),
      state: "undetermined",
      step: 4,
    },
    {
      name: "the transcript holds a clear answer, and the timeline is unread, with no timeline item obtained",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "unread", items: [] },
      }),
      state: "unread",
      step: 5,
    },
    {
      name: "a clear answer, and a removal from the queue after its time",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "read", items: [removal("2026-11-20T10:30:00Z")] },
      }),
      state: "established",
      step: 6,
    },
    {
      name: "a clear, then a blocked, then a clear answer, and a removal after the last answer's time",
      read: read({
        transcript: {
          state: "read",
          answers: [
            transcriptAnswer("clear", "2026-11-20T10:00:00Z"),
            transcriptAnswer("blocked", "2026-11-20T10:10:00Z"),
            transcriptAnswer("clear", "2026-11-20T10:20:00Z"),
          ],
        },
        timeline: { state: "read", items: [removal("2026-11-20T10:30:00Z")] },
      }),
      state: "established",
      step: 6,
    },
    {
      name: "a blocked answer, then a clear one, in transcript order, the timeline read and empty",
      read: read({
        transcript: {
          state: "read",
          answers: [transcriptAnswer("blocked", "2026-11-20T10:00:00Z"), transcriptAnswer("clear", "2026-11-20T10:10:00Z")],
        },
      }),
      state: "absent",
      step: 7,
    },
    {
      name: "a clear, then a blocked, then a clear answer, in transcript order, the timeline read and empty",
      read: read({
        transcript: {
          state: "read",
          answers: [
            transcriptAnswer("clear", "2026-11-20T10:00:00Z"),
            transcriptAnswer("blocked", "2026-11-20T10:10:00Z"),
            transcriptAnswer("clear", "2026-11-20T10:20:00Z"),
          ],
        },
      }),
      state: "absent",
      step: 7,
    },
    {
      name: "a clear answer, and a removal from the queue before its time",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:30:00Z")] },
        timeline: { state: "read", items: [removal("2026-11-20T10:00:00Z")] },
      }),
      state: "absent",
      step: 7,
    },
    {
      name: "a clear answer, and a removal from the queue at the same time",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "read", items: [removal("2026-11-20T10:00:00Z")] },
      }),
      state: "undetermined",
      step: 8,
    },
    {
      name: "a clear answer whose time cannot be read, and a removal from the queue",
      read: read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", null)] },
        timeline: { state: "read", items: [removal("2026-11-20T10:00:00Z")] },
      }),
      state: "undetermined",
      step: 8,
    },
  ];

  for (const row of rows) {
    test(`${row.name}: ${row.state}, by step ${row.step}`, () => {
      expect(classifyMergeBlocked(row.read)).toEqual({ state: row.state as never, step: row.step });
    });
  }

  // The case that shows the steps are ordered: read as unordered tests, the case of L clear with
  // the timeline unread and no timeline item obtained meets step 7 as "none counts" and gives
  // `absent`, where the ordered steps give `unread` at step 5.
  test("L clear with an unread timeline and no item obtained is unread, never absent", () => {
    const got = classifyMergeBlocked(
      read({
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "unread", items: [] },
      })
    );
    expect(got.state).toBe("unread");
    expect(got.state).not.toBe("absent");
    expect(got.step).toBe(5);
  });
});

/** One entry of a made session transcript, in the form the transcript store imports. */
type Line = Record<string, unknown>;

/** An assistant turn whose one tool call is `tool` with `args`. */
function call(at: string, id: string, tool: string, args: object): Line {
  return {
    type: "message",
    timestamp: at,
    message: { role: "assistant", content: [{ type: "toolCall", id, name: tool, arguments: args }] },
  };
}

/** The result of the call `id`, as the tool answered it. */
function answered(at: string, id: string, tool: string, text: string, isError = false): Line {
  return {
    type: "message",
    timestamp: at,
    message: { role: "toolResult", toolName: tool, toolCallId: id, isError, content: [{ type: "text", text }] },
  };
}

/** An assistant message of text alone. */
function said(at: string, text: string): Line {
  return { type: "message", timestamp: at, message: { role: "assistant", content: [{ type: "text", text }] } };
}

/** A delivery the session was sent, as Oh My Pi keeps one and the store imports it. */
function delivered(at: string, text: string): Line {
  return { type: "custom_message", customType: "envoy-message", timestamp: at, content: [{ type: "text", text }] };
}

/** The role's own `request_backward_move`, the call every answer that counts stands before. */
function backwardMove(at: string, id = "back-1"): Line[] {
  return [
    call(at, id, "legion", { op: "request_backward_move", to: "implementing", reason: "the head conflicts with its base" }),
    answered(at, id, "legion", "{}"),
  ];
}

/** A session file of `lines`, as the store imports one. */
function session(lines: Line[], id = "ses-made-1"): string {
  return [JSON.stringify({ type: "session", id }), ...lines.map((line) => JSON.stringify(line))].join("\n") + "\n";
}

/** GitHub's payload for the head: CONFLICTING and DIRTY, as `gh pr view --json` answers it. */
const CONFLICTING = JSON.stringify({ headRefOid: HEAD, mergeable: "CONFLICTING", mergeStateStatus: "DIRTY" });

/** The answers a made session gives, with its `request_backward_move` as the cut. */
function answersOf(lines: Line[]): Answer[] {
  const parsed = parseSession(session(lines));
  return transcriptAnswers(parsed.entries, { apiHosts: API_HOSTS });
}

/** A reason that states `merge-blocked` and a defect of the role's own, as a merger writes one. */
const BLOCK_AND_DEFECT =
  "the head conflicts with main since the base moved, and the change itself writes the cache key before it reads the entry, so a second read of one key answers the first entry";

/** The return a made session's answers give, with both sources read and an empty timeline. */
function returnOfSession(lines: Line[]): ReturnLine {
  const answers = answersOf(lines);
  const facts = made({
    reason: BLOCK_AND_DEFECT,
    mergeBlocked: { head: HEAD, transcript: { state: "read", answers }, timeline: { state: "read", items: [] } },
  });
  return returnOf(facts, { answers });
}

describe("the source of an answer: made payloads", () => {
  // In each case GitHub's JSON that reads the head CONFLICTING stands in one place, and nothing
  // else holds an answer. Each gives `undetermined`, and a `pending` return.
  const rows: { name: string; lines: Line[] }[] = [
    { name: "an assistant message", lines: [said("2026-11-20T10:00:00Z", `GitHub answers ${CONFLICTING}`)] },
    {
      name: "a tool call's arguments",
      lines: [
        call("2026-11-20T10:00:00Z", "c1", "bash", { command: `echo ${CONFLICTING} > /tmp/state.json` }),
        answered("2026-11-20T10:00:01Z", "c1", "bash", ""),
      ],
    },
    {
      name: "an envoy_publish and a dispatch comment that the role sent",
      lines: [
        call("2026-11-20T10:00:00Z", "c1", "envoy_publish", { topic: "notifications.role.architect", message: CONFLICTING }),
        answered("2026-11-20T10:00:01Z", "c1", "envoy_publish", "published"),
        call("2026-11-20T10:01:00Z", "c2", "bash", { command: `dispatch comment --issue ACME-9 --body '${CONFLICTING}'` }),
        answered("2026-11-20T10:01:01Z", "c2", "bash", "Commented"),
      ],
    },
    {
      name: "the result of an echo of the payload",
      lines: [
        call("2026-11-20T10:00:00Z", "c1", "bash", { command: `echo '${CONFLICTING}'` }),
        answered("2026-11-20T10:00:01Z", "c1", "bash", CONFLICTING),
      ],
    },
    {
      name: "the result of a cat of a file that the role wrote",
      lines: [
        call("2026-11-20T10:00:00Z", "c1", "bash", { command: "cat /tmp/state.json" }),
        answered("2026-11-20T10:00:01Z", "c1", "bash", CONFLICTING),
      ],
    },
    {
      name: "the result of a curl to a host that is not GitHub's API host",
      lines: [
        call("2026-11-20T10:00:00Z", "c1", "bash", { command: "curl -fsS https://mirror.example.com/graphql" }),
        answered("2026-11-20T10:00:01Z", "c1", "bash", CONFLICTING),
      ],
    },
    {
      name: "an Envoy message from another agent that relays the payload, with its rendered from line",
      lines: [
        delivered(
          "2026-11-20T10:00:00Z",
          `envoy:\n  to: you (ses-made-1)\n  from: ses-architect (Legion architect)\n  at: "2026-11-20T10:00:00Z"\n  message: "GitHub answers ${CONFLICTING}"`
        ),
      ],
    },
  ];

  for (const row of rows) {
    test(`${row.name} holds no answer: undetermined, and the return is pending`, () => {
      const lines = [...row.lines, ...backwardMove("2026-11-20T10:30:00Z")];
      expect(answersOf(lines)).toEqual([]);
      const ret = returnOfSession(lines);
      expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "undetermined", step: 4 }]);
      expect(resolveReturn(ret, [confirming(ret.id, "1"), confirming(ret.id, "2")]).class).toBe("pending");
    });
  }
});

describe("the source of an answer: real daemon asks, word for word", () => {
  // The daemon's ask to merge the base forward is prose that names the head, with no JSON. Each
  // fixture is one entry copied from a real session the store holds, with its provenance in the
  // file's own header line. With no other answer, each gives `undetermined`, and a `pending`
  // return. The store holds no merge-forward ask over the worker stream: the daemon publishes the
  // ask to the role's own Envoy topic alone (`record.MergeForwardAsk`), so this directory holds no
  // worker-stream fixture and the worker reported that to the lead.
  const fixtures = ["real-ask-envoy.jsonl"];

  for (const name of fixtures) {
    test(`${name} holds no answer about the head`, () => {
      const text = readFileSync(path.join(import.meta.dir, "fixtures", name), "utf8");
      const parsed = parseSession(text);
      expect(parsed.entries.length).toBeGreaterThan(0);
      // The ask names a head of its own. The classifier is asked about that head, so a case that
      // read the ask as an answer would show here.
      const askHead = /\b([0-9a-f]{40})\b/.exec(text)?.[1];
      expect(askHead).toBeDefined();
      const answers = transcriptAnswers(parsed.entries, { apiHosts: API_HOSTS });
      expect(answers).toEqual([]);
      const state = classifyMergeBlocked({
        head: askHead as string,
        transcript: { state: "read", answers },
        timeline: { state: "read", items: [] },
      });
      expect(state).toEqual({ state: "undetermined", step: 4 });
    });
  }
});

describe("the source of an answer: the merger's own reads", () => {
  test("gh pr view --json mergeStateStatus,reviewDecision reading DIRTY names no head: undetermined, and the return is pending", () => {
    const lines = [
      call("2026-11-20T10:00:00Z", "c1", "bash", { command: "gh pr view 41 --json mergeStateStatus,reviewDecision" }),
      answered("2026-11-20T10:00:01Z", "c1", "bash", JSON.stringify({ mergeStateStatus: "DIRTY", reviewDecision: "APPROVED" })),
      ...backwardMove("2026-11-20T10:30:00Z"),
    ];
    expect(answersOf(lines).map((answer) => answer.head)).toEqual([null]);
    const ret = returnOfSession(lines);
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "undetermined", step: 4 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1"), confirming(ret.id, "2")]).class).toBe("pending");
  });

  test("a refused gh pr merge names the head through its own --match-head-commit: established, by step 3", () => {
    const lines = [
      call("2026-11-20T10:00:00Z", "c1", "bash", { command: `gh pr merge 41 --squash --match-head-commit ${HEAD}` }),
      answered(
        "2026-11-20T10:00:01Z",
        "c1",
        "bash",
        "X GitHub refused the merge: Pull Request is not mergeable",
        true
      ),
      ...backwardMove("2026-11-20T10:30:00Z"),
    ];
    const answers = answersOf(lines);
    expect(answers).toEqual([
      { source: "transcript", place: "gh pr merge --match-head-commit", at: "2026-11-20T10:00:01Z", value: "blocked", head: HEAD },
    ]);
    const ret = returnOfSession(lines);
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "established", step: 3 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1")]).class).toBe("mixed");
  });

  test("a gh pr view that reads the head MERGEABLE and CLEAN is a clear answer about that head", () => {
    const lines = [
      call("2026-11-20T10:00:00Z", "c1", "bash", { command: "gh pr view 41 --json headRefOid,mergeable,mergeStateStatus" }),
      answered(
        "2026-11-20T10:00:01Z",
        "c1",
        "bash",
        JSON.stringify({ headRefOid: HEAD, mergeable: "MERGEABLE", mergeStateStatus: "CLEAN" })
      ),
      ...backwardMove("2026-11-20T10:30:00Z"),
    ];
    expect(answersOf(lines)).toEqual([
      { source: "transcript", place: "gh pr view", at: "2026-11-20T10:00:01Z", value: "clear", head: HEAD },
    ]);
  });

  test("the result of a curl to GitHub's GraphQL host that reads the head CONFLICTING is an answer", () => {
    const lines = [
      call("2026-11-20T10:00:00Z", "c1", "bash", {
        command: "curl -fsS -H \"Authorization: bearer $TOKEN\" https://api.github.com/graphql -d @query.json",
      }),
      answered(
        "2026-11-20T10:00:01Z",
        "c1",
        "bash",
        JSON.stringify({
          data: { repository: { pullRequest: { headRefOid: HEAD, mergeable: "CONFLICTING", mergeStateStatus: "DIRTY" } } },
        })
      ),
      ...backwardMove("2026-11-20T10:30:00Z"),
    ];
    const answers = answersOf(lines);
    expect(answers.map((answer) => [answer.value, answer.head])).toEqual([["blocked", HEAD]]);
    const ret = returnOfSession(lines);
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "established", step: 3 }]);
  });

  test("an answer that stands after the role's request_backward_move call counts for nothing", () => {
    const lines = [
      ...backwardMove("2026-11-20T10:00:00Z"),
      call("2026-11-20T10:10:00Z", "c1", "bash", { command: "gh pr view 41 --json headRefOid,mergeable,mergeStateStatus" }),
      answered(
        "2026-11-20T10:10:01Z",
        "c1",
        "bash",
        JSON.stringify({ headRefOid: HEAD, mergeable: "MERGEABLE", mergeStateStatus: "CLEAN" })
      ),
    ];
    expect(answersOf(lines)).toEqual([]);
    expect(returnOfSession(lines).causes).toEqual([{ cause: "merge-blocked", state: "undetermined", step: 4 }]);
  });
});

/** The reason of a return that states no cause of C7's table. */
const OWN_DEFECT = "the change's reader keeps the file open after it answers, so a second read of one path answers the first entry";

/** One return's facts, with the parts a case does not name read and empty. */
function made(parts: Partial<ReturnFacts> = {}): ReturnFacts {
  return {
    kind: "merge",
    at: "2026-11-20T11:00:00Z",
    stepStartedAt: "2026-11-20T09:00:00Z",
    head: HEAD,
    reason: OWN_DEFECT,
    reasonRead: true,
    checks: [],
    approvals: [],
    taskSteps: [
      { role: "implementer", kind: "step", type: "implementation" },
      { role: "reviewer", kind: "step", type: "review" },
    ],
    requirementIds: ["r1", "r2"],
    mergeBlocked: {
      head: HEAD,
      transcript: { state: "read", answers: [] },
      timeline: { state: "read", items: [] },
    },
    ...parts,
  };
}

/** The return line facts give, as `collect` writes it. */
function returnOf(facts: ReturnFacts, parts: Partial<ReturnLine> = {}): ReturnLine {
  return {
    id: "ACME-9/default/merge/1",
    project: "ACME",
    issue: "ACME-9",
    task: "default",
    kind: facts.kind,
    attempt: 1,
    role: facts.kind === "merge" ? "merger" : "implementer",
    session: "ses-made-1",
    at: facts.at,
    day: facts.at.slice(0, 10),
    head: facts.head,
    reason: facts.reason,
    reasonRead: facts.reasonRead,
    heldBy: ["steps"],
    sources: { transcript: facts.mergeBlocked.transcript.state, timeline: facts.mergeBlocked.timeline.state },
    causes: causeRecords(facts),
    answers: [],
    ...parts,
  };
}

/** A reading that owns the return and confirms each cause it states. */
function confirming(id: string, reader: Reading["reader"], own = true): Reading {
  return { return: id, reader, model: "made/reader", own, confirmed: { "merge-blocked": true, ci: true, "failed-check": true, news: true, "goal-check": true } };
}

/** A reading that owns the return and confirms no cause. */
function ownAlone(id: string, reader: Reading["reader"] = "1"): Reading {
  return { return: id, reader, model: "made/reader", own: true, confirmed: {} };
}

describe("the stated causes of a reason", () => {
  const inputs = { kind: "merge" as const, checkIds: ["c-units", "c-docs"], ciJobNames: ["unit-limits"], requirementIds: ["r1", "r2"] };

  test("merge-blocked, by each word the rule names", () => {
    for (const word of ["CONFLICTING", "DIRTY", "BLOCKED", "conflict", "CHANGES_REQUESTED", "requested changes", "merge queue"]) {
      expect(statedCauses({ ...inputs, reason: `GitHub says ${word} at this head.` })).toEqual(["merge-blocked"]);
    }
  });

  test("ci, by its words and by the name of a CI job", () => {
    for (const word of ["failed_checks", "merge_group", "required check", "unit-limits"]) {
      expect(statedCauses({ ...inputs, reason: `The head fails here: ${word}.` })).toEqual(["ci"]);
    }
  });

  test("failed-check names a check id of the issue's Verification document, from a retro alone", () => {
    expect(statedCauses({ ...inputs, kind: "retro", reason: "c-units has no pass at this head." })).toEqual(["failed-check"]);
    expect(statedCauses({ ...inputs, kind: "merge", reason: "c-units has no pass at this head." })).toEqual([]);
  });

  test("news names an approved new version of the design", () => {
    expect(statedCauses({ ...inputs, reason: "A person approved spec v4, which adds R6." })).toEqual(["news"]);
    expect(statedCauses({ ...inputs, reason: "Verification version 9 adds m4." })).toEqual(["news"]);
    expect(statedCauses({ ...inputs, reason: "The spec asks for two names." })).toEqual([]);
  });

  test("goal-check names a requirement id of the spec, from a retro alone", () => {
    expect(statedCauses({ ...inputs, kind: "retro", reason: "r2 is unmet at this head." })).toEqual(["goal-check"]);
    expect(statedCauses({ ...inputs, kind: "merge", reason: "r2 is unmet at this head." })).toEqual([]);
  });

  test("a reason that states two causes at once states both", () => {
    const reason = "GitHub reads the head DIRTY, and the required check unit-limits failed.";
    expect(statedCauses({ ...inputs, reason })).toEqual(["merge-blocked", "ci"]);
  });

  test("a reason that was not read states nothing", () => {
    expect(statedCauses({ ...inputs, reason: null })).toEqual([]);
  });
});

describe("the record of each other cause", () => {
  test("ci is established where a check that a CI job runs does not stand passed at the head", () => {
    const facts = made({
      reason: "The required check unit-limits failed at this head.",
      checks: [{ blockId: "c-ci", ciCheck: "unit-limits", passStands: "no" }],
    });
    expect(causeRecords(facts)).toEqual([{ cause: "ci", state: "established", step: null }]);
  });

  test("ci is absent where every check that a CI job runs stands passed at the head", () => {
    const facts = made({
      reason: "The required check unit-limits failed at this head.",
      checks: [{ blockId: "c-ci", ciCheck: "unit-limits", passStands: "yes" }],
    });
    expect(causeRecords(facts)).toEqual([{ cause: "ci", state: "absent", step: null }]);
  });

  test("ci is unread where the results read failed", () => {
    const facts = made({ reason: "The required check unit-limits failed at this head.", checks: null });
    expect(causeRecords(facts)).toEqual([{ cause: "ci", state: "unread", step: null }]);
  });

  test("failed-check is established where Dispatch holds no pass of the named check at the head", () => {
    const facts = made({
      kind: "retro",
      reason: "c-units has no pass at this head.",
      checks: [{ blockId: "c-units", ciCheck: null, passStands: "no" }],
    });
    expect(causeRecords(facts)).toEqual([{ cause: "failed-check", state: "established", step: null }]);
  });

  test("failed-check is absent where that check stands passed at the head", () => {
    const facts = made({
      kind: "retro",
      reason: "c-units has no pass at this head.",
      checks: [{ blockId: "c-units", ciCheck: null, passStands: "yes" }],
    });
    expect(causeRecords(facts)).toEqual([{ cause: "failed-check", state: "absent", step: null }]);
  });

  test("news is established where Dispatch holds an approval between the step's start and the return", () => {
    const facts = made({
      reason: "A person approved spec v4 at 10:05Z, which adds R6.",
      approvals: [{ at: "2026-11-20T10:05:00Z" }],
    });
    expect(causeRecords(facts)).toEqual([{ cause: "news", state: "established", step: null }]);
  });

  test("news is absent where the approval stands outside the step's run", () => {
    const facts = made({ reason: "A person approved spec v4, which adds R6.", approvals: [{ at: "2026-11-19T10:05:00Z" }] });
    expect(causeRecords(facts)).toEqual([{ cause: "news", state: "absent", step: null }]);
  });

  test("news is unread where the events read failed", () => {
    const facts = made({ reason: "A person approved spec v4, which adds R6.", approvals: null });
    expect(causeRecords(facts)).toEqual([{ cause: "news", state: "unread", step: null }]);
  });

  test("goal-check is established where the task's step records hold no reviewer and no acceptance", () => {
    const facts = made({
      kind: "retro",
      reason: "r2 is unmet at this head.",
      taskSteps: [{ role: "implementer", kind: "step", type: "implementation" }],
    });
    expect(causeRecords(facts)).toEqual([{ cause: "goal-check", state: "established", step: null }]);
  });

  test("goal-check is absent where the task ran a reviewer step", () => {
    const facts = made({ kind: "retro", reason: "r2 is unmet at this head." });
    expect(causeRecords(facts)).toEqual([{ cause: "goal-check", state: "absent", step: null }]);
  });

  test("every record is unread where the head could not be read", () => {
    const facts = made({
      head: null,
      reason: "GitHub reads the head DIRTY, and the required check unit-limits failed.",
      checks: [{ blockId: "c-ci", ciCheck: "unit-limits", passStands: "no" }],
      mergeBlocked: { head: null, transcript: { state: "read", answers: [] }, timeline: { state: "read", items: [] } },
    });
    expect(causeRecords(facts)).toEqual([
      { cause: "merge-blocked", state: "unread", step: 1 },
      { cause: "ci", state: "unread", step: null },
    ]);
  });

  test("a reason that was not read gives no cause at all", () => {
    const facts = made({ reason: null, reasonRead: false });
    expect(causeRecords(facts)).toEqual([]);
  });
});

/** The five DECOY returns of C7's table, each with the record its sources give. */
function decoy(number: number, parts: Partial<ReturnFacts>): ReturnLine {
  const facts = made({
    reason: "Conflict with main's new defaults behavior: src/config.ts:loadConfig still swallows parse errors and returns DEFAULTS.",
    ...parts,
  });
  return returnOf(facts, { id: `DECOY-${number}/default/merge/1`, issue: `DECOY-${number}`, project: "DECOY" });
}

describe("how a return resolves", () => {
  test("DECOY-1: the merger's own defect, with GitHub reading the head clear, counts against R23", () => {
    const ret = decoy(1, {
      reason: "the change's conflict handling loses data: resolveTree drops the base's side where both sides changed one line.",
      mergeBlocked: {
        head: HEAD,
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "read", items: [] },
      },
    });
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "absent", step: 7 }]);
    expect(resolveReturn(ret, [{ ...ownAlone(ret.id), confirmed: { "merge-blocked": false } }]).class).toBe("against-R23");
  });

  test("DECOY-2: the readers' shared word cannot make a block hold against GitHub's answer", () => {
    const ret = decoy(2, {
      mergeBlocked: {
        head: HEAD,
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "read", items: [] },
      },
    });
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "absent", step: 7 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1")]).class).toBe("against-R23");
  });

  test("DECOY-3: a block GitHub answered outside gh keeps a mixed return out of the count", () => {
    const ret = decoy(3, {
      mergeBlocked: {
        head: HEAD,
        transcript: {
          state: "read",
          answers: [{ source: "transcript", place: "curl https://api.github.com/graphql", at: "2026-11-20T10:00:00Z", value: "blocked", head: HEAD }],
        },
        timeline: { state: "read", items: [] },
      },
    });
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "established", step: 3 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1"), confirming(ret.id, "2")]).class).toBe("mixed");
  });

  test("DECOY-4: a block with no eligible answer neither clears the return nor counts it", () => {
    const ret = decoy(4, {});
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "undetermined", step: 4 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1"), confirming(ret.id, "2")]).class).toBe("pending");
  });

  test("DECOY-4 with readings that do not confirm the block counts against R23", () => {
    const ret = decoy(4, {});
    const reading: Reading = { ...ownAlone(ret.id), confirmed: { "merge-blocked": false } };
    expect(resolveReturn(ret, [reading]).class).toBe("against-R23");
  });

  test("DECOY-5: GitHub's payload in text the role wrote, or in another agent's relay, is no answer", () => {
    const ret = decoy(5, {});
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "undetermined", step: 4 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1"), confirming(ret.id, "2")]).class).toBe("pending");
  });

  test("the gate's case of two verdicts: an unread record of a confirmed block is pending", () => {
    const ret = decoy(6, {
      mergeBlocked: {
        head: HEAD,
        transcript: { state: "read", answers: [transcriptAnswer("clear", "2026-11-20T10:00:00Z")] },
        timeline: { state: "unread", items: [] },
      },
    });
    expect(ret.causes).toEqual([{ cause: "merge-blocked", state: "unread", step: 5 }]);
    expect(resolveReturn(ret, [confirming(ret.id, "1"), confirming(ret.id, "2")]).class).toBe("pending");
  });

  test("a contradicted cause: the readers confirm ci where every CI check stands passed", () => {
    const facts = made({
      reason: "The required check unit-limits failed at this head.",
      checks: [{ blockId: "c-ci", ciCheck: "unit-limits", passStands: "yes" }],
    });
    const ret = returnOf(facts);
    const reading: Reading = { return: ret.id, reader: "1", model: "made/reader", own: false, confirmed: { ci: true } };
    expect(resolveReturn(ret, [reading, { ...reading, reader: "2" }]).class).toBe("unexplained");
  });

  test("an unread record of a cause the readings confirm is pending", () => {
    const facts = made({ reason: "The required check unit-limits failed at this head.", checks: null });
    const ret = returnOf(facts);
    const reading: Reading = { return: ret.id, reader: "1", model: "made/reader", own: false, confirmed: { ci: true } };
    expect(resolveReturn(ret, [reading, { ...reading, reader: "2" }]).class).toBe("pending");
  });

  test("a goal-check return resolves goal-check and counts nothing", () => {
    const facts = made({
      kind: "retro",
      reason: "r2 is unmet at this head.",
      taskSteps: [{ role: "implementer", kind: "step", type: "implementation" }],
    });
    const ret = returnOf(facts);
    const reading: Reading = { return: ret.id, reader: "1", model: "made/reader", own: false, confirmed: { "goal-check": true } };
    expect(resolveReturn(ret, [reading, { ...reading, reader: "2" }]).class).toBe("goal-check");
  });

  test("a cause the readers confirm and the record establishes exempts the return", () => {
    const facts = made({
      reason: "The required check unit-limits failed at this head.",
      checks: [{ blockId: "c-ci", ciCheck: "unit-limits", passStands: "no" }],
    });
    const ret = returnOf(facts);
    const reading: Reading = { return: ret.id, reader: "1", model: "made/reader", own: false, confirmed: { ci: true } };
    expect(resolveReturn(ret, [reading, { ...reading, reader: "2" }]).class).toBe("exempt:ci");
  });

  test("a return with no reading is pending", () => {
    const ret = returnOf(made());
    expect(resolveReturn(ret, []).class).toBe("pending");
  });

  test("a return whose reason was not read is pending, whatever the readings say", () => {
    const ret = returnOf(made({ reason: null, reasonRead: false }));
    expect(ret.causes).toEqual([]);
    expect(resolveReturn(ret, [ownAlone(ret.id)]).class).toBe("pending");
  });
});

describe("the readers", () => {
  const ret = returnOf(
    made({ reason: "The required check unit-limits failed at this head.", checks: [{ blockId: "c-ci", ciCheck: "unit-limits", passStands: "no" }] })
  );
  const counted = returnOf(made(), { id: "ACME-8/default/merge/1", issue: "ACME-8" });

  test("a disagreement with no resolver reading is pending", () => {
    const one: Reading = { return: ret.id, reader: "1", model: "made/one", own: false, confirmed: { ci: true } };
    const two: Reading = { return: ret.id, reader: "2", model: "made/two", own: true, confirmed: { ci: true } };
    expect(resolveReturn(ret, [one, two]).class).toBe("pending");
  });

  test("the resolver's reading decides a disagreement", () => {
    const one: Reading = { return: ret.id, reader: "1", model: "made/one", own: false, confirmed: { ci: true } };
    const two: Reading = { return: ret.id, reader: "2", model: "made/two", own: true, confirmed: { ci: true } };
    const resolver: Reading = { return: ret.id, reader: "resolver", model: "made/resolver", own: true, confirmed: { ci: false } };
    expect(resolveReturn(ret, [one, two, resolver]).class).toBe("against-R23");
  });

  test("a return reader 1 leaves out of the count with no reading of reader 2 is pending", () => {
    const one: Reading = { return: ret.id, reader: "1", model: "made/one", own: false, confirmed: { ci: true } };
    expect(resolveReturn(ret, [one]).class).toBe("pending");
  });

  test("a return reader 1's reading counts needs no reading of reader 2", () => {
    expect(resolveReturn(counted, [ownAlone(counted.id)]).class).toBe("against-R23");
    expect(readerTwoFaults([counted], [ownAlone(counted.id)])).toEqual([]);
  });

  test("a reading of reader 2 on a return reader 1's reading counts is refused by name", () => {
    const faults = readerTwoFaults([counted], [ownAlone(counted.id), { ...ownAlone(counted.id), reader: "2" }]);
    expect(faults).toEqual([
      `reader 2 read ${counted.id}, which reader 1's reading counts against R23: reader 2 reads each return that reader 1 leaves out of the count, and no other`,
    ]);
  });
});

/** One resolved return, as `summaryOf` takes it. */
function resolved(id: string, issue: string, className: string) {
  return { id, issue, class: className as never };
}

describe("the summary and the verdict", () => {
  const lines = (text: string) => text.split("\n").filter((line) => line !== "");

  test("fail with one against-R23 and one pending", () => {
    const text = summaryOf({
      returns: [resolved("A/1", "ACME-1", "against-R23"), resolved("A/2", "ACME-2", "pending")],
      lastStepsPassed: 1,
      unknown: [],
    });
    expect(lines(text)).toContain("against R23: 1");
    expect(lines(text)).toContain("pending: 1");
    expect(lines(text)).toContain("verdict: fail");
    expect(lines(text)).toContain("ACME-1 against-R23");
    expect(lines(text)).toContain("ACME-2 pending");
  });

  test("fail with one against-R23 and an unread day", () => {
    const text = summaryOf({
      returns: [resolved("A/1", "ACME-1", "against-R23")],
      lastStepsPassed: 1,
      unknown: [{ project: "ACME", day: "2026-11-20" }],
    });
    expect(lines(text)).toContain("verdict: fail");
    expect(lines(text)).toContain("unknown: ACME 2026-11-20");
  });

  test("unknown with no against-R23 and one pending", () => {
    const text = summaryOf({ returns: [resolved("A/2", "ACME-2", "pending")], lastStepsPassed: 1, unknown: [] });
    expect(lines(text)).toContain("against R23: 0");
    expect(lines(text)).toContain("verdict: unknown");
  });

  test("pass with no return, every source read and a last step passed", () => {
    const text = summaryOf({ returns: [], lastStepsPassed: 1, unknown: [] });
    expect(lines(text)).toContain("last steps passed: 1");
    expect(lines(text)).toContain("unknown: none");
    expect(lines(text)).toContain("verdict: pass");
  });

  test("unknown with no return and an unread project", () => {
    const text = summaryOf({ returns: [], lastStepsPassed: 1, unknown: [{ project: "ACME", day: null }] });
    expect(lines(text)).toContain("unknown: ACME (every day of the window)");
    expect(lines(text)).toContain("verdict: unknown");
  });

  test("a project whose read failed prints unknown for it, never 0", () => {
    const text = summaryOf({ returns: [], lastStepsPassed: 0, unknown: [{ project: "ACME", day: null }] });
    expect(lines(text)).not.toContain("unknown: none");
    expect(lines(text)).toContain("unknown: ACME (every day of the window)");
  });

  test("a window with no last step passed gives verdict unknown", () => {
    const text = summaryOf({ returns: [], lastStepsPassed: 0, unknown: [] });
    expect(lines(text)).toContain("last steps passed: 0");
    expect(lines(text)).toContain("verdict: unknown");
  });

  test("each class of C7 gets a line of its own, exempt by cause", () => {
    const text = summaryOf({
      returns: [
        resolved("A/1", "ACME-1", "against-R23"),
        resolved("A/2", "ACME-2", "mixed"),
        resolved("A/3", "ACME-3", "goal-check"),
        resolved("A/4", "ACME-4", "exempt:ci"),
        resolved("A/5", "ACME-5", "unexplained"),
      ],
      lastStepsPassed: 2,
      unknown: [],
    });
    for (const line of [
      "against R23: 1",
      "mixed: 1",
      "goal-check: 1",
      "exempt merge-blocked: 0",
      "exempt ci: 1",
      "exempt failed-check: 0",
      "exempt news: 0",
      "exempt goal-check: 0",
      "unexplained: 1",
      "pending: 0",
      "verdict: fail",
    ]) {
      expect(lines(text)).toContain(line);
    }
  });
});

describe("the sources a return is held by", () => {
  test("a return the transcript store alone holds is listed with that source", () => {
    const ret = returnOf(made(), { heldBy: ["transcript"] });
    expect(ret.heldBy).toEqual(["transcript"]);
  });
});

describe("parsing a session the store imports", () => {
  test("the header names the session, and a delivery is read as a delivery", () => {
    const text = session([delivered("2026-11-20T10:00:00Z", "envoy:\n  from: dispatch\n  message: \"a notice\"")], "ses-x");
    const parsed = parseSession(text);
    expect(parsed.session).toBe("ses-x");
    const kinds = parsed.entries.map((entry: TranscriptEntry) => entry.kind);
    expect(kinds).toEqual(["delivery"]);
  });

  test("a call and its result are read with the id that joins them", () => {
    const text = session([
      call("2026-11-20T10:00:00Z", "c1", "bash", { command: "gh pr view 41 --json headRefOid" }),
      answered("2026-11-20T10:00:01Z", "c1", "bash", "{}"),
    ]);
    const parsed = parseSession(text);
    expect(parsed.entries.map((entry) => [entry.kind, entry.callId])).toEqual([
      ["call", "c1"],
      ["result", "c1"],
    ]);
  });
});
