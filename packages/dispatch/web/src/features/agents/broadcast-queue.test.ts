import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts/dispatch-api-constants";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { countEngineParses, displayTree, type EngineParses } from "../../__tests__/display-trees";
import { onFakeClock, settle } from "../../__tests__/fake-clock";
import { keptTab, keyMinted, memoryFiles } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import type {
  BroadcastCreated,
  CreateBroadcastInput,
  Message,
  MessageDelivery,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { FIRST_BACKOFF_MS, SendExpiredError } from "../writes/keyed-write";
import { Writes } from "../writes/write-store";
import { BroadcastSends } from "./BroadcastSends";
import {
  type BroadcastSend,
  type BroadcastSendRow,
  broadcastRows,
  enqueueBroadcast,
} from "./broadcast-queue";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** A press of Send: `body` to the Planner, under `key`. */
function press(body: string, key: string): BroadcastSend {
  return {
    input: { body, delivery: "btw", session_ids: ["planner-session"] },
    key,
    picked: false,
    selected: ["planner-session"],
  };
}

/** One recipient of `input`, its message's one attempt in `state`. */
function recipient(input: CreateBroadcastInput, state: MessageDelivery["state"]) {
  return {
    message: {
      author: { id: "alice", kind: "user" },
      body: input.body,
      created_at: "2026-10-25T00:00:00Z",
      deliveries: [
        {
          attempt: 1,
          created_at: "2026-10-25T00:00:00Z",
          delivery: input.delivery,
          envelope_id: null,
          error: null,
          message_id: "message-1",
          reply_id: null,
          session_id: "planner-session",
          state,
        },
      ],
      id: "message-1",
      in_reply_to: null,
      issue_key: null,
      target: "session:planner-session",
    } as Message,
    replies: [],
    session_id: "planner-session",
  };
}

/** Dispatch's answer to `input`: the broadcast it made, its one recipient's attempt `state`. */
function created(
  input: CreateBroadcastInput,
  state: MessageDelivery["state"] = "sent"
): BroadcastCreated {
  return {
    author: { id: "alice", kind: "user" },
    body: input.body,
    created_at: "2026-10-25T00:00:00Z",
    delivery: input.delivery,
    excluded: [],
    id: `broadcast-${input.body}`,
    recipients: [recipient(input, state)],
  };
}

function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

// Broadcasts go one request at a time in press order, each under its key, so a reload that
// interrupts them loses nothing: the page after it takes up every one not yet made, in press
// order and under the same keys. A made broadcast stays in the strip, sent, and nothing of it is
// kept.
test("broadcasts go in press order, survive a reload, and a made one is kept no longer", async () => {
  const tab = keptTab();
  const first = Promise.withResolvers<BroadcastCreated>();
  const createBroadcast = spyOn(api, "createBroadcast").mockReturnValueOnce(first.promise);
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    const input = request.body as CreateBroadcastInput;
    return created(input) as never;
  });
  const before = new Writes(signedIn(), tab.page(), memoryFiles());
  try {
    enqueueBroadcast(before, press("First.", "key-1"));
    enqueueBroadcast(before, press("Second.", "key-2"));
    await settle();
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    expect(broadcastRows(before.rows()).map((row) => row.status)).toEqual(["queued", "sending"]);
    expect(tab.kept("key-2")).toMatchObject({ kind: "broadcast", state: { status: "queued" } });

    const after = new Writes(signedIn(), tab.page(), memoryFiles());
    after.resume("alice");
    await settle();
    // Both are made again behind each other, in press order, each under its own key.
    expect(send.mock.calls.map(([, bounds]) => bounds?.key)).toEqual(["key-1", "key-2"]);
    expect(createBroadcast.mock.calls.slice(1).map(([, bounds]) => bounds.key)).toEqual([
      "key-1",
      "key-2",
    ]);
    await waitFor(() =>
      expect(broadcastRows(after.rows()).map((row) => row.status)).toEqual(["sent", "sent"])
    );
    expect(tab.kept("key-1")).toBeUndefined();
  } finally {
    createBroadcast.mockRestore();
    send.mockRestore();
    tab.close();
  }
});

// A broadcast made with no answer to give has nothing to link: it leaves the strip, and every
// recipient's conversation and the broadcasts are read again.
test("a broadcast answered KEY_COMMITTED leaves the strip as made", async () => {
  const client = signedIn();
  const invalidate = spyOn(client, "invalidateQueries");
  const createBroadcast = spyOn(api, "createBroadcast").mockRejectedValueOnce(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made" })
  );
  const writes = new Writes(client, keptTab().page(), memoryFiles());
  try {
    enqueueBroadcast(writes, press("First.", "key-1"));
    await waitFor(() => expect(broadcastRows(writes.rows())).toEqual([]));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["broadcast"] });
  } finally {
    createBroadcast.mockRestore();
  }
});

