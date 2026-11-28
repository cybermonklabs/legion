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
 * process loaded, never a path read anywhere else. In a pod a Claude Code role installs no plugin
 * (the daemon's `NoPluginInstall`) and loads its image's own claude-envoy, so a relaunch keeps
 * this build and only the update round over the pod brings another. It is an Oh My Pi role's
 * relaunch that installs its plugin pair fresh under the role's state home. Its contract is
 * `daemonContract`'s, read as the module loads. */
const BUILD = pluginBuildOf(pkg, import.meta.dir)

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
  /** What a completion by `role` reports beside its summary while the issue stands at `phase`
   * (`findHandoffReport` with the pane's environment and Dispatch): the newest record of the
   * phase the role works or, once the issue has moved on, of the phase it left, or `@-` alone where
   * that left phase has none or the implementer's left record names no head. Throws naming the remedy when the phase the role works has no
   * handoff record, Dispatch does not answer, or, while the issue stands in implementing, the
   * implementer's record names no head, the commit its workspace stands on is not pushed, or its
   * record's head is not that pushed commit. */
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

/** Runs one call of the `legion` tool, as executeLegionTool does, and tells the host once a
 * `ready_for_update` it ran answered the pod's update message: the worker then waits for its
 * process to be replaced, so the `Stop` of the turn it answered in blocks nothing
 * (`updateAnswered`, `./legion-phase-hook`). A completion and a look's report tell the host
 * nothing: the reminder asks the daemon what the claim owes. A refusal throws before any hook. */
export async function answerLegionTool(
  runtime: LegionToolRuntime,
  args: Record<string, unknown>,
  hooks: { readonly updateAnswered: () => void | Promise<void> },
  signal?: AbortSignal,
): Promise<unknown> {
  const answer = await executeLegionTool(runtime, args, signal)
  if (args["op"] === "ready_for_update") await hooks.updateAnswered()
  return answer
}
