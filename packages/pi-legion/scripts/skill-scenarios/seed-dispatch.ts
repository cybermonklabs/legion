// What seed.ts and owner-walk-seed.ts share of a run's scratch Dispatch: the person who files a
// run's issues and answers its asks, Dispatch's approval answer, and every handoff record of an
// issue. e2e/api.ts, which this module imports, reads its port (DISPATCH_E2E_PORT) when it loads.
import type { Handoff, HandoffSummary } from "@legion/contracts";
import { baseUrl, e2eAgentToken } from "../../../dispatch/e2e/api";

/** The person who files the run's issue, posts the source's input and answers every ask. */
export const PERSON = "alice";
/** Dispatch's approval answer (packages/envoy/internal/dispatch/docs/approval_review.go). */
export const APPROVE = "Approve";

/** `GET route` as the agent bearer, its JSON answer; a refusal names the route and Dispatch's answer. */
async function agentGet<T>(route: string): Promise<T> {
  const response = await fetch(new URL(route, baseUrl), {
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  if (!response.ok)
    throw new Error(`GET ${route} failed: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
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
