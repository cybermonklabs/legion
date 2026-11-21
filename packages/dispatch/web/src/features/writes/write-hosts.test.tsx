import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import { PendingWritesHost } from "./write-hosts";
import { plainWrite, writesOf } from "./write-store";

// The store's take-up of what a reload kept waits for every kind of write, a chunk of its own
// (`kinds.ts`), and its own ask for that chunk can fail with no reload to follow: the page being
// left, the reader staying at the leave prompt a write pressed meanwhile opens, or the tab's
// recovery reloads spent. The writes host asks for the same chunk once the page has drawn, and the
// chunk ends every take-up still waiting as it is evaluated (`kindsRegistered`), so the write
// pressed meanwhile goes out with no second press to ask again. Nothing this file imports loads
// `kinds.ts`, and each test file loads its modules afresh (`--parallel` implies `--isolate`), so
// the host's is the one load here that can evaluate it. The write is one the indicator does not
// show (`composer`): the host draws nothing, and the row reads the store alone.
test("the writes host's fetch of the kinds of write ends a take-up whose own ask failed, and the write pressed meanwhile goes out", async () => {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const send = spyOn(api, "send").mockResolvedValue(null as never);
  const writes = writesOf(client);
  let asked = 0;
  writes.resume("alice", () => {
    asked += 1;
    return Promise.reject(new TypeError("Failed to fetch dynamically imported module"));
  });
  const pressed = writes.run({
    detail: null,
    kind: plainWrite,
    label: "A comment on CORE-1",
    presentation: "composer",
    request: {
      body: { body: "On it" },
      method: "POST",
      path: "/api/v1/issues/CORE-1/comments",
    },
  });
  let view: RenderResult | undefined;
  try {
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    expect(asked).toBe(1);
    expect(send, "the take-up whose ask failed still holds the write").not.toHaveBeenCalled();

    view = render(
      <QueryClientProvider client={client}>
        <PendingWritesHost />
      </QueryClientProvider>
    );
    await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
    expect(send.mock.calls[0]?.[0]).toMatchObject({ path: "/api/v1/issues/CORE-1/comments" });
    expect(await pressed).toBeNull();
    expect(asked, "the take-up's own load was not asked for again").toBe(1);
  } finally {
    view?.unmount();
    send.mockRestore();
  }
});
