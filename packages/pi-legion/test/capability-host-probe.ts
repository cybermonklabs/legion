// An Oh My Pi extension that measures the capability report through the production host
// (`ompCapabilityHost`) and writes what it found to the file `CAPABILITY_PROBE_OUT` names, so
// `extensions/capability-host-omp.test.ts` can read it. The production host's Oh My Pi imports
// resolve only inside the binary's bundle, and `capability-report.test.ts` fakes the whole host,
// so only an extension the real binary loads shows what the adapter hands the checks.
//
// The repository-extensions row is the one measured on the host's own discovery: its two members,
// `discoverExtensionPaths` and `loadSkills`, are the production ones, and every other member
// answers at once with nothing, so no other row reaches a network, a child command or the
// profile's MCP servers.
import { rename, writeFile } from "node:fs/promises";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import {
  type CapabilityHost,
  measureCapabilities,
  ompCapabilityHost,
} from "../src/capability-report";

/** The variable that names the file the probe writes. */
export const CAPABILITY_PROBE_OUT = "LEGION_TEST_CAPABILITY_PROBE_OUT";

/** What the probe writes: the adapter's own answer for the workspace, and the row it measured. */
export interface CapabilityProbeReport {
  /** `discoverExtensionPaths(cwd)` as the production host answers it. */
  readonly extensionPaths: readonly unknown[];
  /** The repository-extensions row of a report measured through that host. */
  readonly row: unknown;
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

async function probe(context: SessionContext, out: string): Promise<void> {
  const host = probeHost();
  let report: CapabilityProbeReport | { readonly error: string };
  try {
    const extensionPaths = await host.discoverExtensionPaths(context.cwd);
    const measured = await measureCapabilities(host, {
      cwd: context.cwd,
      sessionId: context.sessionManager.getSessionId(),
      issue: "PROBE-1",
      promptAgents: [],
      activeTools: [],
      agentsExposed: false,
    });
    report = {
      extensionPaths,
      row: measured.rows.find((row) => row.name === "repository-extensions"),
    };
  } catch (error) {
    report = { error: error instanceof Error ? (error.stack ?? error.message) : String(error) };
  }
  // Whole or not at all: the test reads the file as soon as it exists.
  await writeFile(`${out}.partial`, JSON.stringify(report));
  await rename(`${out}.partial`, out);
}

export default function capabilityHostProbe(pi: PiApi): void {
  const out = process.env[CAPABILITY_PROBE_OUT];
  if (out === undefined) return;
  pi.on("session_start", async (_event, context) => {
    // Not awaited: the host holds its startup for a session_start handler, and the probe needs none.
    void probe(context, out);
  });
}
