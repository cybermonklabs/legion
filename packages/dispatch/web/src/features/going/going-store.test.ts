import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { waitFor } from "@testing-library/react";

import { realClock, settle } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import type { GoingRun } from "../../api/types";
import {
  asksPerson,
  continueRun,
  conversationTitle,
  draftOf,
  dropQueued,
  goingState,
  newConversation,
  newestRun,
  openConversation,
  openRun,
  queuedReady,
  rereadRun,
  resetGoingStore,
  setDraft,
  setLevel,
  setPlace,
  setUndone,
  startRun,
  stopRun,
  titleWords,
} from "./going-store";

// The bar's store (design v2, states 2 and 3 and the Keys table): Ctrl-K opens the bar on the
// conversation while its run works or its plan waits, else on the search; words sent while the
// run works start no second conversation beside it, and wait for it; the title is the first
// question; an answer asks the person a question where the frame that ended it says so; and a
// run whose stream dropped is reconnecting until the stream comes back, ends or is given up.

const CONTEXT = { path: "/projects/CORE/issues" };

function record(id: string, conversation: string, words: string): GoingRun {
  return {
    context: null,
    continues: null,
    conversation,
    created_at: new Date().toISOString(),
    finished_at: null,
    id,
    issues: [],
    level: "standard",
    line: null,
    mode: "ask",
    standing_count: 0,
    state: "queued",
    stop_reason: null,
    undoable: false,
    undoable_count: 0,
    undone_at: null,
    undone_count: 0,
    used: { calls: 0, tokens: 0, working_ms: 0 },
    words,
  };
}

/** Answers each start with the next of `records`, and holds each run's stream open, handing the
 *  test the way to send it a frame. */
function answerRuns(...records: GoingRun[]) {
  const start = spyOn(api, "createGoingRun");
  for (const made of records) start.mockResolvedValueOnce(made);
  const frames = new Map<string, (name: string, data: unknown) => void>();
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    frames.set(id, onFrame);
    return Promise.withResolvers<void>().promise;
  });
  return {
    restore: () => {
      start.mockRestore();
      stream.mockRestore();
    },
    send: (id: string, name: string, data: unknown) => frames.get(id)?.(name, data),
    start,
  };
}

const ENDED_DONE = { offers: [], reason: "", state: "done" };

afterEach(resetGoingStore);

// Design v2, state 9: Continue on a run a bound stopped is a follow-up at that run's own level,
// whatever the bar is set to now, and "Continue in Deep" names Deep; it stamps when the run ended.
test("Continue follows a bound-stopped run up at its own level, and Continue in Deep at Deep", async () => {
  const runs = answerRuns(
    record("r-1", "c-1", "split CORE-1"),
    record("r-2", "c-1", "Continue"),
    record("r-3", "c-1", "Continue")
  );
  const BOUND = { offers: ["continue"], reason: "TIME_BUDGET: …", state: "stopped" };
  try {
    await startRun("split CORE-1", CONTEXT, "alice");
    runs.send("r-1", "end", BOUND);
    expect(goingState().runs.get("r-1")?.endedAt).toBeNumber();
    setLevel("fast");
    expect(await continueRun("r-1", CONTEXT, "alice")).toBe(true);
    expect(runs.start.mock.calls[1]?.[0]).toEqual(
      expect.objectContaining({ continues: "r-1", level: "standard", words: "Continue" })
    );
    runs.send("r-2", "end", BOUND);
    expect(await continueRun("r-2", CONTEXT, "alice", "deep")).toBe(true);
    expect(runs.start.mock.calls[2]?.[0]).toEqual(
      expect.objectContaining({ continues: "r-2", level: "deep" })
    );
    // A run that a bound did not stop offers no Continue.
    runs.send("r-3", "end", ENDED_DONE);
    expect(await continueRun("r-3", CONTEXT, "alice")).toBe(false);
  } finally {
    runs.restore();
  }
});

