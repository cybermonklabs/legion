/**
 * The page's read of every project's open asks against a Dispatch process of the release before
 * (`openAsksOfTheReleaseBefore`). Such a process takes `GET /api/v1/asks/open` only with `project`
 * or `author_session`, and refuses the read with neither 400 AUTHOR_SESSION_OR_PROJECT_REQUIRED.
 * The margin, the Inbox's asks of other people and the Agents and Machines pages' counts all draw
 * from that read, so while such a process serves it is answered from the read that release's page
 * made, and none of those surfaces fails during a roll or after a rollback.
 *
 * This file answers that release's read as its server answers it, so the acceptance driver's search
 * for callers of the reads this release replaced spares it by its path
 * (`e2e/acceptance/one-inbox-callers.sh`, `callers_spared`), and the root's pull request deletes it
 * with the fallback (`root-deletes.txt`, `open-asks-scopeless-fallback`).
 */

import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { http } from "msw";

import { answerRequests } from "../__tests__/network";
import { issueAsk } from "../features/inbox/inbox-fixture";
import { ApiError } from "./client";
import { openAsksQuery } from "./queries";

/** A request's path and query, as a route answering it is named. */
function pathOf(request: Request): string {
  const url = new URL(request.url);
  return `${url.pathname}${url.search}`;
}

/** A fresh client for one read of the open asks, which asks nothing again on a failure. */
function openAsksClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

test("the open asks a process of the release before refuses to list with no narrowing are read as that release's page read them", async () => {
  const ask = issueAsk({ id: "ask-a" });
  const asked: string[] = [];
  answerRequests(
    http.get("/api/v1/asks/open", ({ request }) => {
      asked.push(pathOf(request));
      return Response.json(
        {
          code: "AUTHOR_SESSION_OR_PROJECT_REQUIRED",
          error: "exactly one of author_session or project is required",
        },
        { status: 400 }
      );
    }),
    http.get("/api/v1/inbox", ({ request }) => {
      asked.push(pathOf(request));
      return Response.json([ask]);
    })
  );

  expect(await openAsksClient().fetchQuery(openAsksQuery())).toEqual([ask]);
  expect(asked).toEqual(["/api/v1/asks/open", "/api/v1/inbox"]);
});

test("any other refusal of the open asks is the read's own failure, and nothing else is read", async () => {
  const asked: string[] = [];
  answerRequests(
    http.get("/api/v1/asks/open", ({ request }) => {
      asked.push(pathOf(request));
      return Response.json(
        { code: "INVALID_QUERY", error: "waiting_on must name a person" },
        { status: 400 }
      );
    }),
    http.get("/api/v1/inbox", ({ request }) => {
      asked.push(pathOf(request));
      return Response.json([]);
    })
  );

  const refusal = await openAsksClient()
    .fetchQuery(openAsksQuery())
    .catch((thrown: unknown) => thrown);
  expect(refusal).toBeInstanceOf(ApiError);
  expect(refusal).toMatchObject({ code: "INVALID_QUERY", status: 400 });
  expect(asked).toEqual(["/api/v1/asks/open"]);
});
