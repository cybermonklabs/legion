import { afterEach, expect, test } from "bun:test";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  type Cleanup,
  ompRoot,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { useRowGuard } from "@legion/test-fixtures/deadline";
import { until } from "@legion/test-fixtures/wait";
import { CAPABILITY_PROBE_OUT } from "../test/capability-host-probe";

// What pi-legion reads of Oh My Pi's own modules, on the real Oh My Pi. Those modules resolve
// only inside the binary's bundle, so the unit tests fake them and the typecheck reads only
// `omp-host.d.ts`: neither sees what the host's functions really answer. Each case runs the reader
// inside the binary (test/capability-host-probe.ts) and reads what it found.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which CI's
// pi-legion job installs (.github/actions/install-omp). The probe needs no NATS server, so a run
// without the binary skips, except on GitHub Actions, where a skip would hide the only run of the
// check on the host that ships it.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

// Each case starts a cold Oh My Pi and waits for its probe's report, so it needs more than Bun's
// 5000 ms default: `bun run test`'s workers declare 300000 ms (`test-timeout-preload.ts`), but a
// run of this file alone takes no worker and keeps 5000 ms. In a pod where Go tests ran, or with
// three CPU loops on its 2 CPUs, every case failed there with "until gave up waiting for the
// probe's report, 1000 ms before the row's guard of 5000 ms" (measured on 2026-11-28 at
// cybermonklabs/legion#5929's c54a88df; dispatch://INFRA-3904/comment/3b6fde09-c1cb-4c5a-8b24-980ee3c984c7).
// Each case therefore takes the package's guard as its own, as the cases of
// `legion-stall-watchdog-omp.test.ts` do: the case calls `useRowGuard` for the shared wait's
// deadline and passes the same value as `test`'s third argument for Bun's row kill, and the
// afterEach, whose cleanup ends a real Oh My Pi, passes it as its own hook timeout.
const COLD_START_GUARD_MS = 300_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
}, COLD_START_GUARD_MS);

/** Starts the binary with the probe loaded, after `prepare` has written into the session's
 *  workspace, and answers the report the probe wrote. */
async function probeReport(prepare: (workspace: string) => Promise<void>): Promise<unknown> {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  const { root, home, workspace, sessions } = await ompRoot(omp, "capability-host-omp-", cleanup);
  await prepare(workspace);
  // No turn runs; the profile still needs a model, and none may reach a real one.
  const { base } = serveStandin(cleanup, () => new Response("not served", { status: 404 }));
  await writeStandinProfile(home, base);

  const out = path.join(root, "capability-probe.json");
  const rpc = spawnRpc(
    omp,
    {
      extensions: [path.join(import.meta.dir, "..", "test", "capability-host-probe.ts")],
      home,
      workspace,
      sessions,
      env: { [CAPABILITY_PROBE_OUT]: out },
      onFrame: () => undefined,
    },
    cleanup
  );
  const written = await Promise.race([
    until("the probe's report", () => readFile(out, "utf8").catch(() => undefined)),
    rpc.closed,
  ]);
  await rpc.end();
  return JSON.parse(written);
}

// The capability report's production host (`ompCapabilityHost` in src/capability-report.ts). Oh
// My Pi's `discoverExtensionPaths` returned bare paths at v18.18.6-sami.20261123-024918 and
// returns `{path, scope}` entries at v18.18.6-sami.20261127-055727
// (`extensibility/extensions/loader.ts:804-809`), and an adapter that passed the entries on
// failed the repository-extensions row of every repository that carries an extension or a skill.
// This case runs against a workspace that carries one of each. Measured on 2026-11-25 against
// v18.18.6-sami.20261124-152326, which returns entries too: the adapter that passed them on fails
// this case, the row reading `The "paths[0]" property must be of type string, got object`, and the
// one that takes each entry's path passes it.
test.skipIf(omp === undefined && !onActions)(
  "the production host hands the repository-extensions row each extension's path, and the row finds the repository's own extension and skill",
  async () => {
    useRowGuard(COLD_START_GUARD_MS);
    let extension = "";
    const report = await probeReport(async (workspace) => {
      extension = path.join(workspace, ".omp", "extensions", "repository-probe.ts");
      await mkdir(path.dirname(extension), { recursive: true });
      await writeFile(extension, "export default function repositoryProbe(): void {}\n");
      const skill = path.join(workspace, ".omp", "skills", "repository-probe", "SKILL.md");
      await mkdir(path.dirname(skill), { recursive: true });
      await writeFile(
        skill,
        "---\nname: repository-probe\ndescription: A skill the repository carries.\n---\n\nNothing to do.\n"
      );
    });

    expect(report).toMatchObject({
      capabilities: {
        // Each a path, as the checks read it, never the host's `{path, scope}` entry.
        extensionPaths: expect.arrayContaining([extension]),
        rows: {
          "repository-extensions": {
            name: "repository-extensions",
            ok: true,
            detail:
              "discovered: .omp/extensions/repository-probe.ts, .omp/skills/repository-probe/SKILL.md",
          },
        },
      },
    });
  },
  COLD_START_GUARD_MS
);