// A second Continue while the first one's start is out, such as Continue in Deep pressed after
// Continue, starts nothing: a second follow-up would take the first start's place on the screen,
// and the run Dispatch answers the first with would join no conversation shown. Once Dispatch
// answers, that run joins the conversation.
test("Continue starts no second follow-up while a start is out, and the first one joins the conversation", async () => {
  const runs = answerRuns(record("r-1", "c-1", "split CORE-1"));
  try {
    await startRun("split CORE-1", CONTEXT, "alice");
    runs.send("r-1", "end", { offers: ["continue"], reason: "TIME_BUDGET: …", state: "stopped" });
    const answered = Promise.withResolvers<GoingRun>();
    runs.start
      .mockReturnValueOnce(answered.promise)
      .mockResolvedValueOnce(record("r-3", "c-1", "Continue"));
    const first = continueRun("r-1", CONTEXT, "alice");
    expect(goingState().starting?.follows).toBe("r-1");
    expect(await continueRun("r-1", CONTEXT, "alice", "deep")).toBe(false);
    expect(runs.start).toHaveBeenCalledTimes(2);

    answered.resolve(record("r-2", "c-1", "Continue"));
    expect(await first).toBe(true);
    expect(goingState().conversation).toEqual(["r-1", "r-2"]);
    expect(goingState().starting).toBeUndefined();
  } finally {
    runs.restore();
  }
});

// A run started from the line on top records the line pressed; a run started anywhere else - a
// follow-up typed in the box, a Continue - records none.
test("a start sends the line on top it was pressed on, and a follow-up or a Continue sends none", async () => {
  const runs = answerRuns(
    { ...record("r-1", "c-1", "mark CORE-1 started"), line: "do" },
    record("r-2", "c-1", "and CORE-2"),
    record("r-3", "c-1", "Continue")
  );
  try {
    await startRun("mark CORE-1 started", CONTEXT, "alice", { line: "do" });
    expect(runs.start.mock.calls[0]?.[0]).toEqual(
      expect.objectContaining({ line: "do", words: "mark CORE-1 started" })
    );
    expect(goingState().runs.get("r-1")?.record.line).toBe("do");
    runs.send("r-1", "end", ENDED_DONE);
    await startRun("and CORE-2", CONTEXT, "alice");
    expect(runs.start.mock.calls[1]?.[0]).toEqual(
      expect.objectContaining({ continues: "r-1", words: "and CORE-2" })
    );
    expect(runs.start.mock.calls[1]?.[0]).not.toHaveProperty("line");
    runs.send("r-2", "end", { offers: ["continue"], reason: "TIME_BUDGET: …", state: "stopped" });
    expect(await continueRun("r-2", CONTEXT, "alice")).toBe(true);
    expect(runs.start.mock.calls[2]?.[0]).not.toHaveProperty("line");
  } finally {
    runs.restore();
  }
});

test("Ctrl-K opens the bar on the conversation while its run works or its plan waits, and on the search once it ended", async () => {
  const runs = answerRuns(record("r-1", "c-1", "split CORE-1"));
  try {
    await startRun("split CORE-1", CONTEXT, "alice");
    setPlace("over");
    expect(goingState().conversation).toEqual(["r-1"]);
    setPlace("closed");

    runs.send("r-1", "plan", { plan: 1, state: "waiting" });
    runs.send("r-1", "end", { offers: [], reason: "", state: "waiting" });
    setPlace("over");
    expect(goingState().conversation).toEqual(["r-1"]);
    setPlace("closed");

    runs.send("r-1", "end", ENDED_DONE);
    setPlace("over");
    expect(goingState().conversation).toBeUndefined();
    expect(goingState().place).toBe("over");
  } finally {
    runs.restore();
  }
});

test("Ctrl-K opens the conversation of this page's run that works when the open one has ended", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  try {
    await startRun("count the moons", CONTEXT, "alice");
    // History opens another conversation, which ended, over the closed bar's place.
    const old: GoingRun = { ...record("r-0", "c-0", "chart the comets"), state: "done" };
    openConversation({ id: "c-0", newest_at: old.created_at, runs: [old] });
    runs.send("r-0", "end", ENDED_DONE);
    setPlace("closed");

    setPlace("over");
    expect(goingState().conversation).toEqual(["r-1"]);
  } finally {
    runs.restore();
  }
});

