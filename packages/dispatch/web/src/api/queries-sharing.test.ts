import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { http } from "msw";

import { answerRequests } from "../__tests__/network";
import type { ClosedWindow } from "../lib/closed-window";
import { api } from "./client";
import {
  advancesIssuesQuery,
  legionBoardQuery,
  projectDoneIssuesQuery,
  projectDonePageQuery,
  projectIssuesQuery,
  projectOpenCountQuery,
  UnsortedPageError,
} from "./queries";
import type { IssueSummary } from "./types";

function issue(key: string, updated_at: string): IssueSummary {
  return {
    advances: [],
    advances_revision: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    key,
    labels: [],
    last_seq: 1,
    open_asks: 0,
    progress: { tasks: null, children: null },
    parent: null,
    blocked_by: [],
    primary_artifact_id: "artifact-1",
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    priority: null,
    rank: key,
    status: "done",
    title: `Work ${key}`,
    updated_at,
    closed_at: updated_at,
    created_by: null,
    created_at: "2026-09-10T00:00:00Z",
  };
}

// A listing's first read, and the next one: CORE-3 changed and moved to the top, and CORE-1 and
// CORE-2 each moved down one place, unchanged. Each call builds new objects, as a read does.
const firstRead = () => [
  issue("CORE-1", "2026-10-03T00:00:00Z"),
  issue("CORE-2", "2026-10-02T00:00:00Z"),
  issue("CORE-3", "2026-10-01T00:00:00Z"),
];
const movedRead = () => [
  issue("CORE-3", "2026-10-04T00:00:00Z"),
  issue("CORE-1", "2026-10-03T00:00:00Z"),
  issue("CORE-2", "2026-10-02T00:00:00Z"),
];

const client = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

/** Each `read` reads the query again and answers its data as the cache holds it. */
function reads<T>(observer: {
  refetch: () => Promise<{ readonly data: T | undefined }>;
  subscribe: (listener: () => void) => () => void;
}) {
  const stop = observer.subscribe(() => {});
  return { read: async () => (await observer.refetch()).data, stop };
}

/** Three reads of a listing: the first, the moved one, and the moved one again. */
async function expectSharedByKey(read: () => Promise<readonly IssueSummary[] | undefined>) {
  const before = (await read()) ?? [];
  const after = (await read()) ?? [];
  expect(after.map((row) => row.key)).toEqual(["CORE-3", "CORE-1", "CORE-2"]);
  // Paired by key, the two issues that only moved keep the objects the page holds.
  expect(after[1]).toBe(before[0]);
  expect(after[2]).toBe(before[1]);
  // The issue that changed is the one new object.
  expect(after[0]).not.toBe(before[2]);
  expect(after[0]?.updated_at).toBe("2026-10-04T00:00:00Z");
  // A read that changed nothing keeps the cached list itself.
  expect(await read()).toBe(after);
}

test("the project listing keeps the object of every issue a read did not change", async () => {
  const listIssues = spyOn(api, "listIssues")
    .mockResolvedValueOnce(firstRead())
    .mockResolvedValueOnce(movedRead())
    .mockResolvedValueOnce(movedRead());
  const { read, stop } = reads(new QueryObserver(client(), projectIssuesQuery("CORE", true)));
  try {
    await expectSharedByKey(read);
  } finally {
    stop();
    listIssues.mockRestore();
  }
});

test("the read of every done issue keeps the object of every issue a read did not change", async () => {
  const listIssues = spyOn(api, "listIssues")
    .mockResolvedValueOnce(firstRead())
    .mockResolvedValueOnce(movedRead())
    .mockResolvedValueOnce(movedRead());
  const { read, stop } = reads(
    new QueryObserver(client(), projectDoneIssuesQuery("CORE", undefined))
  );
  try {
    await expectSharedByKey(read);
  } finally {
    stop();
    listIssues.mockRestore();
  }
});

