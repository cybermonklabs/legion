// An Oh My Pi extension that measures, through the real host, what pi-legion reads of the host's
// own modules, and writes what it found to the file `CAPABILITY_PROBE_OUT` names, so
// `extensions/capability-host-omp.test.ts` can read it. Those modules resolve only inside the
// binary's bundle, and `capability-report.test.ts` and `turn-outcome.test.ts` fake them, so only
// an extension the real binary loads shows what they answer.
//
// It measures the capability report through the production host
// (`ompCapabilityHost`): the repository-extensions row is the one measured on the host's own
// discovery, since its two members, `discoverExtensionPaths` and `loadSkills`, are the production
// ones, and every other member answers at once with nothing, so no other row reaches a network, a
// child command or the profile's MCP servers. And the host's cyber-policy classifier
// (`isCyberPolicyRefusal` of `@oh-my-pi/pi-ai/error`), which `extensions/legion.ts` hands the
// turn-outcome reporter: pi-legion's one runtime import of a pi-ai subpath, which a reporter that
// cannot read it absorbs as a report with no `code` (`src/turn-outcome.ts`), so no other case
// would see it fail.
// A separate command measures the rejection from a real send in a capped session.
import { rename, writeFile } from "node:fs/promises";
import { costCapDrop } from "@legion/pi-shared/cost-cap";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import {
  type CapabilityHost,
  measureCapabilities,
  ompCapabilityHost,
} from "../src/capability-report";

/** The variable that names the file the probe writes. */
export const CAPABILITY_PROBE_OUT = "LEGION_TEST_CAPABILITY_PROBE_OUT";

/** The output file for the dropped-send test. */
export const COST_CAP_PROBE_OUT = "LEGION_TEST_COST_CAP_PROBE_OUT";

/**
 * The messages the probe asks the host's cyber-policy classifier about, each the assistant message
 * that ends a refused turn, which `src/turn-outcome.ts` hands it as the host hands it the same
 * message to set a `prompt_result`'s `code` (`modes/rpc/rpc-prompt-results.ts:232` at
 * v18.18.6-sami.20261126-222037). `refusal` is OpenAI's refusal as a gateway relays it, a 503 with
 * the code in its words, the shape the host's own `test/rpc-prompt-result.test.ts:205-212` gives
 * it at that tag; `other` is a refusal of the same status that is not one, so only the words can
 * tell them apart. Synthetic refusal text only.
 */
export const CYBER_POLICY_PROBE_MESSAGES = {
  refusal: {
    role: "assistant",
    stopReason: "error",
    errorStatus: 503,
    errorMessage: "503 cyber_policy: This content was flagged for possible cybersecurity risk.",
  },
  other: {
    role: "assistant",
    stopReason: "error",
    errorStatus: 503,
    errorMessage: "503 Service Unavailable: the model is overloaded",
  },
} as const;

/** A measurement that threw, with the error's stack. */
export interface ProbeFailure {
  readonly error: string;
}

/** What the probe writes. Each measurement fails alone, so one that throws hides no other. */
export interface CapabilityProbeReport {
  readonly capabilities:
    | {
        /** `discoverExtensionPaths(cwd)` as the production host answers it. */
        readonly extensionPaths: readonly unknown[];
        /** The repository-extensions row of a report measured through that host. */
        readonly row: unknown;
      }
    | ProbeFailure;
  /** The host classifier's answer for each of `CYBER_POLICY_PROBE_MESSAGES`. */
  readonly cyberPolicyRefusal:
    | { readonly [Name in keyof typeof CYBER_POLICY_PROBE_MESSAGES]: boolean }
    | ProbeFailure;
}

/** The production host, with every member but the repository-extensions row's two answering
 *  nothing. */
function probeHost(): CapabilityHost {
  const real = ompCapabilityHost();
  return {
    discoverAgents: async () => ({ agents: [] }),
    disabledAgents: async () => [],
    runSearchQuery: async () => ({}),
    loadMCPConfigs: async () => ({ configs: {}, sources: {} }),
    discoverMCPServers: async () => {
      throw new Error("the probe configures no MCP server");
    },
    discoverExtensionPaths: real.discoverExtensionPaths,
    loadSkills: real.loadSkills,
    run: async () => ({ code: 0, stdout: "", stderr: "" }),
  };
}

async function measured<T>(measure: () => Promise<T>): Promise<T | ProbeFailure> {
  try {
    return await measure();
  } catch (error) {
    return { error: error instanceof Error ? (error.stack ?? error.message) : String(error) };
  }
}

async function probe(context: SessionContext, out: string): Promise<void> {
  const report: CapabilityProbeReport = {
    capabilities: await measured(async () => {
      const host = probeHost();
      const extensionPaths = await host.discoverExtensionPaths(context.cwd);
      const capabilities = await measureCapabilities(host, {
        cwd: context.cwd,
        sessionId: context.sessionManager.getSessionId(),
        issue: "PROBE-1",
        promptAgents: [],
        activeTools: [],
        agentsExposed: false,
      });
      return {
        extensionPaths,
        row: capabilities.rows.find((row) => row.name === "repository-extensions"),
      };
    }),
    cyberPolicyRefusal: await measured(async () => {
      // Read as `extensions/legion.ts` reads it, a lazy string-literal `import()`, and never a
      // static one: the module exists only inside the host's bundle, whose loader resolves a
      // specifier of it only as a string literal, and `capability-host-omp.test.ts` loads this
      // file under `bun test`, where a static import of it fails the whole file.
      const { isCyberPolicyRefusal } = await import("@oh-my-pi/pi-ai/error");
      return {
        refusal: isCyberPolicyRefusal(CYBER_POLICY_PROBE_MESSAGES.refusal),
        other: isCyberPolicyRefusal(CYBER_POLICY_PROBE_MESSAGES.other),
      };
    }),
  };
  await writeReport(out, report);
}

async function writeReport(out: string, report: unknown): Promise<void> {
  // Whole or not at all: the test reads the file as soon as it exists.
  await writeFile(`${out}.partial`, JSON.stringify(report));
  await rename(`${out}.partial`, out);
}

export default function capabilityHostProbe(pi: PiApi): void {
  const dropOut = process.env[COST_CAP_PROBE_OUT];
  if (dropOut !== undefined) {
    pi.registerCommand("probe-cost-cap", {
      description: "Test the host's reply to a send at the session cost cap",
      handler: async () => {
        try {
          await pi.sendUserMessage("This send must not start a turn.");
          await writeReport(dropOut, { dropped: false });
        } catch (error) {
          await writeReport(dropOut, { dropped: true, message: costCapDrop(error) ?? null });
        }
      },
    });
  }
  const out = process.env[CAPABILITY_PROBE_OUT];
  if (out === undefined) return;
  pi.on("session_start", async (_event, context) => {
    // Not awaited: the host holds its startup for a session_start handler, and the probe needs none.
    void probe(context, out);
  });
}
