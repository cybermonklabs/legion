import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, screen, waitFor, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { answerRoute } from "../../__tests__/send-answers";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type {
  ArtifactUploadResponse,
  GoingDecisionEvent,
  GoingEvent,
  GoingProposalEvent,
  GoingWriteInput,
  GoingWriteLine,
} from "../../api/types";
import { writesOf } from "../writes/write-store";
import {
  type GoingBarSpies,
  BAR_HIT as hit,
  type OpenedStream,
  renderBar,
  spyOnGoingBar,
} from "./going-bar-fixture";
import { forgetTurns } from "./going-turns";
import { OPERATION_OPTION_ID, offerOptionId, useGoingOperations } from "./OperationRows";
import { optionId } from "./search-model";
import { forgetAnsweredRun, type GoingAnswer } from "./useGoingAnswer";
import { KEYSTROKE_DEBOUNCE_MS, MEANING_PAUSE_MS } from "./useTieredSearch";

// The bar's operations as the person meets them: the operation the decision acts on is a row of
// its own above the hits, which the highlight never moves to by itself, and the bar's submit runs
// it; a submit before the proposal prepares and runs as it lands; below the threshold the two
// best are offered and only a pick runs one; a missing argument is asked for; and with the
// person's setting on every write asks first. The clock is held, as in AnswerPanel.test.tsx.

let streams: OpenedStream[];
let settings: Mock<typeof api.myGoingSettings>;
let spies: GoingBarSpies;
type WriteAnswer = (run: string, input: GoingWriteInput, bounds: WriteBounds) => Promise<unknown>;
let written: Mock<WriteAnswer>;

/** Dispatch's line for a write of `input` that landed: a status change of the item it names. */
function landedLine(input: GoingWriteInput): GoingWriteLine {
  return {
    action: input.offer,
    before: { status: "todo" },
    fields: ["status"],
    item: `LEGION-${3 + (input.invocation ?? 0)}`,
    now: { status: "in_progress" },
    outcome: { code: "", message: "", ok: true, status: 200 },
    write_id: `write-${input.invocation ?? 0}`,
  };
}

beforeEach(() => {
  jest.useFakeTimers();
  spies = spyOnGoingBar();
  streams = spies.streams;
  settings = spyOn(api, "myGoingSettings").mockResolvedValue({ confirm_writes: false });
  written = answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes");
  written.mockImplementation(async (_run, input) => landedLine(input));
});

afterEach(() => {
  spies.restore();
  settings.mockRestore();
  forgetAnsweredRun();
  forgetTurns();
  realClock();
});

