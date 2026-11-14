import { expect, test } from "bun:test";
import { MAX_REFERENCE_TARGET_BYTES } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { http } from "msw";

import { answerRequests } from "../../__tests__/network";
import { knownSessionQuery } from "./known-sessions";

/** Answers `GET /api/v1/agents/known` as Dispatch would for a registry listing `s-listed` alone,
 *  and records each request's query. */
function answerKnownSessions(): URLSearchParams[] {
  const asked: URLSearchParams[] = [];
  answerRequests(
    http.get("/api/v1/agents/known", ({ request }) => {
      const query = new URL(request.url).searchParams;
      asked.push(query);
      return Response.json({
        sessions: query
          .getAll("id")
          .filter((id) => id === "s-listed")
          .map((id) => ({ session_id: id, title: "Planner" })),
      });
    })
  );
  return asked;
}

test("a page's session reads are one request naming each session in an id parameter", async () => {
  const asked = answerKnownSessions();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const [listed, never] = await Promise.all([
    client.fetchQuery(knownSessionQuery("s-listed")),
    client.fetchQuery(knownSessionQuery("s-never")),
  ]);

  expect(asked.map((query) => query.getAll("id"))).toEqual([["s-listed", "s-never"]]);
  expect(asked[0]?.has("session")).toBe(false);
  expect(listed).toEqual({ known: true, title: "Planner" });
  expect(never).toEqual({ known: false, title: undefined });
});

test("an id longer than a reference names is answered unknown and never sent, and its batch still reads", async () => {
  const asked = answerKnownSessions();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const longest = "s".repeat(MAX_REFERENCE_TARGET_BYTES);
  const past = `${longest}s`;

  const [listed, atBound, pastBound] = await Promise.all([
    client.fetchQuery(knownSessionQuery("s-listed")),
    client.fetchQuery(knownSessionQuery(longest)),
    client.fetchQuery(knownSessionQuery(past)),
  ]);

  expect(asked.map((query) => query.getAll("id"))).toEqual([["s-listed", longest]]);
  expect(listed).toEqual({ known: true, title: "Planner" });
  expect(atBound).toEqual({ known: false, title: undefined });
  expect(pastBound).toEqual({ known: false, title: undefined });
});