test("an advances listing keeps the object of every issue a read did not change", async () => {
  const listIssues = spyOn(api, "listIssues")
    .mockResolvedValueOnce(firstRead())
    .mockResolvedValueOnce(movedRead())
    .mockResolvedValueOnce(movedRead());
  const { read, stop } = reads(
    new QueryObserver(client(), advancesIssuesQuery({ advances: "ship-it" }))
  );
  try {
    await expectSharedByKey(read);
  } finally {
    stop();
    listIssues.mockRestore();
  }
});

test("a page of Done keeps its unchanged issues by key, and the cached page when nothing changed", async () => {
  // The route names the sort it applied, as it answers Done's read.
  const page = (issues: IssueSummary[], total: number) => ({
    issues,
    limit: 3,
    offset: 0,
    total,
    sort: "-closed_at" as const,
  });
  const listIssuePage = spyOn(api, "listIssuePage")
    .mockResolvedValueOnce(page(firstRead(), 3))
    .mockResolvedValueOnce(page(movedRead(), 4))
    .mockResolvedValueOnce(page(movedRead(), 4));
  const { read, stop } = reads(
    new QueryObserver(client(), projectDonePageQuery("CORE", { offset: 0, size: 3 }))
  );
  try {
    const before = await read();
    const after = await read();
    expect(after?.total).toBe(4);
    expect(after?.issues?.map((row) => row.key)).toEqual(["CORE-3", "CORE-1", "CORE-2"]);
    expect(after?.issues?.[1]).toBe(before?.issues?.[0]);
    expect(after?.issues?.[2]).toBe(before?.issues?.[1]);
    expect(after?.issues?.[0]).not.toBe(before?.issues?.[2]);
    expect(await read()).toBe(after);
  } finally {
    stop();
    listIssuePage.mockRestore();
  }
});

const sevenDays: ClosedWindow = {
  from: Date.parse("2026-11-10T13:47:12Z"),
  to: null,
  rolling: true,
};

test("a page of Done reads its window's own first moment, newest close first", async () => {
  const listIssuePage = spyOn(api, "listIssuePage").mockResolvedValue({
    issues: [],
    limit: 50,
    offset: 50,
    total: 0,
    sort: "-closed_at",
  });
  try {
    await client().fetchQuery(
      projectDonePageQuery("CORE", { offset: 50, size: 50, window: sevenDays })
    );
    expect(listIssuePage.mock.calls.map(([query]) => query)).toEqual([
      {
        project: "CORE",
        status: ["done"],
        sort: "-closed_at",
        limit: 50,
        offset: 50,
        closed_since: "2026-11-10T13:47:12.000Z",
      },
    ]);
  } finally {
    listIssuePage.mockRestore();
  }
});

// A server older than `sort` ignores it and answers the route's own order, with no `sort` in its
// page. Done's read, through the client, fails on that page rather than hold it, and on a page in
// the other order, and holds the page whose answer names `-closed_at`.
test("a page of Done whose answer does not name sort=-closed_at fails the read", async () => {
  const answer = {
    issues: [issue("CORE-1", "2026-10-03T00:00:00Z")],
    limit: 50,
    offset: 0,
    total: 1,
  };
  const answers = [answer, { ...answer, sort: "closed_at" }, { ...answer, sort: "-closed_at" }];
  answerRequests(http.get("/api/v1/issues", () => Response.json(answers.shift())));
  const read = () => client().fetchQuery(projectDonePageQuery("CORE", { offset: 0, size: 50 }));
  await expect(read()).rejects.toBeInstanceOf(UnsortedPageError);
  await expect(read()).rejects.toBeInstanceOf(UnsortedPageError);
  expect(await read()).toEqual({ ...answer, sort: "-closed_at" });
});

