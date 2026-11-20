import { fieldFlag } from "../dispatch-command";
import { defaultExec } from "../dispatch-cwd";
import type { DispatchClient } from "../dispatch-http";
import { resolveOwnerArguments } from "./check";
import {
  type CheckedCall,
  type DispatchCallCheckInput,
  type DispatchToolResult,
  optionalString,
  stringArg,
} from "./context";
import { observedAsKept } from "./observed-text";

// ci-job-result.ts is `dispatch result --issue` as a CI job runs it: with the job's own identity
// (AGENT_SECRETS_MACHINE_IDENTITY=github-actions), the job records its result of the check of an
// issue's Verification document whose ci_check names it. The route takes check, commit, verdict
// and observed from a results token, and reads who recorded the result from the token and from
// GitHub, so the command takes those flags and the issue alone.

const TOOL = "dispatch_result";

/** The arguments a CI job's result of a check its ci_check names takes. */
const CI_JOB_RESULT_FIELDS: Readonly<Record<string, true>> = {
  issue: true,
  check: true,
  verdict: true,
  commit: true,
  observed: true,
};

/** The three of them the record cannot do without. */
const CI_JOB_RESULT_REQUIRED = ["check", "verdict", "commit"] as const;

/** What the job's form takes, written once: each refusal of a call that breaks it gives it. */
const CI_JOB_RESULT_RULE =
  "With the CI job's own identity (AGENT_SECRETS_MACHINE_IDENTITY=github-actions), dispatch " +
  "result --issue records the job's result of the check that its ci_check names, and takes " +
  "--issue, --check, --verdict, --commit and --observed alone.";

/** Every rule a CI job's result call breaks, each naming its flags, or none. */
function ciJobResultProblems(args: Readonly<Record<string, unknown>>): string[] {
  const problems: string[] = [];
  const carried = Object.keys(args)
    .filter((field) => args[field] !== undefined && CI_JOB_RESULT_FIELDS[field] !== true)
    .map((field) => fieldFlag(TOOL, field));
  if (carried.length > 0) {
    problems.push(`${CI_JOB_RESULT_RULE} This call also carries ${carried.join(", ")}.`);
  }
  const missing = CI_JOB_RESULT_REQUIRED.filter((field) => args[field] === undefined).map((field) =>
    fieldFlag(TOOL, field)
  );
  if (missing.length > 0) {
    problems.push(`a CI job's result needs ${missing.join(", ")}. ${CI_JOB_RESULT_RULE}`);
  }
  const verdict = args.verdict;
  if (verdict !== undefined && verdict !== "pass" && verdict !== "fail") {
    problems.push(
      "--verdict is pass or fail for a CI job's result of a check that its ci_check names."
    );
  }
  return problems;
}

/** The check `dispatch result --issue` makes under the job's own identity before it sends
 *  anything, the one `--dry-run` runs too: the issue (from --issue, a ref or LEGION_ISSUE), and the
 *  job's own rules (`ciJobResultProblems`) in place of a role's, since the job sends no role. */
export async function checkCIJobResultCall(input: DispatchCallCheckInput): Promise<CheckedCall> {
  const problems: string[] = [];
  const owner = await resolveOwnerArguments(
    input.tool,
    input.args,
    input.cwd,
    input.env ?? process.env,
    input.exec ?? defaultExec,
    input.serverUrls,
    problems
  );
  problems.push(...ciJobResultProblems(owner.args));
  return { problems, owner, args: owner.args, pictures: [] };
}

/** `dispatch result --issue` under the job's own identity: the job's result of one check whose
 *  ci_check names it, printed back as the route's answer whole, as JSON. It sends what a result
 *  keeps of observed (`observedAsKept`), as the project form does. */
export async function ciJobResultCommand(
  client: DispatchClient,
  args: Record<string, unknown>
): Promise<DispatchToolResult> {
  const issue = stringArg(args, "issue");
  const observed = optionalString(args, "observed");
  const recorded = await client.recordCIJobResult(issue, {
    check: stringArg(args, "check"),
    commit: stringArg(args, "commit"),
    verdict: stringArg(args, "verdict"),
    ...(observed === undefined ? {} : { observed: observedAsKept(observed) }),
  });
  return {
    text: JSON.stringify(recorded, null, 2),
    details: {
      issue,
      result: recorded.result.id,
      check: recorded.result.check_block_id,
      verdict: recorded.result.verdict,
      line: recorded.line,
    },
  };
}
