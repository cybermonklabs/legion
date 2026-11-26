import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { onFakeClock, settle } from "../../__tests__/fake-clock";
import { keptTab, memoryFiles } from "../../__tests__/kept-sends-harness";
import { api } from "../../api/client";
import "../writes/kinds";
import { Writes } from "../writes/write-store";
import { threadMessageWrite } from "./thread-message-write";

/** A page's query client with the reader signed in, whose login stamps each write. */
function signedIn(): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

// The store takes up the writes a reload interrupted once, as the person signs in, and drops a
// write whose kind it does not know then (`Writes.resume`). A message a box of Threads sent is
// made again after a reload only because its kind loads with the shell (`writes/kinds.ts`).
test("a message of a box of Threads a reload interrupted is made again once, under its key", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const hang = Promise.withResolvers<never>();
    // The first attempt goes out and stays out, as the page's reload finds it: what went out is
    // told to the store as the client's own send tells it (`WriteBounds.sent`).
    const send = spyOn(api, "send")
      .mockImplementationOnce((request, bounds) => {
        bounds?.sent?.(request);
        return hang.promise;
      })
      .mockResolvedValue({ id: "message-1" } as never);
    const before = new Writes(signedIn(), tab.page(), memoryFiles());
    try {
      void before
        .run({
          detail: { body: "Under the root.", issueKey: "ACME-1" },
          kind: threadMessageWrite,
          label: "Reply",
          method: "POST",
          presentation: "indicator",
          write: (bounds) =>
            api.createMessage("ACME-1", { body: "Under the root.", in_reply_to: "m-0" }, bounds),
        })
        .catch(() => {});
      await settle();
      const [key = ""] = before.rows().map((write) => write.key);
      expect(tab.kept(key)).toMatchObject({
        detail: { body: "Under the root.", issueKey: "ACME-1" },
        kind: "threads-message",
      });

      const after = new Writes(signedIn(), tab.page(), memoryFiles());
      after.resume("alice");
      await settle();
      expect(send.mock.calls.slice(1).map((call) => [call[0], call[1]?.key])).toEqual([
        [
          {
            body: { body: "Under the root.", in_reply_to: "m-0" },
            method: "POST",
            path: "/api/v1/issues/ACME-1/messages",
          },
          key,
        ],
      ]);
    } finally {
      hang.reject(new Error("the page ended"));
      send.mockRestore();
      tab.close();
    }
  });
});