test("words sent while the run works start no run beside it: they wait for it, after any that wait already, until × or the run's end", async () => {
  const runs = answerRuns(record("r-1", "c-1", "watch the comet"));
  try {
    await startRun("watch the comet", CONTEXT, "alice");
    expect(await startRun("and the moon", CONTEXT, "alice")).toBe(false);
    expect(await startRun("then Mars", CONTEXT, "alice")).toBe(false);
    expect(runs.start).toHaveBeenCalledTimes(1);
    expect(goingState().conversation).toEqual(["r-1"]);
    expect(goingState().queued).toEqual({ run: "r-1", words: "and the moon\nthen Mars" });
    // They go only once the run no longer works.
    expect(queuedReady()).toBeUndefined();
    runs.send("r-1", "end", ENDED_DONE);
    expect(queuedReady()).toEqual({ run: "r-1", words: "and the moon\nthen Mars" });

    dropQueued();
    expect(goingState().queued).toBeUndefined();
  } finally {
    runs.restore();
  }
});

test("New lets the words that wait go with the conversation", async () => {
  const runs = answerRuns(record("r-1", "c-1", "watch the comet"));
  try {
    await startRun("watch the comet", CONTEXT, "alice");
    await startRun("and the moon", CONTEXT, "alice");
    newConversation();
    expect(goingState().queued).toBeUndefined();
  } finally {
    runs.restore();
  }
});

