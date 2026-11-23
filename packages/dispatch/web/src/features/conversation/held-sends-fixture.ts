import { type MutationKey, QueryClient } from "@tanstack/react-query";

import { keptTab, memoryFiles } from "../../__tests__/kept-sends-harness";
import { ApiError } from "../../api/client";
import { Writes } from "../writes/write-store";
import { HeldSends } from "./held-sends";
import type { SentRequest } from "./send-request";

/** The sends and pages the held-send store's tests share: `held-sends.test.ts` and
 *  `held-sends.address.test.ts`. */

export const request: SentRequest = {
  anchor: undefined,
  ask: { multiple: false, options: [], urgency: "med" },
  draft: { body: "Held", mentions: [], replacement: "" },
  edit: undefined,
  kind: "comment",
  owner: { issueKey: "CORE-1", kind: "issue" },
  plainDelivery: "steer",
  replyTo: null,
  sendKey: "send-0",
};

/** A message to one session, Send's own: the request the Agents page's composer holds. */
export const agentSend: SentRequest = {
  ...request,
  draft: { body: "Status please", mentions: [], replacement: "" },
  owner: { kind: "session", sessionId: "planner-session" },
  sendKey: "send-1",
};

export const agentKey: MutationKey = ["agent-composer", "planner-session"];

/** Dispatch's verdict on a send: it is not tried again, and the composer shows it. */
export const verdict = new ApiError(400, { code: "COMMENT_INPUT", error: "the body is empty" });

/** A page's query client, signed in: the store stamps every send with this login at its press
 *  and resumes only this login's, as the app's does. */
export function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

/** The held sends of a page of `tab`, over a store of their own. */
export function heldOf(tab = keptTab(), client = signedIn(), unload?: EventTarget) {
  const writes = new Writes(client, tab.page(unload), memoryFiles());
  return { held: new HeldSends(writes), writes };
}
