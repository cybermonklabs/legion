import type { LegionRole } from "@legion/contracts"
import type { LegionPhase } from "@legion/contracts/legion-api"
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import type { HandoffReport } from "@legion/pi-shared/handoff-record"
import {
  type ClaimCapability,
  executePhaseOp,
  legionToolDescription,
  PHASE_OP_SENTENCES,
  PHASE_OPS,
  phaseOpParameters,
  pluginBuildOf,
} from "@legion/pi-shared/legion-phase-ops"
import { z } from "zod"
import pkg from "../package.json" with { type: "json" }

/** This plugin's own build, which `read_build` answers with: the manifest of the module this
 * process loaded, never a path on the image. A relaunch installs the plugin pair fresh under
 * the role's state home, so the build at the image's own path is another one. */
const BUILD = pluginBuildOf(pkg)

/** The tool's name: Claude Code shows it as `mcp__legion__legion`, under the pane's one server. */
export const LEGION_TOOL_NAME = "legion"

/** The tool as `tools/list` declares it: one object schema, `op` choosing one of the phase ops
 * (`@legion/pi-shared/legion-phase-ops`), the operations a phase worker gets. */
export function legionToolDefinition() {
  return {
    name: LEGION_TOOL_NAME,
    description: legionToolDescription({
      envoyPublish: "mcp__legion__envoy_publish",
      before: PHASE_OP_SENTENCES.readRecord + PHASE_OP_SENTENCES.readReleaseRecords,
      after: PHASE_OP_SENTENCES.requestBackwardMove + PHASE_OP_SENTENCES.readBuild,
    }),
    inputSchema: z.toJSONSchema(z.object({ op: z.enum(PHASE_OPS), ...phaseOpParameters(z) })),
  }
}

export interface LegionToolRuntime {
  readonly daemon: LegionDaemonClient
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => ClaimCapability | undefined
  /** What a completion of `phase` by `role` reports beside its summary (`findHandoffReport` with
   * the pane's environment and Dispatch): throws naming the remedy when the phase has no handoff
   * record, Dispatch does not answer, or the implementer's work is not pushed. */
  readonly handoffReport: (phase: LegionPhase, role: LegionRole) => Promise<HandoffReport>
  /** The head of `legion/<issue>@origin` a `ready_for_update` signal reports
   * (`findPushedBranchHead` with the pane's environment): `""` when the branch is not there. */
  readonly branchHead: () => Promise<string>
}

/** Runs one call of the `legion` tool and answers its JSON result; a refusal throws its reason.
 * `signal`, the MCP request's cancellation, ends a `handoff_complete` waiting out a daemon
 * restart. */
export async function executeLegionTool(
  runtime: LegionToolRuntime,
  input: unknown,
  signal?: AbortSignal,
): Promise<unknown> {
  const claim = runtime.claim()
  if (claim === undefined) {
    throw new Error(
      "this session has no Legion claim yet: the pane registers with the daemon once its first turn starts",
    )
  }
  return executePhaseOp(
    {
      daemon: runtime.daemon,
      claim,
      build: BUILD,
      handoffReport: runtime.handoffReport,
      branchHead: runtime.branchHead,
      ...(signal === undefined ? {} : { restart: { signal } }),
    },
    input,
  )
}
