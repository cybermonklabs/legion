import { afterEach, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { evalItem, evalsAnswer } from "../../__tests__/evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import type { EvalKind } from "../../api/queries";
import type { EvalListItem } from "../../api/types";
import { type EvalsScope, useEvalsViews } from "./useEvalsViews";

// What a reader of several kinds' evals views holds of each (dispatch://INFRA-2918, T33): a kind
// whose view has not moved keeps what it held when another kind's view lands, so a reader that
// compares a kind's items draws that kind again only when its own view moves.

const SCOPE: EvalsScope = { compare: null, project: "ACME", repos: [], window: { preset: 28 } };

let sockets: FakeLiveSockets;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  cleanup();
  sockets.restore();
});

/** Dispatch answers the evals view of `kind` on `socket` with `items`. */
async function answerView(socket: FakeLiveSocket, kind: EvalKind, items: EvalListItem[]) {
  const frame = await waitFor(() => {
    const sent = socket
      .subscribes()
      .find(
        (subscribe) =>
          subscribe.view.kind === "evals" &&
          new URLSearchParams(subscribe.view.query).get("kind") === kind
      );
    if (sent === undefined) throw new Error(`no ${kind} view was subscribed`);
    return sent;
  });
  act(() => socket.receive(snapshotFrame(frame.sub, evalsAnswer(items))));
}

test("a kind of one page keeps its items when another kind's view lands", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const { result } = renderHook(() => useEvalsViews(SCOPE, ["delivery", "legion"]), { wrapper });
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());

  await answerView(socket, "delivery", [evalItem("deploy-frequency", "delivery", [])]);
  await waitFor(() => expect(result.current[0]?.status).toBe("painted"));
  const held = result.current[0]?.items;
  expect(held?.map((item) => item.definition.slug)).toEqual(["deploy-frequency"]);

  await answerView(socket, "legion", [evalItem("legion-work-time", "legion", [])]);
  await waitFor(() => expect(result.current[1]?.status).toBe("painted"));
  expect(result.current[0]?.items).toBe(held);
});
