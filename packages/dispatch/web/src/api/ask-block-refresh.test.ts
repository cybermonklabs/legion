import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { eventRefreshTargets } from "./event-refresh";
import { refreshQueries } from "./query-refresh";
import type { DispatchEvent } from "./types";

/**
 * What refreshes the read a document's ask blocks take their answer state, answer, answerer,
 * time and a copy's source from: the asks of the document's owner, `["asks", <issue>]` for an
 * issue's documents and `["artifact", <id>, "asks"]` for a project document's, and, for a copy on
 * a project document, whose source no route lists, the document's live text
 * (`["artifact", <id>, "text"]`), which renders it. A person's answer is held on the ask alone and
 * changes no byte of the document, so the card learns of it through the ask's own events - an
 * answer, a resolution, an opening - and never through the document.
 */

/** An event of `type` about the ask `id`, whose block is on the document `artifact`. */
function askEvent(type: string, artifact: string, issueKey: string | null): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    artifact_id: issueKey === null ? artifact : null,
    created_at: "2026-11-16T00:00:00Z",
    id: 1,
    issue_key: issueKey,
    notify: true,
    payload: { block_artifact: { id: artifact }, block_id: "q1", id: "ask-1" },
    project: "CORE",
    seq: 1,
    type,
  } as unknown as DispatchEvent;
}

/** Whether a target of the event's refresh reads key, by prefix or exactly. */
function refreshes(event: DispatchEvent, key: readonly unknown[]): boolean {
  return eventRefreshTargets(event).some(
    (target) =>
      target.queryKey.length <= key.length &&
      target.queryKey.every((part, index) => part === key[index]) &&
      (target.exact !== true || target.queryKey.length === key.length)
  );
}

test.each([
  "ask.answered",
  "ask.resolved",
  "ask.opened",
])("%s refreshes the asks an issue document's ask blocks read", (type) => {
  expect(refreshes(askEvent(type, "artifact-1", "CORE-1"), ["asks", "CORE-1"])).toBe(true);
});

test.each([
  "ask.answered",
  "ask.resolved",
  "ask.opened",
])("%s refreshes the asks a project document's ask blocks read", (type) => {
  expect(refreshes(askEvent(type, "artifact-2", null), ["artifact", "artifact-2", "asks"])).toBe(
    true
  );
});

test.each([
  "ask.answered",
  "ask.resolved",
  "ask.opened",
])("%s refreshes every project document's live text whose asks hold a copy of an ask, and no other", (type) => {
  const queryClient = new QueryClient();
  const copy = ["artifact", "artifact-3", "text"];
  const plain = ["artifact", "artifact-4", "text"];
  queryClient.setQueryData(copy, {
    markdown: ":::ask{#q1}\nQ?\n:::\n",
    token: "sha256:copy",
    version: 1,
    asks: [
      {
        block_id: "q1",
        state: "answered",
        copied_from: "ask-1",
        copied_from_document: "dispatch://CORE/artifact/plan",
      },
    ],
  });
  // A text whose own words name the attribute, beside asks that hold no copy, is no copy.
  queryClient.setQueryData(plain, {
    markdown: ":::ask{#q2}\nWhat does copied_from= mean?\n:::\n",
    token: "sha256:plain",
    version: 1,
    asks: [{ block_id: "q2", state: "open" }],
  });
  refreshQueries(queryClient, eventRefreshTargets(askEvent(type, "artifact-2", null)));
  expect(queryClient.getQueryState(copy)?.isInvalidated).toBe(true);
  expect(queryClient.getQueryState(plain)?.isInvalidated).toBe(false);
});

test.each([
  "ask.answered",
  "ask.resolved",
  "ask.opened",
])("%s refreshes every live text a Dispatch of the release before renders a copy of an ask in, and no other", (type) => {
  const queryClient = new QueryClient();
  const copy = ["artifact", "artifact-3", "text"];
  const plain = ["artifact", "artifact-4", "text"];
  queryClient.setQueryData(copy, {
    markdown:
      ':::ask{#q1 state="answered" copied_from="ask-1" copied_from_document="dispatch://CORE/artifact/plan"}\nQ?\n:::\n',
    token: "sha256:copy",
    version: 1,
  });
  queryClient.setQueryData(plain, {
    markdown: ":::ask{#q2}\nQ?\n:::\n",
    token: "sha256:plain",
    version: 1,
  });
  refreshQueries(queryClient, eventRefreshTargets(askEvent(type, "artifact-2", null)));
  expect(queryClient.getQueryState(copy)?.isInvalidated).toBe(true);
  expect(queryClient.getQueryState(plain)?.isInvalidated).toBe(false);
});