test("the title is the first question, from the moment of Enter, and an answer asks the person only where its frame says it waits for them", async () => {
  const { promise: answered, resolve: answer } = Promise.withResolvers<GoingRun>();
  const start = spyOn(api, "createGoingRun").mockReturnValue(answered);
  const frames = new Map<string, (name: string, data: unknown) => void>();
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((id, onFrame) => {
    frames.set(id, onFrame);
    return Promise.withResolvers<void>().promise;
  });
  const send = (id: string, name: string, data: unknown) => frames.get(id)?.(name, data);
  try {
    const started = startRun("Hand CORE-1 to Legion", CONTEXT, "alice");
    expect(goingState().starting?.words).toBe("Hand CORE-1 to Legion");
    expect(conversationTitle()).toBe("Hand CORE-1 to Legion");
    answer(record("r-1", "c-1", "Hand CORE-1 to Legion"));
    await started;
    expect(goingState().starting).toBeUndefined();
    expect(conversationTitle()).toBe("Hand CORE-1 to Legion");

    // An answer that ends in a question mark is no question to the person by itself.
    send("r-1", "reply", { labels: [], text: "Did you mean CORE-1 itself?", tools: [] });
    send("r-1", "end", ENDED_DONE);
    expect(asksPerson()).toBe(false);

    // The reply that ends the turn says the answer waits for the person.
    start.mockResolvedValueOnce(record("r-2", "c-1", "yes"));
    await startRun("yes", CONTEXT, "alice");
    send("r-2", "reply", {
      labels: [],
      text: "Which changes hand it over: a label or a status",
      tools: [],
      waits_for_person: true,
    });
    expect(asksPerson()).toBe(false);
    send("r-2", "end", ENDED_DONE);
    expect(asksPerson()).toBe(true);

    // So may the end frame.
    start.mockResolvedValueOnce(record("r-3", "c-1", "both"));
    await startRun("both", CONTEXT, "alice");
    send("r-3", "reply", { labels: [], text: "Both, or only the label", tools: [] });
    send("r-3", "end", { ...ENDED_DONE, waits_for_person: true });
    expect(asksPerson()).toBe(true);
    expect(goingState().conversation).toEqual(["r-1", "r-2", "r-3"]);
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

test("a run whose stream drops is reconnecting until the stream is answered again, a frame comes, or the stream ends or is given up", async () => {
  const start = spyOn(api, "createGoingRun")
    .mockResolvedValueOnce(record("r-1", "c-1", "watch the comet"))
    .mockResolvedValueOnce(record("r-2", "c-2", "watch the moon"));
  const streams = new Map<
    string,
    {
      readonly dropped: (dropped: boolean) => void;
      readonly frame: (name: string, data: unknown) => void;
      readonly ended: {
        readonly resolve: (value: undefined) => void;
        readonly reject: (error: Error) => void;
      };
    }
  >();
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation(
    (id, onFrame, _signal, onDropped) => {
      const ended = Promise.withResolvers<void>();
      streams.set(id, { dropped: (dropped) => onDropped?.(dropped), ended, frame: onFrame });
      return ended.promise;
    }
  );
  const reconnecting = (id: string) => goingState().runs.get(id)?.reconnecting;
  try {
    await startRun("watch the comet", CONTEXT, "alice");
    const comet = streams.get("r-1");
    expect(reconnecting("r-1")).toBe(false);

    comet?.dropped(true);
    expect(reconnecting("r-1")).toBe(true);
    comet?.dropped(false);
    expect(reconnecting("r-1")).toBe(false);

    comet?.dropped(true);
    comet?.frame("call", { kind: "read", label: "Reading CORE-1", step: 2, tool: "read" });
    expect(reconnecting("r-1")).toBe(false);

    // Given up: the run says its steps could not be read, and no longer that it reconnects.
    comet?.dropped(true);
    comet?.ended.reject(new Error("the run's stream kept ending before its run did"));
    await waitFor(() => expect(goingState().runs.get("r-1")?.unreadable).toBeDefined());
    expect(reconnecting("r-1")).toBe(false);

    // Ended while it waited to open the stream again.
    newConversation();
    await startRun("watch the moon", CONTEXT, "alice");
    const moon = streams.get("r-2");
    moon?.dropped(true);
    expect(reconnecting("r-2")).toBe(true);
    moon?.ended.resolve(undefined);
    await waitFor(() => expect(reconnecting("r-2")).toBe(false));
    expect(goingState().runs.get("r-2")?.unreadable).toBeUndefined();
  } finally {
    start.mockRestore();
    stream.mockRestore();
  }
});

// A conversation's title is its first question with a capital, as a title starts (design v3,
// states 2 and 11), however the person typed it; the question's own bubble keeps the words.
test("a conversation's title is its first question with its first letter a capital", () => {
  expect(titleWords("check the telescope work")).toBe("Check the telescope work");
  expect(titleWords("Hand CORE-1 to Legion")).toBe("Hand CORE-1 to Legion");
  expect(titleWords("ébauche du plan")).toBe("Ébauche du plan");
  expect(titleWords("")).toBe("");
});

/** A conversation History lists, of one run that ended done. */
function listed(id: string, conversation: string, words: string) {
  const run: GoingRun = { ...record(id, conversation, words), state: "done" };
  return { id: conversation, newest_at: run.created_at, runs: [run] };
}

// A start belongs to the conversation on the screen at the press. History opening another while
// it is out lets it go: the one opened shows none of it, and the run Dispatch answers joins its
// own conversation, which the chip names, never the one on the screen.
test("a follow-up whose start answers after History opened another conversation joins its own, and the one open shows none of it", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  const held = Promise.withResolvers<GoingRun>();
  try {
    await startRun("count the moons", CONTEXT, "alice");
    runs.send("r-1", "end", ENDED_DONE);
    runs.start.mockReturnValueOnce(held.promise);
    const started = startRun("and the comets", CONTEXT, "alice");
    expect(goingState().starting?.follows).toBe("r-1");

    openConversation(listed("r-9", "c-9", "chart the stars"));
    expect(goingState().starting).toBeUndefined();
    expect(conversationTitle()).toBe("Chart the stars");

    held.resolve({ ...record("r-2", "c-1", "and the comets"), continues: "r-1" });
    expect(await started).toBe(true);
    expect(goingState().conversation).toEqual(["r-9"]);
    expect(goingState().runs.get("r-2")?.startedHere).toBe(true);
    // The next Enter follows up the conversation on the screen, not the one the start was of.
    expect(newestRun()?.id).toBe("r-9");
  } finally {
    runs.restore();
  }
});

test("a start refused while its conversation is open says why, and one refused after another opened is let go with it", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  try {
    await startRun("count the moons", CONTEXT, "alice");
    runs.send("r-1", "end", ENDED_DONE);
    runs.start.mockRejectedValueOnce(new Error("the start was refused"));
    expect(await startRun("and the comets", CONTEXT, "alice")).toBe(false);
    expect(goingState().refusal).toBeDefined();

    const held = Promise.withResolvers<GoingRun>();
    runs.start.mockReturnValueOnce(held.promise);
    const started = startRun("and the comets", CONTEXT, "alice");
    openConversation(listed("r-9", "c-9", "chart the stars"));
    held.reject(new Error("the start was refused"));
    expect(await started).toBe(false);
    expect(goingState().refusal).toBeUndefined();
    expect(goingState().starting).toBeUndefined();
    expect(goingState().conversation).toEqual(["r-9"]);
  } finally {
    runs.restore();
  }
});

// New closes the conversation while its next run's start is out: the bar shows the search again,
// with nothing of that start, and its run, once Dispatch answers it, opens nothing over it.
test("New lets a start that is out go, and its run opens nothing once Dispatch answers it", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  const held = Promise.withResolvers<GoingRun>();
  try {
    await startRun("count the moons", CONTEXT, "alice");
    runs.send("r-1", "end", ENDED_DONE);
    runs.start.mockReturnValueOnce(held.promise);
    const started = startRun("and the comets", CONTEXT, "alice");
    newConversation();
    expect(goingState().starting).toBeUndefined();
    held.resolve({ ...record("r-2", "c-1", "and the comets"), continues: "r-1" });
    expect(await started).toBe(true);
    expect(goingState().conversation).toBeUndefined();
    expect(goingState().runs.has("r-2")).toBe(true);
  } finally {
    runs.restore();
  }
});

