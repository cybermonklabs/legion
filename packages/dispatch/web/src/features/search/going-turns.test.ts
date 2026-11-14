import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";

import { answerRoute } from "../../__tests__/send-answers";
import { ApiError, api, type WriteBounds } from "../../api/client";
import type { GoingUndoAnswer, GoingWriteInput, GoingWriteLine } from "../../api/types";
import { Writes } from "../writes/write-store";
import {
  forgetTurns,
  type GoingWriteSpec,
  runGoingWrites,
  turnOf,
  undoGoingTurn,
  undoNewestGoingTurn,
  useLastUndo,
} from "./going-turns";

// The bar's turns as the store holds them: one write per invocation on the run's own line, each
// with its line; a refusal in the line of the write it refused; an undo that stops a write still
// waiting before it is sent, waits for the one out, and then has Dispatch undo the rest; and the
// undo key, which undoes the newest undoable run Dispatch holds where this page has none out.

afterEach(forgetTurns);

function signedIn(): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

const RUN = "8b0c8f4e-5a7e-4f43-9a5c-1a2b3c4d5e6f";

/** Dispatch's line for a status change of `item`. */
function statusLine(item: string, invocation: number): GoingWriteLine {
  return {
    action: "dispatch_issue_update",
    before: { status: "todo" },
    fields: ["status"],
    item,
    now: { status: "in_progress" },
    outcome: { code: "", message: "", ok: true, status: 200 },
    write_id: `write-${invocation}`,
  };
}

/** The three status changes a confident turn makes, one per issue. */
function statusChanges(): GoingWriteSpec[] {
  return ["CORE-1", "CORE-2", "CORE-3"].map((issue, invocation) => ({
    action: "dispatch_issue_update",
    approved: "auto",
    arguments: { issue, status: "in_progress" },
    invocation,
    runId: RUN,
    target: "issue",
    words: "start the three CORE issues",
  }));
}

type WriteAnswer = (run: string, input: GoingWriteInput, bounds: WriteBounds) => Promise<unknown>;

test("each invocation is one write of the run, sent as it came to run, and its line names the item, the field, before and now", async () => {
  const writes = new Writes(signedIn());
  const route = answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes");
  route.mockImplementation(async (_run, input) =>
    statusLine(`CORE-${(input.invocation ?? 0) + 1}`, input.invocation ?? 0)
  );

  await runGoingWrites(writes, statusChanges());

  expect(route.mock.calls.map(([run, input]) => [run, input])).toEqual([
    [RUN, { approved: "auto", invocation: 0, offer: "dispatch_issue_update" }],
    [RUN, { approved: "auto", invocation: 1, offer: "dispatch_issue_update" }],
    [RUN, { approved: "auto", invocation: 2, offer: "dispatch_issue_update" }],
  ]);
  const lines = turnOf(RUN)?.lines ?? [];
  expect(
    lines.map((line) => [line.state, line.target, line.written?.item, line.written?.fields])
  ).toEqual([
    ["done", "CORE-1", "CORE-1", ["status"]],
    ["done", "CORE-2", "CORE-2", ["status"]],
    ["done", "CORE-3", "CORE-3", ["status"]],
  ]);
});