/** Types `words`, waits for their hits, and lets the pause ask the question. */
async function ask(user: UserEvent, words: string): Promise<void> {
  const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  await user.click(input);
  await user.keyboard(words);
  await tick(KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
  await tick(MEANING_PAUSE_MS - KEYSTROKE_DEBOUNCE_MS);
  await waitFor(() => expect(streams).toHaveLength(1));
}

function decision(act: boolean, actions: readonly string[]): GoingDecisionEvent {
  return {
    data: {
      act,
      actions: actions.map((action, index) => ({
        action,
        confidence: index === 0 ? 0.9 : null,
        probability: null,
      })),
      context: "named",
      display: { chart: false, choice: "prose", confidence: 0.4 },
    },
    event: "decision",
  };
}

/** A status change of LEGION-3 and LEGION-4, and a comment on LEGION-3. */
const proposal: GoingProposalEvent = {
  data: {
    offers: [
      {
        action: "dispatch_issue_update",
        invocations: [
          {
            arguments: { issue: "LEGION-3", status: "in_progress" },
            automatic: true,
            missing: null,
            target_argument: "issue",
          },
          {
            arguments: { issue: "LEGION-4", status: "in_progress" },
            automatic: true,
            missing: null,
            target_argument: "issue",
          },
        ],
        offered: true,
      },
      {
        action: "dispatch_comment",
        invocations: [
          {
            arguments: { body: "On it.", issue: "LEGION-3" },
            automatic: true,
            missing: null,
            target_argument: "issue",
          },
        ],
        offered: true,
      },
    ],
  },
  event: "proposal",
};

const ended: GoingEvent = { data: { items: 0, words: 3 }, event: "end" };

/** What each write sent: the offer, the invocation and how it came to run. */
function sent(): unknown[] {
  return written.mock.calls.map(([, input]) => input);
}

/** Hands the run's stream `frames`, in order, as the server writes them. */
function stream(...frames: GoingEvent[]): void {
  act(() => {
    for (const frame of frames) streams[0]?.send(frame);
  });
}

/** The bar's submit, from the box. */
async function submit(user: UserEvent): Promise<void> {
  await user.keyboard("{Control>}{Enter}{/Control}");
}

test("at or above the threshold the operation is a row above the hits the highlight never takes, and the submit runs each invocation at once", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update", "dispatch_comment"]), proposal, ended);
    const row = await screen.findByRole("option", {
      name: /^Run issue update on LEGION-3 and LEGION-4/,
    });
    expect(row).toHaveAttribute("id", OPERATION_OPTION_ID);
    expect(within(row).getByRole("button", { name: "Run" })).toBeInTheDocument();
    // The hit stays the head: an Enter now opens it and runs nothing.
    expect(screen.getByRole("combobox", { name: "Search" })).toHaveAttribute(
      "aria-activedescendant",
      optionId(hit)
    );
    expect(written).not.toHaveBeenCalled();

    await submit(user);

    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "auto", invocation: 0, offer: "dispatch_issue_update" },
        { approved: "auto", invocation: 1, offer: "dispatch_issue_update" },
      ])
    );
    const lines = await screen.findAllByTestId("going-write-line");
    await waitFor(() =>
      expect(lines.map((line) => line.getAttribute("data-state"))).toEqual(["done", "done"])
    );
    expect(lines[0]).toHaveTextContent("LEGION-3 · status: todo → in_progress");
    expect(lines[1]).toHaveTextContent("LEGION-4 · status: todo → in_progress");
    // The operation ran, so its row is gone and no second submit runs it again.
    expect(screen.queryByRole("option", { name: /^Run issue update/ })).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

test("a submit pressed again while the turn's writes are out, and again once they landed, writes each invocation once", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), proposal, ended);
    await screen.findByRole("option", { name: /^Run issue update/ });
    const first = Promise.withResolvers<GoingWriteLine>();
    written.mockImplementation((_run, input) =>
      input.invocation === 0 ? first.promise : Promise.resolve(landedLine(input))
    );

    await submit(user);
    // The first invocation is out, the second waits behind it on the run's line.
    await waitFor(() => expect(written).toHaveBeenCalledTimes(1));
    await submit(user);
    first.resolve(landedLine({ approved: "auto", invocation: 0, offer: "dispatch_issue_update" }));
    const lines = await screen.findAllByTestId("going-write-line");
    await waitFor(() =>
      expect(lines.map((line) => line.getAttribute("data-state"))).toEqual(["done", "done"])
    );
    await submit(user);
    await tick(1000);

    expect(sent()).toEqual([
      { approved: "auto", invocation: 0, offer: "dispatch_issue_update" },
      { approved: "auto", invocation: 1, offer: "dispatch_issue_update" },
    ]);
    expect(screen.getAllByTestId("going-write-line")).toHaveLength(2);
  } finally {
    bar.unmount();
  }
});