// History's Open of the conversation a start follows up keeps the start, which then joins it;
// History's Open of any other lets go of the words that waited for the run on the screen, which
// then never go, even once that run's conversation is opened again.
test("History's Open keeps a start of the conversation it opens, and lets the words that wait for another conversation's run go", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  const held = Promise.withResolvers<GoingRun>();
  try {
    await startRun("count the moons", CONTEXT, "alice");
    expect(await startRun("and the comets", CONTEXT, "alice")).toBe(false);
    expect(goingState().queued).toEqual({ run: "r-1", words: "and the comets" });
    openConversation(listed("r-9", "c-9", "chart the stars"));
    expect(goingState().queued).toBeUndefined();
    openRun("r-1");
    runs.send("r-1", "end", ENDED_DONE);
    expect(queuedReady()).toBeUndefined();

    runs.start.mockReturnValueOnce(held.promise);
    const started = startRun("and the comets", CONTEXT, "alice");
    const first = goingState().runs.get("r-1")?.record;
    openConversation({
      id: "c-1",
      newest_at: first?.created_at ?? "",
      runs: first === undefined ? [] : [first],
    });
    expect(goingState().starting?.words).toBe("and the comets");
    held.resolve({ ...record("r-2", "c-1", "and the comets"), continues: "r-1" });
    await started;
    expect(goingState().conversation).toEqual(["r-1", "r-2"]);
  } finally {
    runs.restore();
  }
});

// The chip's Open, and a notice's Open or Review, keep the words that wait and a start that is
// out only where they are of the conversation opened; any other lets them go.
test("the chip's Open keeps the words that wait and a start that is out where they are of the conversation it opens, and lets them go otherwise", async () => {
  const runs = answerRuns(
    record("r-1", "c-1", "count the moons"),
    record("r-2", "c-2", "watch the comet")
  );
  const held = Promise.withResolvers<GoingRun>();
  try {
    await startRun("count the moons", CONTEXT, "alice");
    newConversation();
    await startRun("watch the comet", CONTEXT, "alice");
    expect(await startRun("and the tail", CONTEXT, "alice")).toBe(false);
    openRun("r-2");
    expect(goingState().queued).toEqual({ run: "r-2", words: "and the tail" });
    openRun("r-1");
    expect(goingState().queued).toBeUndefined();

    runs.send("r-2", "end", ENDED_DONE);
    openRun("r-2");
    runs.start.mockReturnValueOnce(held.promise);
    const started = startRun("and the tail", CONTEXT, "alice");
    openRun("r-2");
    expect(goingState().starting?.follows).toBe("r-2");
    openRun("r-1");
    expect(goingState().starting).toBeUndefined();
    held.resolve({ ...record("r-3", "c-2", "and the tail"), continues: "r-2" });
    await started;
    expect(goingState().conversation).toEqual(["r-1"]);
  } finally {
    runs.restore();
  }
});

