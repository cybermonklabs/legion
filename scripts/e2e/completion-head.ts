// The driver of handoff-records.sh's `completion head` step: one call of the real
// `findHandoffReport` (packages/pi-shared/src/handoff-record.ts), the function each harness's
// `legion` tool asks what a `handoff_complete` reports beside its summary, made against the run's
// own Dispatch server as the pane's plugin makes it — the session's envoy.json by the pane's own
// rule (`sessionConfigFile("omp", …)`, the line pi-legion's extension holds) and
// `paneLatestHandoff` on it, so the record is read from Dispatch over HTTP with this session's
// own credential. Nothing of the report is computed here.
//
//   LEGION_ISSUE=<KEY> DISPATCH_HOST=omp DISPATCH_SESSION_ID=<session> \
//     DISPATCH_URL=<dispatch-url> DISPATCH_TOKEN=<token> \
//     bun scripts/e2e/completion-head.ts <phase> <role>
//
// Its one line of standard output is the completion that call would send, as JSON: `commit`,
// `handoffId` and `handoffVersion`, each written only where the report carries it. So a phase
// whose newest record names a `head` prints that commit, and one whose record names none prints
// no `commit` at all, which is the step's negative control.
//
// A missing argument or variable exits 2 naming it, before Dispatch is asked anything; a report
// the function refuses (no record of that phase, a Dispatch that does not answer, an
// implementer's head its branch does not carry) exits 1 with its message on standard error.

import type { LegionPhase } from "../../packages/contracts/src/legion-api";
import type { LegionRole } from "../../packages/contracts/src/legion-roles";
import { sessionConfigFile } from "../../packages/envoy-client/src/session-settings";
import { findHandoffReport, paneLatestHandoff } from "../../packages/pi-shared/src/handoff-record";

function refuse(why: string): never {
  console.error(`completion-head: ${why}`);
  process.exit(2);
}

const [phase, role] = process.argv.slice(2);
if (phase === undefined || role === undefined) {
  refuse("usage: bun scripts/e2e/completion-head.ts <phase> <role>");
}
const session = process.env.DISPATCH_SESSION_ID;
if (session === undefined || session === "") {
  refuse("DISPATCH_SESSION_ID names the session this report is made as, and it is not set");
}
if (process.env.LEGION_ISSUE === undefined || process.env.LEGION_ISSUE === "") {
  refuse("LEGION_ISSUE names the issue whose record is read, and it is not set");
}

// The pane's own two lines (`paneHandoffReport`): the envoy.json this session reads, and the
// Dispatch behind it. The run writes no such file, so `DISPATCH_URL` and `DISPATCH_TOKEN` are
// the whole of this pane's Dispatch settings, as they are for its `dispatch` command.
const file = sessionConfigFile("omp", process.env);
const latestHandoff = paneLatestHandoff(file, process.env, process.cwd(), session);

try {
  const report = await findHandoffReport({
    phase: phase as LegionPhase,
    role: role as LegionRole,
    env: process.env,
    latestHandoff,
  });
  console.log(JSON.stringify(report));
} catch (error) {
  console.error(`completion-head: ${error instanceof Error ? error.message : String(error)}`);
  process.exit(1);
}
