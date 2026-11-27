// What seed.ts, heads-world.ts and owner-walk-seed.ts share of a run's scratch Dispatch: the person
// who files a run's issues and answers its asks, Dispatch's approval answer, the session of the
// implementer some of their requests are made as, the requests they make as the agent bearer, and
// every handoff record of an issue. e2e/api.ts, which this module imports, reads its port
// (DISPATCH_E2E_PORT) when it loads.
import type { Handoff, HandoffSummary } from "@legion/contracts";
import { baseUrl, e2eAgentToken } from "../../../dispatch/e2e/api";

/** The person who files the run's issue, posts the source's input and answers every ask. */
export const PERSON = "alice";
/** Dispatch's approval answer (packages/envoy/internal/dispatch/docs/approval_review.go). */
export const APPROVE = "Approve";
/** The implementer's session, never the run's own: tester-proof's implement handoff and its pass,
 * and the heads world's repository settings and the implementer's results, are written as it. */
export const IMPLEMENTER = { kind: "session", id: "rig-implementer" } as const;

/** `GET route` as the agent bearer, its JSON answer; a refusal names the route and Dispatch's answer. */
export async function agentGet<T>(route: string): Promise<T> {
  const response = await fetch(new URL(route, baseUrl), {
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  if (!response.ok)
    throw new Error(`GET ${route} failed: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

/** `POST route` as the agent bearer, with `input` as its JSON body, and Dispatch's JSON answer; a
 * refusal names the route and Dispatch's answer. */
export async function agentPost(route: string, input: unknown): Promise<unknown> {
  const response = await fetch(new URL(route, baseUrl), {
    method: "POST",
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(input),
  });
  if (!response.ok)
    throw new Error(`POST ${route} failed: ${response.status} ${await response.text()}`);
  return response.json();
}

/** Every handoff record of `issue`, newest first as `GET /api/v1/issues/{key}/handoffs` lists
 * them, each read whole by id (`GET /api/v1/handoffs/{id}`) for its body. */
export async function handoffsOf(issue: string): Promise<Handoff[]> {
  const summaries = await agentGet<HandoffSummary[]>(
    `/api/v1/issues/${encodeURIComponent(issue)}/handoffs`
  );
  const records: Handoff[] = [];
  for (const summary of summaries)
    records.push(await agentGet<Handoff>(`/api/v1/handoffs/${encodeURIComponent(summary.id)}`));
  return records;
}