test("a line names a document edit's document while it is out, and a new issue's or project's key once it is made", async () => {
  const writes = new Writes(signedIn());
  const held = Promise.withResolvers<GoingWriteLine>();
  const made = (action: string, item: string, invocation: number): GoingWriteLine => ({
    action,
    before: null,
    fields: [action === "dispatch_doc_edit" ? "content" : action.replace("dispatch_", "")],
    item,
    now: { id: item },
    outcome: { code: "", message: "", ok: true, status: 201 },
    write_id: `write-${invocation}`,
  });
  answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes").mockImplementation(
    (_run, input) =>
      input.offer === "dispatch_doc_edit"
        ? held.promise
        : Promise.resolve(
            made(input.offer, input.offer === "dispatch_issue" ? "DEMO-9" : "NEWP", 0)
          )
  );
  const spec = (action: string, args: Record<string, unknown>, target: string): GoingWriteSpec => ({
    action,
    approved: "person",
    arguments: args,
    invocation: 0,
    runId: RUN,
    target,
    words: "edit the spec and make DEMO's issue and project",
  });

  // Dispatch names a document edit's item by its document (`target_argument` "artifact"), and a
  // write that makes its item by nothing until it is made.
  const written = runGoingWrites(writes, [
    spec("dispatch_doc_edit", { artifact: "spec", project: "DEMO" }, "artifact"),
    spec("dispatch_issue", { project: "DEMO", title: "A new one" }, ""),
    spec("dispatch_project", { key: "NEWP", name: "New" }, ""),
  ]);
  await waitFor(() => expect(turnOf(RUN)?.lines).toHaveLength(3));
  expect(turnOf(RUN)?.lines.map((line) => [line.action, line.state, line.target])).toEqual([
    ["dispatch_doc_edit", "sending", "spec"],
    ["dispatch_issue", "sending", ""],
    ["dispatch_project", "sending", ""],
  ]);

  held.resolve(made("dispatch_doc_edit", "doc-1", 0));
  await written;
  expect(turnOf(RUN)?.lines.map((line) => [line.action, line.state, line.written?.item])).toEqual([
    ["dispatch_doc_edit", "done", "doc-1"],
    ["dispatch_issue", "done", "DEMO-9"],
    ["dispatch_project", "done", "NEWP"],
  ]);
});

test("a write Dispatch refuses says its refusal in its own line, and the others land", async () => {
  const writes = new Writes(signedIn());
  answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes").mockImplementation(
    async (_run, input) => {
      if (input.invocation === 1) {
        throw new ApiError(404, { code: "ISSUE_NOT_FOUND", error: "no issue CORE-2" });
      }
      return statusLine(`CORE-${(input.invocation ?? 0) + 1}`, input.invocation ?? 0);
    }
  );

  await runGoingWrites(writes, statusChanges());

  const lines = turnOf(RUN)?.lines ?? [];
  expect(lines.map((line) => line.state)).toEqual(["done", "refused", "done"]);
  expect(lines[1]?.refusal).toEqual({ code: "ISSUE_NOT_FOUND", message: "no issue CORE-2" });
});

test("an undo stops a write still waiting before it is sent, waits for the one out, then undoes the turn", async () => {
  const writes = new Writes(signedIn());
  const held = Promise.withResolvers<GoingWriteLine>();
  const route = answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes");
  route.mockImplementation((_run, input) =>
    input.invocation === 0
      ? held.promise
      : Promise.resolve(statusLine("CORE-2", input.invocation ?? 0))
  );
  const undo = answerRoute<(run: string, input: unknown) => Promise<GoingUndoAnswer>>(
    "POST",
    "/api/v1/going/runs/*/undo"
  );
  undo.mockResolvedValue({
    lines: [
      {
        action: "dispatch_issue_update",
        fields: [{ field: "status", message: "", ok: true }],
        item: "CORE-1",
        now: { status: "todo" },
        outcome: { code: "", message: "", ok: true, status: 200 },
        undo: "write_back",
        write_id: "write-0",
      },
    ],
  });

  const written = runGoingWrites(writes, statusChanges());
  // The first is out; the two behind it wait their turn on the run's line.
  await Promise.resolve();
  const undone = undoGoingTurn(writes, RUN);
  expect(turnOf(RUN)?.undo).toMatchObject({ state: "undoing", stopped: 2 });
  expect(undo).not.toHaveBeenCalled();

  held.resolve(statusLine("CORE-1", 0));
  await Promise.all([written, undone]);

  expect(route.mock.calls.map(([, input]) => input.invocation)).toEqual([0]);
  expect(undo).toHaveBeenCalledWith(
    RUN,
    { reason: "Undone from the command bar: “start the three CORE issues”" },
    expect.anything()
  );
  const turn = turnOf(RUN);
  expect(turn?.lines.map((line) => line.state)).toEqual(["done", "stopped", "stopped"]);
  expect(turn?.lines[0]?.undo?.undo).toBe("write_back");
  expect(turn?.undo?.state).toBe("undone");
});