// Why a stop was refused belongs to the conversation of the run it would have stopped: it shows
// while that conversation is open, the chip's Open of another lets it go, and a refusal that
// lands after another was opened is let go too.
test("a refused stop says why under its run's conversation alone", async () => {
  const runs = answerRuns(
    record("r-1", "c-1", "count the moons"),
    record("r-2", "c-2", "watch the comet")
  );
  const stop = spyOn(api, "stopGoingRun");
  try {
    await startRun("count the moons", CONTEXT, "alice");
    newConversation();
    await startRun("watch the comet", CONTEXT, "alice");
    const refused = Promise.withResolvers<GoingRun>();
    stop.mockReturnValueOnce(refused.promise);
    stopRun("r-2", "alice");
    refused.reject(new Error("the stop was refused"));
    await refused.promise.catch(() => undefined);
    expect(goingState().refusal).toBeDefined();
    openRun("r-1");
    expect(goingState().refusal).toBeUndefined();

    const late = Promise.withResolvers<GoingRun>();
    stop.mockReturnValueOnce(late.promise);
    stopRun("r-1", "alice");
    openRun("r-2");
    late.reject(new Error("the stop was refused"));
    await late.promise.catch(() => undefined);
    expect(goingState().refusal).toBeUndefined();
  } finally {
    stop.mockRestore();
    runs.restore();
  }
});

// Design v2, Keys: Ctrl-K from closed opens the bar on the open conversation while the start of
// its next run is out, and while words wait for its run, however that run ended.
test("Ctrl-K opens the bar on the conversation while its next run's start is out", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  const held = Promise.withResolvers<GoingRun>();
  try {
    await startRun("count the moons", CONTEXT, "alice");
    runs.send("r-1", "end", ENDED_DONE);
    runs.start.mockReturnValueOnce(held.promise);
    const started = startRun("and the comets", CONTEXT, "alice");
    setPlace("over");
    expect(goingState().conversation).toEqual(["r-1"]);
    held.resolve({ ...record("r-2", "c-1", "and the comets"), continues: "r-1" });
    await started;
    expect(goingState().conversation).toEqual(["r-1", "r-2"]);
  } finally {
    runs.restore();
  }
});

test("Ctrl-K opens the bar on the conversation while words wait for its run", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  try {
    await startRun("count the moons", CONTEXT, "alice");
    await startRun("and the comets", CONTEXT, "alice");
    runs.send("r-1", "end", ENDED_DONE);
    setPlace("over");
    expect(goingState().conversation).toEqual(["r-1"]);
    expect(queuedReady()).toEqual({ run: "r-1", words: "and the comets" });
  } finally {
    runs.restore();
  }
});

// Ctrl-K from closed that opens another conversation - this page's run that works - lets go of
// the answer to the plan drafted and of the refusal shown under the conversation before.
test("Ctrl-K that opens another conversation lets the plan's draft and the refusal go", async () => {
  const runs = answerRuns(record("r-1", "c-1", "count the moons"));
  try {
    await startRun("count the moons", CONTEXT, "alice");
    openConversation(listed("r-0", "c-0", "chart the stars"));
    setDraft({ edits: new Map(), plan: 1, run: "r-0", unticked: new Set([0]) });
    runs.start.mockRejectedValueOnce(new Error("the start was refused"));
    await startRun("and the comets", CONTEXT, "alice");
    expect(goingState().refusal).toBeDefined();
    setPlace("closed");

    setPlace("over");
    expect(goingState().conversation).toEqual(["r-1"]);
    expect(goingState().refusal).toBeUndefined();
    expect(draftOf("r-0", 1).unticked.size).toBe(0);
  } finally {
    runs.restore();
  }
});