// W31. A broadcast Dispatch answered 201 is made, whatever its deliveries' state: it never reads as
// unconfirmed or offers Stop trying again. A recipient whose delivery is still on its way shows as
// delivering, and as delivered once the live feed's event refreshes the broadcast.
test("a broadcast answered 201 with a recipient still delivering is made and delivering, then delivered", async () => {
  const input = press("Ship it.", "key-1").input;
  const client = signedIn();
  const createBroadcast = spyOn(api, "createBroadcast").mockResolvedValueOnce(
    created(input, "pending")
  );
  const getBroadcast = spyOn(api, "getBroadcast").mockResolvedValue(created(input, "sent"));
  const writes = new Writes(client, keptTab().page(), memoryFiles());
  try {
    enqueueBroadcast(writes, press("Ship it.", "key-1"));
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent"])
    );
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    const view = render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          MemoryRouter,
          null,
          createElement(BroadcastSends, {
            onDiscard: () => {},
            onRestore: () => {},
            onRetry: () => {},
            onStop: () => {},
            restoreRefusal: null,
            rows: broadcastRows(writes.rows()),
          })
        )
      )
    );
    try {
      expect(screen.getByRole("status").textContent).toContain("delivering to 1 agent");
      expect(screen.queryByRole("button", { name: "Stop trying" })).toBeNull();
      // The recipient's delivery event refreshes every broadcast view.
      await act(async () => {
        await client.invalidateQueries({ queryKey: ["broadcast"] });
      });
      await waitFor(() => expect(screen.getByRole("status").textContent).toContain("delivered"));
      expect(screen.getByRole("status").textContent).not.toContain("delivering");
    } finally {
      view.unmount();
    }
  } finally {
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
  }
});

// A refused broadcast stays failed, holding its draft, and no longer holds the line: the next one
// goes; its Retry goes under a new key since Dispatch refused it.
test("a refused broadcast does not hold the line, and its Retry takes a new key", async () => {
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockRejectedValueOnce(new ApiError(400, { code: "BROADCAST_INPUT", error: "too long" }))
    .mockImplementation(async (input) => created(input));
  const writes = new Writes(signedIn(), keptTab().page(), memoryFiles());
  try {
    enqueueBroadcast(writes, press("First.", "key-1"));
    enqueueBroadcast(writes, press("Second.", "key-2"));
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent", "failed"])
    );
    const failed = broadcastRows(writes.rows()).find((row) => row.status === "failed");
    writes.retry(failed?.id ?? 0);
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent", "sent"])
    );
    const keys = createBroadcast.mock.calls.map(([, bounds]) => bounds.key);
    expect(keys.slice(0, 2)).toEqual(["key-1", "key-2"]);
    expect(keys[2]).not.toBe("key-1");
    // A retried send keeps its row's place in the strip.
    expect(broadcastRows(writes.rows()).map((row) => row.send.input.body)).toEqual([
      "Second.",
      "First.",
    ]);
  } finally {
    createBroadcast.mockRestore();
  }
});

// W51. A broadcast the sender's sign-out in another tab holds says so on its row, and is not
// offered Stop trying, since nothing of it is being tried; it goes on once that tab's hold lifts.
test("a broadcast held for a sign-out in another tab says so, and goes on once released", async () => {
  const first = Promise.withResolvers<BroadcastCreated>();
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockReturnValueOnce(first.promise)
    .mockImplementation(async (input) => created(input));
  const client = signedIn();
  const writes = new Writes(client, keptTab().page(), memoryFiles());
  const strip = () =>
    render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          MemoryRouter,
          null,
          createElement(BroadcastSends, {
            onDiscard: () => {},
            onRestore: () => {},
            onRetry: () => {},
            onStop: () => {},
            restoreRefusal: null,
            rows: broadcastRows(writes.rows()),
          })
        )
      )
    );
  try {
    enqueueBroadcast(writes, press("First.", "key-1"));
    enqueueBroadcast(writes, press("Second.", "key-2"));
    await settle();
    writes.holdFor("alice");
    first.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" }));
    await settle();
    expect(broadcastRows(writes.rows()).map((row) => row.signingOutElsewhere)).toEqual([
      true,
      true,
    ]);
    const held = strip();
    try {
      expect(screen.getAllByRole("status").map((status) => status.textContent)).toEqual([
        "Waiting for a sign-out in another tab: to 1 agent",
        "Waiting for a sign-out in another tab: to 1 agent",
      ]);
      expect(screen.queryByRole("button", { name: "Stop trying" })).toBeNull();
    } finally {
      held.unmount();
    }
    expect(createBroadcast).toHaveBeenCalledTimes(1);

    writes.release("alice");
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent", "sent"])
    );
  } finally {
    createBroadcast.mockRestore();
  }
});