test("the undo key undoes the newest undoable run Dispatch holds, where this page has no write of the bar out", async () => {
  const writes = new Writes(signedIn());
  const runs = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 1,
    offset: 0,
    rows: [
      {
        author_login: "alice",
        context: null,
        created_at: "2026-11-12T10:00:00Z",
        decision: null,
        finished: true,
        finished_at: "2026-11-12T10:00:02Z",
        hits: [],
        id: RUN,
        issues: ["CORE-1"],
        proposal: null,
        structured_output: null,
        undos: [],
        words: "start CORE-1",
        writes: [],
      },
    ],
    total: 1,
  });
  const undo = answerRoute<(run: string, input: unknown) => Promise<GoingUndoAnswer>>(
    "POST",
    "/api/v1/going/runs/*/undo"
  );
  undo.mockResolvedValue({
    lines: [
      {
        action: "dispatch_issue_update",
        fields: [{ field: "status", message: "status is in_review now", ok: false }],
        item: "CORE-1",
        now: { status: "in_review" },
        outcome: {
          code: "GOING_UNDO_CHANGED",
          message: "another writer changed it",
          ok: false,
          status: 409,
        },
        undo: "write_back",
        write_id: "write-0",
      },
    ],
  });
  try {
    await undoNewestGoingTurn(writes);

    expect(runs).toHaveBeenCalledWith({ limit: 1, undoable: true });
    expect(undo).toHaveBeenCalledWith(
      RUN,
      { reason: "Undone from the command bar: “start CORE-1”" },
      expect.anything()
    );
    // What the shell's corner reads, through the hook it reads it with.
    const hook = renderHook(() => useLastUndo());
    const said = hook.result.current;
    hook.unmount();
    expect(said?.state).toBe("undone");
    expect(said?.words).toBe("start CORE-1");
    expect(said?.lines[0]?.fields?.[0]?.message).toBe("status is in_review now");
  } finally {
    runs.mockRestore();
  }
});

test("the undo key pressed while a turn's Undo is out follows that undo, sends no second one, and works again once it settles", async () => {
  const writes = new Writes(signedIn());
  const route = answerRoute<WriteAnswer>("POST", "/api/v1/going/runs/*/writes");
  route.mockImplementation(async (_run, input) =>
    statusLine(`CORE-${(input.invocation ?? 0) + 1}`, input.invocation ?? 0)
  );
  await runGoingWrites(writes, statusChanges());
  const held = Promise.withResolvers<GoingUndoAnswer>();
  const undo = answerRoute<(run: string, input: unknown) => Promise<GoingUndoAnswer>>(
    "POST",
    "/api/v1/going/runs/*/undo"
  );
  undo.mockImplementation(() => held.promise);
  const runs = spyOn(api, "myGoingRuns").mockResolvedValue({
    limit: 1,
    offset: 0,
    rows: [],
    total: 0,
  });
  const said = () => {
    const hook = renderHook(() => useLastUndo());
    const last = hook.result.current;
    hook.unmount();
    return last;
  };
  try {
    // The turn's own Undo, out and not yet answered.
    const button = undoGoingTurn(writes, RUN, "start the three CORE issues");
    await waitFor(() => expect(undo).toHaveBeenCalledTimes(1));

    // The key, pressed meanwhile: the shell follows the undo that is out, and nothing is sent.
    await undoNewestGoingTurn(writes);
    expect(undo).toHaveBeenCalledTimes(1);
    expect(said()).toMatchObject({ runId: RUN, state: "undoing" });

    held.resolve({ lines: [] });
    await button;
    expect(said()).toMatchObject({ runId: RUN, state: "undone" });

    // Settled, the key works again: it asks Dispatch for the newest undoable run.
    await undoNewestGoingTurn(writes);
    expect(runs).toHaveBeenCalledWith({ limit: 1, undoable: true });
    expect(said()).toMatchObject({ runId: "", state: "undone" });
    expect(undo).toHaveBeenCalledTimes(1);
  } finally {
    runs.mockRestore();
  }
});