// A block's time is when it was read (its caption's "as of"): a run's stream read again from its
// first frame, as History's Open of a run whose stream ended early reads it, reads each block
// again, and keeps the time the block was first read, which the block holds; a block new to the
// second reading takes that reading's time.
test("a run's stream read again from its first frame keeps the time each block was first read", async () => {
  jest.useFakeTimers();
  const start = spyOn(api, "createGoingRun").mockResolvedValue({
    ...record("r-1", "c-1", "chart the moons"),
    state: "working",
  });
  const readings: {
    readonly frame: (name: string, data: unknown) => void;
    readonly end: () => void;
  }[] = [];
  const stream = spyOn(api, "streamGoingRunEvents").mockImplementation((_id, onFrame) => {
    const ended = Promise.withResolvers<void>();
    readings.push({ end: () => ended.resolve(), frame: onFrame });
    return ended.promise;
  });
  const block = (title: string) => ({ kind: "table", rows: [], spec: {}, title });
  try {
    await startRun("chart the moons", CONTEXT, "alice");
    const firstRead = Date.now();
    readings[0]?.frame("block", block("Moons by orbit"));
    expect(goingState().runs.get("r-1")?.blocksAt).toEqual([firstRead]);
    // The stream ends before the run did, so History's Open reads it again from its first frame.
    readings[0]?.end();
    await settle();

    jest.advanceTimersByTime(120_000);
    const known = goingState().runs.get("r-1")?.record;
    openConversation({
      id: "c-1",
      newest_at: known?.created_at ?? "",
      runs: known === undefined ? [] : [known],
    });
    expect(readings).toHaveLength(2);
    readings[1]?.frame("block", block("Moons by orbit"));
    expect(goingState().runs.get("r-1")?.blocksAt).toEqual([firstRead]);
    readings[1]?.frame("block", block("Moons by size"));
    expect(goingState().runs.get("r-1")?.blocksAt).toEqual([firstRead, Date.now()]);
  } finally {
    start.mockRestore();
    stream.mockRestore();
    realClock();
  }
});

// What Undo did and can still do of a run is Dispatch's record of it, which moves as the rows of
// the run's plan run and as an Undo lands (class search, entries 36 and 55). The page reads the
// record again at each of those, so two reads can be out at once: the record held is the one the
// newest read answered, whichever of them answers last.
test("a run's record read again is the newest read's, though an older read answers after it", async () => {
  const runs = answerRuns(record("r-1", "c-1", "raise CORE-1"));
  const older = Promise.withResolvers<GoingRun>();
  const newer = Promise.withResolvers<GoingRun>();
  const read = spyOn(api, "goingRun")
    .mockReturnValueOnce(older.promise)
    .mockReturnValueOnce(newer.promise);
  const ran: GoingRun = {
    ...record("r-1", "c-1", "raise CORE-1"),
    state: "done",
    undoable: true,
    undoable_count: 1,
  };
  const undone: GoingRun = {
    ...ran,
    undoable: false,
    undoable_count: 0,
    undone_at: "2026-11-18T11:35:00Z",
    undone_count: 1,
  };
  try {
    await startRun("raise CORE-1", CONTEXT, "alice");
    // The plan's rows ran, and then an Undo of the run answered: one read for each.
    rereadRun("r-1");
    setUndone("r-1", []);
    expect(read.mock.calls).toEqual([["r-1"], ["r-1"]]);
    newer.resolve(undone);
    await waitFor(() => expect(goingState().runs.get("r-1")?.record).toEqual(undone));
    older.resolve(ran);
    await settle();
    expect(goingState().runs.get("r-1")?.record).toEqual(undone);
    // A run the page does not hold has no record here to read again.
    rereadRun("r-9");
    expect(read).toHaveBeenCalledTimes(2);
  } finally {
    read.mockRestore();
    runs.restore();
  }
});