test("a submit pressed again after Dispatch refused one invocation makes that one again, and only it", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), proposal, ended);
    await screen.findByRole("option", { name: /^Run issue update/ });
    let refuse = true;
    written.mockImplementation(async (_run, input) => {
      if (input.invocation === 1 && refuse) {
        refuse = false;
        throw new ApiError(409, { code: "ISSUE_CLAIMED", error: "LEGION-4 is claimed" });
      }
      return landedLine(input);
    });

    await submit(user);
    await waitFor(() =>
      expect(
        screen.getAllByTestId("going-write-line").map((line) => line.getAttribute("data-state"))
      ).toEqual(["done", "refused"])
    );
    await submit(user);

    await waitFor(() =>
      expect(
        screen.getAllByTestId("going-write-line").map((line) => line.getAttribute("data-state"))
      ).toEqual(["done", "done"])
    );
    await tick(1000);
    expect(sent()).toEqual([
      { approved: "auto", invocation: 0, offer: "dispatch_issue_update" },
      { approved: "auto", invocation: 1, offer: "dispatch_issue_update" },
      { approved: "auto", invocation: 1, offer: "dispatch_issue_update" },
    ]);
  } finally {
    bar.unmount();
  }
});

test("a submit before the proposal says it is preparing and runs once the proposal lands", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]));
    await screen.findByRole("option", { name: /^Run issue update/ });

    await submit(user);

    expect(await screen.findByTestId("going-operation-preparing")).toBeInTheDocument();
    expect(written).not.toHaveBeenCalled();
    stream(proposal);
    await waitFor(() => expect(written).toHaveBeenCalledTimes(2));
    expect(screen.queryByTestId("going-operation-preparing")).not.toBeInTheDocument();
  } finally {
    bar.unmount();
  }
});

test("below the threshold the two best are offered, a submit runs neither, and a pick runs that one as the person's", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(false, ["dispatch_issue_update", "dispatch_comment"]), proposal, ended);
    const comment = await screen.findByRole("option", { name: /^Run comment on LEGION-3/ });
    expect(comment).toHaveAttribute("id", offerOptionId("dispatch_comment"));
    expect(
      screen.getByRole("option", { name: /^Run issue update on LEGION-3 and LEGION-4/ })
    ).toHaveAttribute("id", offerOptionId("dispatch_issue_update"));

    await submit(user);
    await tick(1000);
    expect(written).not.toHaveBeenCalled();

    await user.click(comment);
    await waitFor(() =>
      expect(sent()).toEqual([{ approved: "person", invocation: 0, offer: "dispatch_comment" }])
    );
  } finally {
    bar.unmount();
  }
});

test("with the person's setting on, the operation asks first and is sent confirmed once they confirm", async () => {
  settings.mockResolvedValue({ confirm_writes: true });
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), proposal, ended);
    await screen.findByRole("option", { name: /^Run issue update/ });

    await submit(user);

    const step = await screen.findByTestId("going-confirm-step");
    expect(
      within(step)
        .getAllByTestId("going-write-line")
        .map((line) => line.textContent)
    ).toEqual(["issue update LEGION-3: asks first", "issue update LEGION-4: asks first"]);
    await tick(1000);
    expect(written).not.toHaveBeenCalled();

    await user.click(within(step).getByRole("button", { name: "Confirm" }));
    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "confirmed", invocation: 0, offer: "dispatch_issue_update" },
        { approved: "confirmed", invocation: 1, offer: "dispatch_issue_update" },
      ])
    );
  } finally {
    bar.unmount();
  }
});

/** The decision's update of LEGION-3, which the words named, beside one of LEGION-4 that only an
 *  item's stored text asked for: the proposal marks it not automatic. */
const injected: GoingProposalEvent = {
  data: {
    offers: [
      {
        action: "dispatch_issue_update",
        invocations: [
          {
            arguments: { issue: "LEGION-3", status: "in_progress" },
            automatic: true,
            missing: null,
            target_argument: "issue",
          },
          {
            arguments: { issue: "LEGION-4", status: "done" },
            automatic: false,
            missing: null,
            target_argument: "issue",
          },
        ],
        offered: true,
      },
    ],
  },
  event: "proposal",
};