test("a read of every done issue starts a rolling window on its hour, and a range at its own ends", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const later: ClosedWindow = { ...sevenDays, from: Date.parse("2026-11-10T13:59:59Z") };
  // Later in the same hour the rolling window starts on the same hour, so the read is the same.
  expect(projectDoneIssuesQuery("CORE", later).queryKey).toEqual(
    projectDoneIssuesQuery("CORE", sevenDays).queryKey
  );
  const range: ClosedWindow = {
    from: Date.parse("2026-11-03T10:30:00Z"),
    to: Date.parse("2026-11-05T00:00:00Z"),
    rolling: false,
  };
  try {
    await client().fetchQuery(projectDoneIssuesQuery("CORE", sevenDays));
    await client().fetchQuery(projectDoneIssuesQuery("CORE", range));
    expect(listIssues.mock.calls.map(([query]) => query)).toEqual([
      { project: "CORE", status: ["done"], closed_since: "2026-11-10T13:00:00.000Z" },
      {
        project: "CORE",
        status: ["done"],
        closed_since: "2026-11-03T10:30:00.000Z",
        closed_before: "2026-11-05T00:00:00.000Z",
      },
    ]);
  } finally {
    listIssues.mockRestore();
  }
});

// k9: a window whose end is at or before its first moment holds no close, and the list route
// refuses it (INVALID_CLOSED_WINDOW). Done's reads of it answer no row and ask the route nothing.
test("a window that holds no moment is read as no done issue, with no request", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listIssuePage = spyOn(api, "listIssuePage").mockResolvedValue({
    issues: [],
    limit: 50,
    offset: 0,
    total: 0,
  });
  const backwards: ClosedWindow = {
    from: Date.parse("2026-11-10T00:00:00Z"),
    to: Date.parse("2026-11-03T00:00:00Z"),
    rolling: false,
  };
  try {
    expect(await client().fetchQuery(projectDoneIssuesQuery("CORE", backwards))).toEqual([]);
    expect(
      await client().fetchQuery(
        projectDonePageQuery("CORE", { offset: 0, size: 50, window: backwards })
      )
    ).toEqual({ issues: [], limit: 50, offset: 0, total: 0 });
    expect(listIssues).not.toHaveBeenCalled();
    expect(listIssuePage).not.toHaveBeenCalled();
    // Its reads are its own, apart from those of every done issue.
    expect(projectDoneIssuesQuery("CORE", backwards).queryKey).not.toEqual(
      projectDoneIssuesQuery("CORE", undefined).queryKey
    );
    expect(
      projectDonePageQuery("CORE", { offset: 0, size: 50, window: backwards }).queryKey
    ).not.toEqual(projectDonePageQuery("CORE", { offset: 0, size: 50 }).queryKey);
  } finally {
    listIssues.mockRestore();
    listIssuePage.mockRestore();
  }
});

test("Open's count is the total of a one-row page of the open listing", async () => {
  const asked: Record<string, string>[] = [];
  answerRequests(
    http.get("/api/v1/issues", ({ request }) => {
      asked.push(Object.fromEntries(new URL(request.url).searchParams));
      return Response.json({
        issues: [issue("CORE-1", "2026-10-03T00:00:00Z")],
        limit: 1,
        offset: 0,
        total: 47,
      });
    })
  );
  expect(await client().fetchQuery(projectOpenCountQuery("CORE"))).toBe(47);
  expect(asked).toEqual([{ project: "CORE", open: "true", limit: "1" }]);
});

test("the board is one live subscription per project, on TanStack Query's own sharing", () => {
  const board = legionBoardQuery("ACME");
  expect([...board.queryKey]).toEqual(["legion", "board", { kind: "legion", project: "ACME" }]);
  expect(board.meta).toEqual({ live: true });
  expect(board.gcTime).toBe(0);
  expect(board.staleTime).toBe(Number.POSITIVE_INFINITY);
  expect(board.structuralSharing).toBeUndefined();
  expect(board.placeholderData).toBeUndefined();
});
