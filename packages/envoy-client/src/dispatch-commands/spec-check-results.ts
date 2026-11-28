// packages/envoy-client/src/dispatch-commands/spec-check-results.ts
//
// The project form of `dispatch result` and `dispatch results` (`--project`): a CI job's record of
// its result of a check of the project's spec files (`specCheckResultCommand`), and the read of
// the results CI jobs stored for those checks (`specCheckResultsCommand`). issues.ts runs both
// (`resultCommand`, `resultsCommand`), and job-result.ts runs a job's record.

import type { SpecCheckResult } from "@legion/contracts";
import type { DispatchClient } from "../dispatch-http";
import { printable, printableJson } from "../runner-log";
import { type DispatchToolResult, optionalBoolean, optionalString, stringArg } from "./context";
import { observedAsKept } from "./observed-text";

/**
 * `dispatch result --project`: a CI job's record of its result of one check of the spec files,
 * printed back as the stored result whole, as JSON: the in-toto statement of the run, which a
 * reader keeps, whose recorder is the run and no person. It sends what a result keeps of observed
 * (`observedAsKept`). The job runs it with its own identity and no session (`runDispatchCli`), and
 * a session's call reaches it through `resultCommand`.
 */
export async function specCheckResultCommand(
  client: DispatchClient,
  args: Record<string, unknown>
): Promise<DispatchToolResult> {
  const project = stringArg(args, "project");
  const observed = optionalString(args, "observed");
  const stored = await client.recordSpecCheckResult(project, {
    commit: stringArg(args, "commit"),
    check: stringArg(args, "check"),
    verdict: stringArg(args, "verdict"),
    ...(observed === undefined ? {} : { observed: observedAsKept(observed) }),
  });
  const { predicate } = stored.attestation;
  return {
    text: JSON.stringify(stored, null, 2),
    details: {
      project,
      result: stored.id,
      seq: stored.seq,
      check: predicate.check,
      verdict: predicate.verdict,
    },
  };
}

/** One stored result of a check of the spec files, as `dispatch results --project` lists it:
 *  Dispatch's record number, the check, the verdict, the job, the leg, the run, its attempt and
 *  the commit. Each of them is a CI job's or the stored statement's text, so it prints by the rule
 *  of a runner's log (`printable`, runner-log.ts): a listing read in a job's log starts no line of
 *  its own and names no workflow command. */
function specCheckResultLine(result: SpecCheckResult): string {
  const { predicate, subject } = result.attestation;
  const leg = predicate.leg === "" ? "no leg" : `leg ${printable(predicate.leg)}`;
  // biome-ignore lint/complexity/useLiteralKeys: digest is an index signature, and claude-envoy's tsconfig refuses dot access on one.
  const commit = subject?.[0]?.digest?.["gitCommit"] ?? "an unnamed commit";
  return `- #${result.seq} ${printable(predicate.check)}: ${printable(predicate.verdict)}. Job ${printable(predicate.job)}, ${leg}, run ${printable(predicate.run.run_id)} attempt ${predicate.run.attempt}, commit ${printable(commit)}.`;
}

/** `dispatch results --project`: every result CI jobs stored for checks of the project's spec
 *  files at one commit, one line each, newest first as Dispatch serves them, or the stored
 *  results as JSON with `json`, each by the rule of a runner's log (runner-log.ts). */
export async function specCheckResultsCommand(
  client: DispatchClient,
  args: Record<string, unknown>
): Promise<DispatchToolResult> {
  const project = stringArg(args, "project");
  const commit = stringArg(args, "commit");
  const check = optionalString(args, "check");
  const results = (await client.specCheckResults(project, commit, check)).results ?? [];
  const what = check === undefined ? project : `check ${printable(check)} of ${project}`;
  const text =
    optionalBoolean(args, "json") === true
      ? printableJson(JSON.stringify(results, null, 2))
      : results.length === 0
        ? `No result of ${what} is stored at ${printable(commit)}.`
        : [
            `Results of ${what} at ${printable(commit)}, newest first:`,
            ...results.map(specCheckResultLine),
          ].join("\n");
  return { text, details: { project, commit, results: results.length } };
}
