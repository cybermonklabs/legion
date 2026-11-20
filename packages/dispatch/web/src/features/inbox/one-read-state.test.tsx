/**
 * Behind the Inbox's opt-in the reader's own asks are the one read's (`useInboxAsks`), so while
 * that read has not answered the page draws its loading state, and where it failed its error,
 * rather than a list without the reader's own asks (dispatch://INFRA-1152).
 */

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { MeInboxResponse } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { Inbox } from "./Inbox";
import { issueAsk } from "./inbox-fixture";

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
});

/** The Inbox at `/?one=1`, its Inbox read holding one of alice's own asks and one of carol's, with
 *  the one read answering as `getMeInbox` does. */
function renderOneReadInbox(getMeInbox: () => Promise<MeInboxResponse>): void {
  restores.push(spyOn(api, "getCredentialPending").mockResolvedValue({ pending: [] }));
  restores.push(spyOn(api, "getMyAgentState").mockResolvedValue({}));
  restores.push(spyOn(api, "getMeInbox").mockImplementation(getMeInbox));
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxQuery().queryKey, [
    issueAsk({ id: "ask-a" }),
    issueAsk({ id: "ask-c", issue: { assignee: "carol", key: "CORE-2", title: "Carol's" } }),
  ]);
  render(
    <MemoryRouter initialEntries={["/?one=1"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Inbox />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("while the one read has not answered, the Inbox draws its loading state", async () => {
  renderOneReadInbox(() => new Promise(() => {}));
  expect(await screen.findByRole("status", { name: "Loading your inbox" })).toBeTruthy();
  expect(document.querySelector("[data-inbox-row]")).toBeNull();
});

test("where the one read failed, the Inbox says it could not load", async () => {
  renderOneReadInbox(() => Promise.reject(new ApiError(500, { error: "down" })));
  expect(await screen.findByText("Could not load your inbox.")).toBeTruthy();
  expect(document.querySelector("[data-inbox-row]")).toBeNull();
});
