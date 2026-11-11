// The handoff seeds of a worker run's own scratch Dispatch, and the read-back of what the run
// left there: the records each phase before the one under test wrote, and the passing results
// that go with them. seed.ts owns the command line and every other mode; these modes are here
// because they are one world — one fixture file, one issue, one check — and seed.ts had grown
// past a thousand lines around them.
//
//   seed.ts implement-handoff <file> <body file> <commit>   implementHandoff
//   seed.ts tester-result <file> <body file> <commit>       testerResult
//   seed.ts review-handoff <file> <body file>               reviewHandoff
//   seed.ts capture-handoffs <file> <run dir>               captureHandoffs
//
// seed.ts's own header says what each writes and reads.
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Actor, Handoff, HandoffSummary } from "@legion/contracts";
import { z } from "zod";
import { baseUrl, createHandoff, e2eAgentToken } from "../../../dispatch/e2e/api";

/** What `tester-proof` writes, and `implement-handoff`, `tester-result`, `review-handoff` and
 * `capture-handoffs` read: the run's issue, and each phase record a seed has written on it. */
export const TesterFixture = z.object({
  issue: z.string(),
  implement: z.object({ id: z.string(), version: z.number().int() }).optional(),
  test: z.object({ id: z.string(), version: z.number().int() }).optional(),
  review: z.object({ id: z.string(), version: z.number().int() }).optional(),
});
/** The sessions the worker's predecessors wrote their handoffs as: the implementer's, and the
 * tester's and the reviewer's where a scenario seeds one, never the run's own. */
export const IMPLEMENTER = { kind: "session", id: "rig-implementer" } as const;
export const TESTER = { kind: "session", id: "rig-tester" } as const;
export const REVIEWER = { kind: "session", id: "rig-reviewer" } as const;

/** Each phase a seed writes a handoff for: the predecessor's session, which records it, and the
 * role whose passing result on check `k1` goes with it, for the two phases whose role records
 * one — a review round records none. The phase is the key, so a caller names it once and the
 * three cannot disagree: the session, the role and the phase were three arguments, and a
 * handoff written under the tester's session as the implementer's phase is a world no run of
 * the rig has. */
const PHASE_SEED = {
  implement: { actor: IMPLEMENTER as Actor, role: "implementer" },
  test: { actor: TESTER as Actor, role: "tester" },
  review: { actor: REVIEWER as Actor, role: undefined },
} as const;
type HandoffPhase = keyof typeof PHASE_SEED;

/** One predecessor's handoff on the run's issue, as `dispatch handoff-write` records it under
 * that predecessor's own session, and, for a phase whose role records one, its passing result
 * against the issue's check `k1` at `commit`, as `dispatch result` records it; the record goes
 * into the fixture under its phase. A body that is no JSON object is the fixture's fault,
 * refused before anything is written. */
async function phaseHandoff(
  file: string,
  bodyFile: string,
  phase: HandoffPhase,
  commit = ""
): Promise<void> {
  const { actor, role } = PHASE_SEED[phase];
  const fixture = TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const body: unknown = JSON.parse(readFileSync(bodyFile, "utf8"));
  if (typeof body !== "object" || body === null || Array.isArray(body))
    throw new Error(`${bodyFile} holds no JSON object, which a handoff body is`);
  const recorded = await createHandoff(fixture.issue, phase, body, actor);
  if (role !== undefined)
    await agentPost(`/api/v1/issues/${encodeURIComponent(fixture.issue)}/results`, {
      check: "k1",
      role,
      verdict: "pass",
      commit,
      surface: "cli",
      run: "bun greet.ts Ada",
      observed: "prints Hello, Ada! and exits 0",
      negative_control: "bun greet.ts → exit 2, usage: greet.ts <name> on stderr",
      actor,
    });
  const record = { id: recorded.id, version: recorded.version };
  writeFileSync(file, `${JSON.stringify({ ...fixture, [phase]: record })}\n`);
  console.log(
    `seeded ${fixture.issue}'s ${phase} handoff, version ${recorded.version} (${recorded.id})`
  );
}

/** `implement-handoff <file> <body file> <commit>`: the implementer's handoff and its passing
 * result, under the implementer's session. */
export async function implementHandoff(
  file: string,
  bodyFile: string,
  commit: string
): Promise<void> {
  await phaseHandoff(file, bodyFile, "implement", commit);
}

/** `tester-result <file> <body file> <commit>`: the tester's handoff and its passing result,
 * under the tester's session. A reviewer refuses an approval while a `before_merge` check of the
 * issue has no passing result current at the head it reads, so the reviewer scenario seeds this
 * one beside the implementer's and the reviewer's verdict rests on what it found in the change;
 * threads-closed seeds both so that neither of its agents reads a gate of its own as open. */
export async function testerResult(file: string, bodyFile: string, commit: string): Promise<void> {
  await phaseHandoff(file, bodyFile, "test", commit);
}

/** `review-handoff <file> <body file>`: the round's review handoff, under the reviewer's session
 * and with no result of its own, since a reviewer records none against a check. The task of each
 * agent of threads-closed names its version, and the record is the approval they work from. */
export async function reviewHandoff(file: string, bodyFile: string): Promise<void> {
  await phaseHandoff(file, bodyFile, "review");
}

/** `GET route` as the agent bearer, its JSON answer; a refusal names the route and Dispatch's answer. */
async function agentGet<T>(route: string): Promise<T> {
  const response = await fetch(new URL(route, baseUrl), {
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  if (!response.ok)
    throw new Error(`GET ${route} failed: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

/** `POST route` as the agent bearer, with `input` as its JSON body; a refusal names the route and
 * Dispatch's answer. */
async function agentPost(route: string, input: unknown): Promise<void> {
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
}

/** `capture-handoffs <file> <run dir>`: every handoff record of the run's issue, the seeded
 * implement record among them, newest first as `GET …/handoffs` lists them, each read whole by id
 * (`GET /api/v1/handoffs/{id}`) for its body. */
export async function captureHandoffs(file: string, runDir: string): Promise<void> {
  const { issue } = TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const summaries = await agentGet<HandoffSummary[]>(
    `/api/v1/issues/${encodeURIComponent(issue)}/handoffs`
  );
  const records: Handoff[] = [];
  for (const summary of summaries)
    records.push(await agentGet<Handoff>(`/api/v1/handoffs/${encodeURIComponent(summary.id)}`));
  writeFileSync(path.join(runDir, "handoffs.json"), `${JSON.stringify(records)}\n`);
  console.log(`captured ${issue}: ${records.length} handoff records`);
}
