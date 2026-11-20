// The requests seed.ts and heads-world.ts both make of a run's scratch Dispatch as the agent bearer,
// and the session of the implementer they make some as. e2e/api.ts reads its port when it loads, as
// seed.ts's header says.
import { baseUrl, e2eAgentToken } from "../../../dispatch/e2e/api";

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