test("a submit runs only the invocations the proposal marked automatic, and the rest is a row the person picks", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), injected, ended);
    expect(
      await screen.findByRole("option", { name: /^Run issue update on LEGION-3(?! and)/ })
    ).toHaveAttribute("id", OPERATION_OPTION_ID);
    const offered = screen.getByRole("option", { name: /^Run issue update on LEGION-4/ });
    expect(offered).toHaveAttribute("id", offerOptionId("dispatch_issue_update"));

    await submit(user);
    await waitFor(() => expect(screen.getAllByTestId("going-write-line")).toHaveLength(1));
    await tick(1000);
    expect(sent()).toEqual([{ approved: "auto", invocation: 0, offer: "dispatch_issue_update" }]);

    // The person picks the offered write: it runs as theirs, and the one already made is not made
    // again.
    await user.click(screen.getByRole("option", { name: /^Run issue update on LEGION-4/ }));
    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "auto", invocation: 0, offer: "dispatch_issue_update" },
        { approved: "person", invocation: 1, offer: "dispatch_issue_update" },
      ])
    );
  } finally {
    bar.unmount();
  }
});

test("a pick of the offered row before any submit runs only the invocations the row names", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), injected, ended);
    const offered = await screen.findByRole("option", { name: /^Run issue update on LEGION-4/ });

    await user.click(offered);
    await waitFor(() => expect(screen.getAllByTestId("going-write-line")).toHaveLength(1));
    await tick(1000);
    expect(sent()).toEqual([{ approved: "person", invocation: 1, offer: "dispatch_issue_update" }]);
  } finally {
    bar.unmount();
  }
});

test("with the person's setting on, the confirm step lists only the automatic invocations and confirms only those", async () => {
  settings.mockResolvedValue({ confirm_writes: true });
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(decision(true, ["dispatch_issue_update"]), injected, ended);
    await screen.findByRole("option", { name: /^Run issue update on LEGION-3(?! and)/ });

    await submit(user);
    const step = await screen.findByTestId("going-confirm-step");
    expect(
      within(step)
        .getAllByTestId("going-write-line")
        .map((line) => line.textContent)
    ).toEqual(["issue update LEGION-3: asks first"]);
    await user.click(within(step).getByRole("button", { name: "Confirm" }));
    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "confirmed", invocation: 0, offer: "dispatch_issue_update" },
      ])
    );
  } finally {
    bar.unmount();
  }
});

test("an invocation missing one argument asks for it, and the write carries the value as its fill", async () => {
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(
      decision(true, ["dispatch_follow"]),
      {
        data: {
          offers: [
            {
              action: "dispatch_follow",
              invocations: [
                {
                  arguments: { action: "follow", ask: "ask-1" },
                  automatic: true,
                  missing: { field: "@session", kind: "text" },
                  target_argument: "ask",
                },
              ],
              offered: true,
            },
          ],
        },
        event: "proposal",
      } satisfies GoingProposalEvent,
      ended
    );
    await screen.findByRole("option", { name: /^Run follow on ask-1/ });

    await submit(user);

    const field = await screen.findByRole("textbox", { name: "Fill the session for ask-1" });
    await waitFor(() => expect(field).toHaveFocus());
    expect(written).not.toHaveBeenCalled();
    await user.keyboard("Planner{Enter}");
    await waitFor(() =>
      expect(sent()).toEqual([
        { approved: "auto", fill: "Planner", invocation: 0, offer: "dispatch_follow" },
      ])
    );
  } finally {
    bar.unmount();
  }
});