// A broadcast whose key Dispatch has forgotten is never asked under it again. The key counts
// only once something has gone out under it - this attempt tells its request as
// `api.createBroadcast` does, then ends without Dispatch's verdict - so the attempt that would
// follow is the one the spent key stops: the row says the sending ended and, in the refusal's
// own words, that a Retry is a new broadcast which can be delivered a second time, and it
// offers Retry and Discard as a stopped row does.
test("a broadcast whose key has run out says so, with Retry and Discard", async () => {
  await onFakeClock(async () => {
    const client = signedIn();
    const createBroadcast = spyOn(api, "createBroadcast").mockImplementation((input, bounds) => {
      bounds.sent?.({ body: input, method: "POST", path: "/api/v1/broadcasts" });
      return Promise.reject(
        new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
      );
    });
    const writes = new Writes(client, keptTab().page(), memoryFiles());
    try {
      enqueueBroadcast(writes, press("Ship it.", keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS)));
      await settle();
      // The first attempt ended without Dispatch's verdict, so the broadcast waits its first
      // backoff; the attempt the spent key stops is the one after that wait.
      jest.advanceTimersByTime(FIRST_BACKOFF_MS);
      await settle();
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["failed"]);
      expect(createBroadcast).toHaveBeenCalledTimes(1);
      const view = render(
        createElement(
          QueryClientProvider,
          { client },
          createElement(
            MemoryRouter,
            null,
            createElement(BroadcastSends, {
              onDiscard: () => {},
              onRestore: () => {},
              onRetry: () => {},
              onStop: () => {},
              restoreRefusal: null,
              rows: broadcastRows(writes.rows()),
            })
          )
        )
      );
      try {
        expect(screen.getByRole("status").textContent).toBe(
          `Sending to 1 agent ended: ${new SendExpiredError().message}`
        );
        expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
        expect(screen.getByRole("button", { name: "Discard" })).toBeTruthy();
      } finally {
        view.unmount();
      }
    } finally {
      createBroadcast.mockRestore();
    }
  });
});

// A made broadcast's row draws the body Dispatch stored, from the answer the write landed with,
// beside the reading the server made of it (`body_tree`), so the page parses nothing of it. The
// tree here reads differently from the body beside it, so a row drawn from a parse would read
// `Made one.` and count a parse. A send Dispatch refused has no stored text: its row draws the
// draft it holds, which the page parses.
test("a made broadcast's row draws its answer's tree, and a refused one draws its draft", async () => {
  await warmMarkdownRenderer();
  const tree = displayTree("Made **one**, as stored.");
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockImplementationOnce(async (input) => ({ ...created(input), body_tree: tree }))
    .mockRejectedValueOnce(new ApiError(400, { code: "BROADCAST_INPUT", error: "too long" }));
  const getBroadcast = spyOn(api, "getBroadcast").mockImplementation(async () => ({
    ...created(press("Made **one**.", "key-1").input),
    body_tree: tree,
  }));
  const client = signedIn();
  const writes = new Writes(client, keptTab().page(), memoryFiles());
  const strip = (row: BroadcastSendRow | undefined) =>
    render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          MemoryRouter,
          null,
          createElement(BroadcastSends, {
            onDiscard: () => {},
            onRestore: () => {},
            onRetry: () => {},
            onStop: () => {},
            restoreRefusal: null,
            rows: row === undefined ? [] : [row],
          })
        )
      )
    );
  let parses: EngineParses | undefined;
  try {
    enqueueBroadcast(writes, press("Made **one**.", "key-1"));
    enqueueBroadcast(writes, press("Refused **two**.", "key-2"));
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["failed", "sent"])
    );
    const [refused, made] = broadcastRows(writes.rows());
    parses = countEngineParses();
    const madeView = strip(made);
    try {
      const preview = madeView.container.querySelector("[data-markdown-preview]");
      await waitFor(() => expect(preview?.textContent).toBe("Made one, as stored."));
      expect(preview?.querySelector("strong")?.textContent).toBe("one");
      await waitFor(() => expect(preview?.getAttribute("title")).toBe("Made one, as stored."));
      expect(parses.count()).toBe(0);
    } finally {
      madeView.unmount();
    }
    const refusedView = strip(refused);
    try {
      const preview = refusedView.container.querySelector("[data-markdown-preview]");
      await waitFor(() => expect(preview?.textContent).toBe("Refused two."));
      expect(preview?.querySelector("strong")?.textContent).toBe("two");
      expect(parses.count()).toBeGreaterThan(0);
    } finally {
      refusedView.unmount();
    }
  } finally {
    parses?.restore();
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
  }
});
