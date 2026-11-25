import { spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { installFakeLiveSockets } from "./__tests__/live-socket-harness";
import { answerIssuePatches } from "./__tests__/send-answers";
import { ApiError, api, NetworkError } from "./api/client";
import { liveSocketFor } from "./api/live-socket";
import { AuthGate } from "./app";
import { answerOpenAsks } from "./features/inbox/inbox-fixture";
import type { UnsentChange } from "./features/shell/sign-in-channel";
import { plainWrite, writesOf } from "./features/writes/write-store";

/** The app, alice's writes and the other tabs of this browser, as the sign-out rows drive them:
 *  `app-sign-out.test.tsx`, `app-sign-out.other-tab.test.tsx` and the sign-in draft matrix. */

/** The app at `at` (`/` unless named), its whoami answered by `whoAmI` and its sign-out by
 *  `logout`, with window's confirm answered by `confirm` and every question it asked recorded.
 *  It runs on `queryClient`, or, unless one is named, on TanStack's own client retrying nothing. */
export function renderApp({
  at = "/",
  confirm = () => true,
  logout = async () => ({ ok: true as const }),
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } }),
  whoAmI,
}: {
  at?: string;
  confirm?: () => boolean;
  logout?: () => Promise<{ ok: true }>;
  queryClient?: QueryClient;
  whoAmI: () => Promise<{ event_head: string; kind: "user"; login: string }>;
}) {
  const sockets = installFakeLiveSockets();
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const originalConfirm = window.confirm;
  const asked: string[] = [];
  window.confirm = (message?: string) => {
    asked.push(message ?? "");
    return confirm();
  };
  const spies = [
    spyOn(api, "whoAmI").mockImplementation(whoAmI),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    answerOpenAsks([]),
  ];
  // A write this tab keeps is resumed from its stored request through `api.send`, which no spy
  // above covers. It is answered here as alice's own attempts answer: failing at the connection,
  // so a resumed attempt reaches no network and the write stands exactly where it stood.
  answerIssuePatches().mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
  );
  const logoutSpy = spyOn(api, "logout").mockImplementation(logout);
  const view = render(
    <MemoryRouter initialEntries={[at]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    asked,
    logout: logoutSpy,
    queryClient,
    teardown: () => {
      view.unmount();
      liveSocketFor(queryClient).close();
      sockets.restore();
      for (const spy of spies) spy.mockRestore();
      logoutSpy.mockRestore();
      window.matchMedia = originalMatchMedia;
      window.confirm = originalConfirm;
    },
  };
}

/** A write alice pressed: one whose attempts keep failing at the connection, so it is tried
 *  again and again, or - `refuse` - one Dispatch refused, waiting for her Retry. Each attempt is
 *  recorded. */
export function aliceWrite(
  queryClient: QueryClient,
  attempts: string[],
  label: string,
  refuse = false
): Promise<unknown> {
  // The press is the person's: what it starts on the page lands inside `act`, as a press does.
  let pressed: Promise<unknown> = Promise.resolve();
  act(() => {
    pressed = writesOf(queryClient)
      .run({
        detail: null,
        kind: plainWrite,
        label,
        method: "PATCH",
        presentation: "indicator",
        write: (bounds) => {
          attempts.push(label);
          bounds.sent?.({
            body: { status: "done" },
            method: "PATCH",
            path: "/api/v1/issues/CORE-1",
          });
          return Promise.reject(
            refuse
              ? new ApiError(422, { code: "ISSUE_INPUT", error: "refused" })
              : new NetworkError("/api/v1/issues/CORE-1", new TypeError("Failed to fetch"))
          );
        },
      })
      .catch((error: unknown) => error);
  });
  return pressed;
}

/** Another tab of this browser on the sign-in channel, as a test drives it. */
export interface OtherTab {
  readonly close: () => void;
  readonly say: (message: object) => void;
  readonly told: unknown[];
}

/** Another tab of this browser on the sign-in channel, named `id`: it says it opened, records
 *  everything it hears, and answers a sign-out's question with `unsent` - or, `silent`, not at
 *  all. `say` posts a message of its own, naming it; `close` says it went, once however often it
 *  is called, so a test can close it in its `finally` as well as in its steps. */
export function otherTab(id: string, unsent: readonly UnsentChange[] | "silent" = []): OtherTab {
  const channel = new BroadcastChannel("dispatch-sign-in");
  const told: unknown[] = [];
  let closed = false;
  channel.addEventListener("message", (event: MessageEvent) => {
    told.push(event.data);
    const data: unknown = event.data;
    if (typeof data !== "object" || data === null || unsent === "silent") return;
    const ask: unknown = Reflect.get(data, "ask");
    if (typeof ask === "string") channel.postMessage({ answer: ask, tab: id, unsent });
  });
  channel.postMessage({ hello: true, tab: id });
  return {
    close: () => {
      if (closed) return;
      closed = true;
      channel.postMessage({ gone: true, tab: id });
      channel.close();
    },
    say: (message: object) => channel.postMessage({ ...message, tab: id }),
    told,
  };
}