test("a new issue the run made is the issue its write names, with no read of it that could fail", async () => {
  const newIssueSettings = spyOn(api, "getNewIssueSettings").mockResolvedValue({
    label: "",
    session: "",
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(
    {} as ArtifactUploadResponse
  );
  // A read of the issue after its create would fail here, as a timed-out read does.
  const getIssue = spyOn(api, "getIssue").mockRejectedValue(new Error("the read timed out"));
  written.mockImplementation(
    async (_run, input) =>
      ({
        action: input.offer,
        before: null,
        fields: null,
        item: "LEGION-9",
        now: { key: "LEGION-9" },
        outcome: { code: "", message: "", ok: true, status: 201 },
        write_id: "write-issue",
      }) satisfies GoingWriteLine
  );
  const user = userOnFakeClock();
  const bar = renderBar();
  try {
    await ask(user, "astrolabe");
    stream(
      decision(false, ["dispatch_issue"]),
      {
        data: {
          offers: [
            {
              action: "dispatch_issue",
              invocations: [
                {
                  arguments: { project: "LEGION", title: "astrolabe" },
                  automatic: true,
                  missing: null,
                  target_argument: "",
                },
              ],
              offered: true,
            },
          ],
        },
        event: "proposal",
      } satisfies GoingProposalEvent,
      ended
    );

    await user.click(await screen.findByRole("option", { name: /^Make a new issue from/ }));

    await waitFor(() =>
      expect(screen.getByTestId("search-new-issue-said")).toHaveTextContent("LEGION-9 is made.")
    );
    expect(sent()).toEqual([
      {
        approved: "person",
        arguments: { project: "LEGION", title: "astrolabe" },
        invocation: 0,
        offer: "dispatch_issue",
      },
    ]);
    expect(uploadArtifact.mock.calls.map(([owner]) => owner)).toEqual([{ issue: "LEGION-9" }]);
    expect(getIssue).not.toHaveBeenCalled();
    // The issue is made, so its row opens it and nothing offers to make it again.
    expect(screen.getByRole("option", { name: /^Open LEGION-9/ })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /^Make a new issue/ })).not.toBeInTheDocument();
  } finally {
    bar.unmount();
    for (const spy of [newIssueSettings, uploadArtifact, getIssue]) spy.mockRestore();
  }
});

test("a send through the run whose delivery failed answers why, as the direct send does", async () => {
  const words = "tell the agent";
  // Each send is its own run's: a run writes each of its invocations once.
  const answerOf = (runId: string): GoingAnswer => ({
    block: undefined,
    changed: undefined,
    cut: undefined,
    decision: decision(false, ["dispatch_send_to_agent"]).data,
    end: { items: 0, words: 3 },
    error: undefined,
    items: [],
    none: false,
    proposal: {
      offers: [
        {
          action: "dispatch_send_to_agent",
          invocations: [
            {
              arguments: { session: "session-1" },
              automatic: true,
              missing: null,
              target_argument: "session",
            },
          ],
          offered: true,
        },
      ],
    },
    replayed: false,
    roster: undefined,
    runId,
    state: "ended",
    text: "",
    unreachable: undefined,
    words,
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const hook = renderHook(
    ({ runId }: { readonly runId: string }) =>
      useGoingOperations({ answer: answerOf(runId), askNow: () => false, hits: [], words }),
    {
      initialProps: { runId: "run-1" },
      wrapper: ({ children }: { readonly children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );
  // Dispatch answers the send 201 whatever became of its delivery; the line records the delivery.
  const delivered = (delivery: GoingWriteLine["delivery"], id: string) =>
    written.mockImplementation(
      async (_run, input) =>
        ({
          action: input.offer,
          before: null,
          delivery,
          fields: ["message"],
          item: id,
          now: { id },
          outcome: { code: "", message: "", ok: true, status: 201 },
          write_id: "write-send",
        }) satisfies GoingWriteLine
    );
  const send = async (runId: string) => {
    hook.rerender({ runId });
    const sendToAgent = hook.result.current.sendToAgent;
    expect(sendToAgent).toBeDefined();
    let said: string | undefined;
    await act(async () => {
      said = await sendToAgent?.(writesOf(client), {
        block: undefined,
        issue: "LEGION-3",
        session: "session-1",
        words,
      });
    });
    return said;
  };
  try {
    delivered({ error: "the session takes no aside", state: "failed" }, "m-1");
    expect(await send("run-1")).toBe("the session takes no aside");
    delivered({ error: null, state: "failed" }, "m-2");
    expect(await send("run-2")).toBe("Dispatch gave no reason.");
    delivered({ error: null, state: "delivered" }, "m-3");
    expect(await send("run-3")).toBeUndefined();
  } finally {
    hook.unmount();
    client.clear();
  }
});