// The mcp row through the production host's `loadMCPConfigs` and `discoverMCPServers`, on a
// workspace `.mcp.json` of one lazy server. Oh My Pi's manager connects no `lazy: true` server at
// discovery: it validates the configuration, records the validation's message as the server's
// error when it fails, and otherwise leaves the server `disconnected` with no error until its
// first tool call (`connectServers` and `getConnectionStatus` in `mcp/manager.ts` at
// v18.18.6-sami.20261127-055727). Each case starts the binary once, as the cases above do.
test.skipIf(omp === undefined && !onActions)(
  "the production host's mcp row names a lazy server lazy, and the row does not fail, though its command does not exist",
  async () => {
    useRowGuard(COLD_START_GUARD_MS);
    const report = await probeReport((workspace) =>
      writeFile(
        path.join(workspace, ".mcp.json"),
        JSON.stringify({
          mcpServers: { dormant: { command: "legion-test-no-such-mcp-server", lazy: true } },
        })
      )
    );

    expect(report).toMatchObject({
      capabilities: {
        rows: { mcp: { name: "mcp", ok: true, detail: "lazy: dormant (.mcp.json)" } },
      },
    });
  },
  COLD_START_GUARD_MS
);

// The configuration the manager refuses is a `url` under a transport it does not know: discovery
// passes it (`capability/mcp.ts` asks only for a `command` or a `url`) and hands the manager a
// stdio server with an empty command (`convertToLegacyConfig` in `mcp/config.ts`), which
// `validateServerConfig` refuses. A server with neither a `command` nor a `url` never reaches the
// manager: discovery drops it, and the row reads "no MCP server configured" (measured on
// 2026-11-27 at that tag).
test.skipIf(omp === undefined && !onActions)(
  "the production host's mcp row fails for a lazy server whose configuration the host refuses, naming the host's validation error",
  async () => {
    useRowGuard(COLD_START_GUARD_MS);
    const report = await probeReport((workspace) =>
      writeFile(
        path.join(workspace, ".mcp.json"),
        JSON.stringify({
          mcpServers: {
            refused: { type: "websocket", url: "http://127.0.0.1:9/mcp", lazy: true },
          },
        })
      )
    );

    expect(report).toMatchObject({
      capabilities: {
        rows: {
          mcp: {
            name: "mcp",
            ok: false,
            detail:
              'not connected: refused (Server "refused": stdio server requires "command" field)',
          },
        },
      },
    });
  },
  COLD_START_GUARD_MS
);

// The host's cyber-policy classifier, which extensions/legion.ts reads from
// `@oh-my-pi/pi-ai/error` for the turn-outcome reporter: pi-legion's one runtime import of a
// pi-ai subpath. The reporter turns a failure to read it into a warning and a report with no
// `code` (src/turn-outcome.ts), and turn-outcome.test.ts injects a classifier of its own, so only
// this case sees the import resolve in the binary and the host class a refusal by its words.
test.skipIf(omp === undefined && !onActions)(
  "the host's own classifier, read as the turn-outcome reporter reads it, classes a cyber-policy refusal as one and another refusal of the same status as none",
  async () => {
    useRowGuard(COLD_START_GUARD_MS);
    const report = await probeReport(async () => undefined);

    expect(report).toMatchObject({ cyberPolicyRefusal: { refusal: true, other: false } });
  },
  COLD_START_GUARD_MS
);
