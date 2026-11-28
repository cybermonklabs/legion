import { afterAll, afterEach, beforeAll, describe, expect, jest, test } from "bun:test";
import { existsSync, lstatSync, readFileSync, writeFileSync } from "node:fs";
import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  LegionCapabilityReport,
  type LegionCapabilityReportBody,
} from "@legion/contracts/legion-api";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { turn } from "@legion/test-fixtures/fake-clock";
import {
  CAPABILITY_CHECK_TIMEOUT_MS,
  CAPABILITY_REPORT_TIMEOUT_MS,
  type CapabilityHost,
  type CapabilityInput,
  type CommandResult,
  CREDENTIAL_RULES,
  cleanDetail,
  credentialRuleCharacters,
  ENVOY_TOOL_NAMES,
  HELD_READINGS,
  LIVE_CAPABILITIES,
  type LiveCapability,
  MAX_DETAIL_BYTES,
  measureCapabilities,
  type RunOptions,
  WEB_SEARCH_PROBE_QUERY,
} from "./capability-report";

interface RecordedRun {
  readonly command: string;
  readonly args: readonly string[];
  readonly cwd: string;
  readonly signal: AbortSignal;
  readonly env: Readonly<Record<string, string>> | undefined;
}

interface RecordedSearch {
  readonly args: { readonly query: string; readonly limit: number };
  readonly options: { readonly sessionId?: string; readonly signal: AbortSignal };
}

/** A host where every check passes: three task agents, a provider answering the search, no MCP
 * server, nothing discovered, and both commands exiting 0. `overrides` replace members. */
function fakeHost(overrides: Partial<CapabilityHost> = {}): CapabilityHost & {
  readonly runs: RecordedRun[];
  readonly searches: RecordedSearch[];
} {
  const runs: RecordedRun[] = [];
  const searches: RecordedSearch[] = [];
  return {
    runs,
    searches,
    discoverAgents: async () => ({
      agents: [{ name: "oracle" }, { name: "deep-worker" }, { name: "scout" }],
    }),
    disabledAgents: async () => [],
    runSearchQuery: async (args, options) => {
      searches.push({ args, options });
      return {
        content: [{ type: "text", text: "[1] Jujutsu—a version control system" }],
        details: { response: { provider: "parallel", sources: [] } },
      };
    },
    loadMCPConfigs: async () => ({ configs: {}, sources: {} }),
    discoverMCPServers: async () => {
      throw new Error("discoverMCPServers must not run when no server is configured");
    },
    discoverExtensionPaths: async () => [],
    loadSkills: async () => ({ skills: [] }),
    run: async (command, args, options) => {
      runs.push({ command, args, cwd: options.cwd, signal: options.signal, env: options.env });
      return command === "gh"
        ? { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" }
        : { code: 0, stdout: "LEGION-663 · Capability report\n", stderr: "" };
    },
    ...overrides,
  };
}

let workspace: string;
const originalPath = process.env.PATH;

beforeAll(async () => {
  workspace = await mkdtemp(path.join(os.tmpdir(), "legion-capability-"));
});
afterAll(async () => {
  await rm(workspace, { recursive: true, force: true });
});
afterEach(() => {
  process.env.PATH = originalPath;
  // A row that holds the clock hands it back here too, so a failure inside it leaves no later
  // row on a clock that does not move.
  jest.useRealTimers();
});

/** The moment a row that holds the clock (Bun's fake timers) starts it at. */
const CLOCK_START = 1_793_000_000_000;

function input(overrides: Partial<CapabilityInput> = {}): CapabilityInput {
  return {
    cwd: workspace,
    sessionId: "ses_capability",
    issue: "LEGION-663",
    promptAgents: ["oracle", "deep-worker"],
    activeTools: ["read", "task", "web_search", ...ENVOY_TOOL_NAMES],
    agentsExposed: true,
    ...overrides,
  };
}

/** The named row of a report. */
async function measure(
  host: CapabilityHost,
  overrides: Partial<CapabilityInput> = {}
): Promise<Record<LiveCapability, { readonly ok: boolean; readonly detail: string }>> {
  const report = await measureCapabilities(host, input(overrides));
  const rows = {} as Record<LiveCapability, { readonly ok: boolean; readonly detail: string }>;
  for (const row of report.rows)
    rows[row.name as LiveCapability] = { ok: row.ok, detail: row.detail };
  return rows;
}

/**
 * The names of the rows that passed, less `repository-extensions`: alone among the live checks it
 * awaits the filesystem — a `readdir`, then a `stat` for each entry — so its outcome lands on a
 * macrotask a loaded event loop can defer past a short budget, the budget working as designed, not
 * a defect. `checkDispatchEnvoyTools` reads the disk too, but synchronously, through `whichOnPath`
 * (`Bun.which`); every other check settles in microtasks alone under `fakeHost`, so no timer can
 * defer one and its row reads the same on any machine.
 */
function passingNamesExceptTheDiskCheck(report: LegionCapabilityReportBody): string[] {
  return report.rows
    .filter((row) => row.ok && row.name !== "repository-extensions")
    .map((row) => row.name);
}

/** `fakeHost` with its gh call answered by `gh`, which is handed the call's options and runs as
 * the call starts, so a step file it writes is there before the check's budget starts counting. */
function ghHost(
  gh: (options: RunOptions) => CommandResult | Promise<CommandResult>
): CapabilityHost {
  return fakeHost({
    run: async (command, _args, options) =>
      command === "gh"
        ? gh(options)
        : { code: 0, stdout: "LEGION-663 · Capability report\n", stderr: "" },
  });
}

/** The step file the github row's call names for `legion gh` (LEGION_GH_STEP_FILE). */
function stepFile(options: RunOptions): string {
  const file = options.env?.LEGION_GH_STEP_FILE;
  if (file === undefined) throw new Error("the gh call names no LEGION_GH_STEP_FILE");
  return file;
}

/** A command that runs until its signal aborts and then ends as a killed child does, with no exit
 * code: what `Bun.spawn` answers once a check's budget aborts the call. */
function untilAborted(signal: AbortSignal): Promise<CommandResult> {
  const ended = Promise.withResolvers<CommandResult>();
  signal.addEventListener("abort", () => ended.resolve({ code: null, stdout: "", stderr: "" }), {
    once: true,
  });
  return ended.promise;
}

describe("the report", () => {
  test("carries the six live rows in the daemon's table order, when it started and how long it took, in the wire's shape", async () => {
    // The report reads the time from this process's clock, so the row holds that clock (Bun's
    // fake timers) and moves it while the two commands are out: the moments are the ones it set.
    const measuredMs = 1_234;
    const answered = Promise.withResolvers<void>();
    const answers = fakeHost();
    const host = fakeHost({
      run: async (command, args, options) => {
        await answered.promise;
        return answers.run(command, args, options);
      },
    });
    jest.useFakeTimers({ now: CLOCK_START });
    const measuring = measureCapabilities(host, input());
    jest.advanceTimersByTime(measuredMs);
    answered.resolve();
    const report = await measuring;
    jest.useRealTimers();

    expect(report.rows.map((row) => row.name)).toEqual([...LIVE_CAPABILITIES]);
    expect(report.rows.every((row) => row.ok)).toBe(true);
    expect(report.measuredAt).toBe(new Date(CLOCK_START).toISOString());
    expect(report.elapsedMs).toBe(measuredMs);
    expect(LegionCapabilityReport.parse(report)).toEqual(report);
  });

  test("the budgets are 8 s a check and 10 s a report", () => {
    expect(CAPABILITY_CHECK_TIMEOUT_MS).toBe(8_000);
    expect(CAPABILITY_REPORT_TIMEOUT_MS).toBe(10_000);
  });

  test("ENVOY_TOOL_NAMES is every tool the Envoy plugin registers, in its order", () => {
    expect([...ENVOY_TOOL_NAMES]).toEqual(envoyToolSpecs.map((spec) => spec.name));
  });

  test("a check that throws is a failing row carrying the first line of its message; the others stand", async () => {
    const rows = await measure(
      fakeHost({
        discoverAgents: async () => {
          throw new Error("agents directory unreadable: EACCES\n    at readdir (node:fs)");
        },
      })
    );
    expect(rows.subagents).toEqual({ ok: false, detail: "agents directory unreadable: EACCES" });
    expect(rows["web-search"].ok).toBe(true);
    expect(rows.github.ok).toBe(true);
  });

  test("a host failing everywhere still yields six rows and never throws", async () => {
    const refuse = async (): Promise<never> => {
      throw new TypeError("host.discoverAgents is not a function");
    };
    const report = await measureCapabilities(
      {
        discoverAgents: refuse,
        disabledAgents: refuse,
        runSearchQuery: refuse,
        loadMCPConfigs: refuse,
        discoverMCPServers: refuse,
        discoverExtensionPaths: refuse,
        loadSkills: refuse,
        run: refuse,
      },
      input()
    );
    expect(report.rows).toHaveLength(6);
    expect(report.rows.map((row) => row.ok)).toEqual([false, false, false, true, false, false]);
    expect(report.rows[0]?.detail).toBe("host.discoverAgents is not a function");
  });

  test("a check still pending at its budget is a failing row and its signal is aborted", async () => {
    let seen: AbortSignal | undefined;
    const host = fakeHost({
      runSearchQuery: (_args, options) => {
        seen = options.signal;
        return Promise.withResolvers<never>().promise;
      },
    });
    const report = await measureCapabilities(host, input(), { checkMs: 20, reportMs: 500 });
    const row = report.rows.find((candidate) => candidate.name === "web-search");
    expect(row).toEqual({ name: "web-search", ok: false, detail: "did not finish within 20 ms" });
    expect(seen?.aborted).toBe(true);
    expect(passingNamesExceptTheDiskCheck(report)).toEqual([
      "subagents",
      "mcp",
      "dispatch-envoy-tools",
      "github",
    ]);
  });

  test("a row still pending at the report's budget is a failing row naming that budget, and the report does not wait for it", async () => {
    const pending = Promise.withResolvers<CommandResult>();
    const host = fakeHost({ run: () => pending.promise });
    // Nothing but a budget can end the two command rows, and the row holds the clock both budgets
    // run on (Bun's fake timers): it moves it past the report's 20 ms and never to the checks'
    // 30 s, and answers the commands only after that. The order of the two is the verdict: a
    // report that waited for its checks could come back only once the commands had answered.
    const order: string[] = [];
    jest.useFakeTimers({ now: CLOCK_START });
    const measuring = measureCapabilities(host, input(), { checkMs: 30_000, reportMs: 20 }).then(
      (report) => {
        order.push("the report came back");
        return report;
      }
    );
    // Every check that answers at once has answered before the report's budget passes.
    await turn();
    jest.advanceTimersByTime(20);
    await turn();
    order.push("the commands answered");
    // Resolving lets the abandoned checks settle, so their 30 s timers clear.
    pending.resolve({ code: 0, stdout: "", stderr: "" });
    const report = await measuring;
    jest.useRealTimers();

    expect(order).toEqual(["the report came back", "the commands answered"]);
    // And by the report's own reading of that clock, it ended at its budget.
    expect(report.elapsedMs).toBe(20);
    for (const name of ["github", "dispatch-envoy-tools"])
      expect(report.rows.find((row) => row.name === name)).toEqual({
        name,
        ok: false,
        detail: "did not finish within 20 ms",
      });
    expect(passingNamesExceptTheDiskCheck(report)).toEqual(["subagents", "web-search", "mcp"]);
  });

  test("a whole-second budget reads in seconds", async () => {
    const host = fakeHost({ run: () => Promise.withResolvers<never>().promise });
    const report = await measureCapabilities(host, input(), { checkMs: 1_000, reportMs: 1_500 });
    expect(report.rows.find((row) => row.name === "dispatch-envoy-tools")).toEqual({
      name: "dispatch-envoy-tools",
      ok: false,
      detail: "did not finish within 1 s",
    });
    // This gh wrote no step file, as no `legion gh` stood behind it.
    expect(report.rows.find((row) => row.name === "github")).toEqual({
      name: "github",
      ok: false,
      detail: "did not finish within 1 s; legion gh wrote no step",
    });
  });

  // INFRA-927: the session measures again while a row is missing (src/capability-schedule.ts), so
  // a second measurement must carry nothing of the first: the row that failed on its budget is
  // asked again, and so is every row that passed.
  test("a second measurement carries nothing of the first: every row is measured again, and the one that failed on its budget passes", async () => {
    let searches = 0;
    const host = fakeHost({
      // The first measurement's search never answers, so its row fails on the check budget; the
      // second's answers.
      runSearchQuery: () => {
        searches += 1;
        return searches === 1
          ? Promise.withResolvers<never>().promise
          : Promise.resolve({
              content: [{ type: "text", text: "[1] Jujutsu—a version control system" }],
              details: { response: { provider: "parallel" } },
            });
      },
    });
    const budgets = { checkMs: 20, reportMs: 500 };

    // The row holds the clock both measurements read (Bun's fake timers) and moves it once: past
    // the first search's budget, after every check that answers at once has answered.
    jest.useFakeTimers({ now: CLOCK_START });
    const measuringFirst = measureCapabilities(host, input(), budgets);
    await turn();
    jest.advanceTimersByTime(budgets.checkMs);
    const first = await measuringFirst;
    const second = await measureCapabilities(host, input(), budgets);
    jest.useRealTimers();

    expect(first.rows.find((row) => row.name === "web-search")).toEqual({
      name: "web-search",
      ok: false,
      detail: "did not finish within 20 ms",
    });
    expect(second.rows.map((row) => row.name)).toEqual([...LIVE_CAPABILITIES]);
    expect(passingNamesExceptTheDiskCheck(second)).toEqual([
      "subagents",
      "web-search",
      "mcp",
      "dispatch-envoy-tools",
      "github",
    ]);
    // The second report is its own measurement, not the first with one row patched: it started
    // when the first ended, at the moment the first's search budget passed, and every check asked
    // the host a second time.
    expect(first.measuredAt).toBe(new Date(CLOCK_START).toISOString());
    expect(first.elapsedMs).toBe(budgets.checkMs);
    expect(second.measuredAt).toBe(new Date(CLOCK_START + budgets.checkMs).toISOString());
    expect(searches).toBe(2);
    // `dispatch` and `gh`, once each per measurement, in the order the checks start them.
    expect(host.runs.map((run) => run.command)).toEqual(["dispatch", "gh", "dispatch", "gh"]);
  });
});

describe("subagents", () => {
  test("passes when task resolves every agent the prompts dispatch", async () => {
    const rows = await measure(fakeHost());
    expect(rows.subagents).toEqual({
      ok: true,
      detail: "task resolves 3 agents, every one the prompts dispatch",
    });
  });

  test("fails when the host exposes no pi.agents", async () => {
    const rows = await measure(fakeHost(), { agentsExposed: false });
    expect(rows.subagents).toEqual({ ok: false, detail: "pi.agents is not exposed" });
  });

  test("fails when task is not an active tool", async () => {
    const rows = await measure(fakeHost(), { activeTools: ["read", "web_search"] });
    expect(rows.subagents).toEqual({ ok: false, detail: "task is not a registered tool" });
  });

  test("names the prompts' agents task does not resolve", async () => {
    const rows = await measure(fakeHost(), { promptAgents: ["oracle", "ghost", "phantom"] });
    expect(rows.subagents).toEqual({
      ok: false,
      detail: "agents the prompts dispatch that the task tool does not resolve: ghost, phantom",
    });
  });

  test("names the prompts' agents task.disabledAgents turns off", async () => {
    const rows = await measure(fakeHost({ disabledAgents: async () => ["oracle", "scout"] }));
    expect(rows.subagents).toEqual({
      ok: false,
      detail: "disabled by task.disabledAgents: oracle",
    });
  });

  test("reports both problems when both hold", async () => {
    const rows = await measure(fakeHost({ disabledAgents: async () => ["deep-worker"] }), {
      promptAgents: ["oracle", "deep-worker", "ghost"],
    });
    expect(rows.subagents).toEqual({
      ok: false,
      detail:
        "agents the prompts dispatch that the task tool does not resolve: ghost; disabled by task.disabledAgents: deep-worker",
    });
  });
});

describe("web-search", () => {
  test("runs the fixed probe query once, with the session id and the check's signal", async () => {
    const host = fakeHost();
    const rows = await measure(host);
    expect(rows["web-search"].ok).toBe(true);
    expect(rows["web-search"].detail).toMatch(
      /^web_search registered; provider parallel answered in \d+ ms$/
    );
    expect(host.searches).toHaveLength(1);
    expect(host.searches[0]?.args).toEqual({ query: WEB_SEARCH_PROBE_QUERY, limit: 1 });
    expect(WEB_SEARCH_PROBE_QUERY).toBe("jujutsu version control");
    expect(host.searches[0]?.options.sessionId).toBe("ses_capability");
    expect(host.searches[0]?.options.signal).toBeInstanceOf(AbortSignal);
  });

  test("fails without a search when web_search is not an active tool", async () => {
    const host = fakeHost();
    const rows = await measure(host, { activeTools: ["read", "task", ...ENVOY_TOOL_NAMES] });
    expect(rows["web-search"]).toEqual({
      ok: false,
      detail: "web_search is not a registered tool",
    });
    expect(host.searches).toEqual([]);
  });

  test("fails with the provider's error", async () => {
    const rows = await measure(
      fakeHost({
        runSearchQuery: async () => ({
          content: [],
          details: { error: "rate limited by exa", provider: "exa" },
        }),
      })
    );
    expect(rows["web-search"]).toEqual({
      ok: false,
      detail: "web_search failed: rate limited by exa",
    });
  });

  test("fails when no provider answered", async () => {
    for (const details of [{ provider: "none" }, { response: { provider: "none" } }, {}]) {
      const rows = await measure(
        fakeHost({
          runSearchQuery: async () => ({ content: [{ type: "text", text: "" }], details }),
        })
      );
      expect(rows["web-search"]).toEqual({
        ok: false,
        detail: "web_search failed: no provider answered",
      });
    }
  });

  test("fails with the first line of a text answer that is an error", async () => {
    const rows = await measure(
      fakeHost({
        runSearchQuery: async () => ({
          content: [{ type: "text", text: "Error: provider exa refused the key\nTry again later" }],
          details: { provider: "exa" },
        }),
      })
    );
    expect(rows["web-search"]).toEqual({
      ok: false,
      detail: "web_search failed: Error: provider exa refused the key",
    });
  });

  test("reads the provider from details.provider when the response names none", async () => {
    const rows = await measure(
      fakeHost({
        runSearchQuery: async () => ({
          content: [{ type: "text", text: "[1] Jujutsu" }],
          details: { provider: "brave" },
        }),
      })
    );
    expect(rows["web-search"].detail).toMatch(/^web_search registered; provider brave answered/);
  });
});

describe("mcp", () => {
  interface FakeManager {
    readonly statuses: Record<string, string>;
    readonly calls: string[];
    readonly waitError?: Error;
  }
  const manager = (fake: FakeManager) => ({
    waitForPendingConnections: async () => {
      fake.calls.push("wait");
      if (fake.waitError !== undefined) throw fake.waitError;
    },
    getConnectionStatus: (name: string) => fake.statuses[name] ?? "unknown",
    disconnectAll: async () => {
      fake.calls.push("disconnectAll");
    },
  });
  const configured = async () => ({
    configs: { alpha: { command: "alpha" }, beta: { url: "http://beta" } },
    sources: {
      alpha: { path: path.join(workspace, ".omp", "mcp.json") },
      beta: { path: path.join(workspace, ".mcp.json") },
    },
  });

  test("passes with no server configured, connecting nothing", async () => {
    const rows = await measure(fakeHost());
    expect(rows.mcp).toEqual({ ok: true, detail: "no MCP server configured" });
  });

  test("passes naming every connected server and the file it is configured in, then disconnects", async () => {
    const fake: FakeManager = { statuses: { alpha: "connected", beta: "connected" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({ manager: manager(fake), errors: [] }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: true,
      detail: "connected: alpha (.omp/mcp.json), beta (.mcp.json)",
    });
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("fails naming the server that did not connect with the error recorded for it", async () => {
    const fake: FakeManager = { statuses: { alpha: "connected", beta: "disconnected" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({
          manager: manager(fake),
          errors: [
            { path: "mcp:beta", error: "ENOENT: no such file or directory, posix_spawn 'beta'" },
          ],
        }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: false,
      detail:
        "not connected: beta (ENOENT: no such file or directory, posix_spawn 'beta'); connected: alpha (.omp/mcp.json)",
    });
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("fails with the connection status when no error names the server, and reads server/message entries", async () => {
    const fake: FakeManager = { statuses: { alpha: "failed" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({
          manager: manager(fake),
          errors: [{ server: "alpha", message: "handshake refused" }],
        }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: false,
      detail: "not connected: alpha (handshake refused), beta (status unknown)",
    });
  });

  test("disconnects even when waiting for the connections throws, and the row carries the error", async () => {
    const fake: FakeManager = {
      statuses: {},
      calls: [],
      waitError: new Error("connection pool closed\nat MCPManager.wait"),
    };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({ manager: manager(fake), errors: [] }),
      })
    );
    expect(rows.mcp).toEqual({ ok: false, detail: "connection pool closed" });
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("disconnects when the check's budget aborts a connection that never finishes", async () => {
    const fake: FakeManager = { statuses: {}, calls: [] };
    const never = new Promise<void>(() => undefined);
    const stuck = {
      ...manager(fake),
      waitForPendingConnections: () => {
        fake.calls.push("wait");
        return never;
      },
    };
    const report = await measureCapabilities(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({ manager: stuck, errors: [] }),
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    const mcp = report.rows.find((row) => row.name === "mcp");
    expect(mcp).toEqual({ name: "mcp", ok: false, detail: "did not finish within 20 ms" });
    // The abort tore the second set of clients down; the finally that never ran would have.
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("redacts the userinfo of a URL the recorded error quotes", async () => {
    const fake: FakeManager = { statuses: { alpha: "connected", beta: "failed" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({
          manager: manager(fake),
          errors: [
            {
              path: "mcp:beta",
              error: "fetch failed: https://user:secret@mcp.example/sse (HTTP 401)",
            },
          ],
        }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: false,
      detail:
        "not connected: beta (fetch failed: https://***@mcp.example/sse (HTTP 401)); connected: alpha (.omp/mcp.json)",
    });
  });

  test("disconnects when the budget aborted while the manager was still being made", async () => {
    const fake: FakeManager = { statuses: {}, calls: [] };
    const tornDown = Promise.withResolvers<void>();
    const stuck = {
      ...manager(fake),
      waitForPendingConnections: () => {
        fake.calls.push("wait");
        return new Promise<void>(() => undefined);
      },
      disconnectAll: async () => {
        fake.calls.push("disconnectAll");
        tornDown.resolve();
      },
    };
    // The manager exists only once the report has settled, which aborts every check's signal —
    // the command checks', read here — and the report settles once the mcp check's own 20 ms
    // budget has aborted it. A listener the check adds after that never hears the abort.
    const settled = Promise.withResolvers<void>();
    const report = await measureCapabilities(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => {
          await settled.promise;
          return { manager: stuck, errors: [] };
        },
        run: async (_command, _args, options) => {
          options.signal.addEventListener("abort", () => settled.resolve(), { once: true });
          return { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" };
        },
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    const mcp = report.rows.find((row) => row.name === "mcp");
    expect(mcp).toEqual({ name: "mcp", ok: false, detail: "did not finish within 20 ms" });
    // The check found its signal already aborted and tore the clients down before waiting on a
    // connection that never finishes; the finally that never runs would have.
    await tornDown.promise;
    expect(fake.calls).toEqual(["disconnectAll", "wait"]);
  });
});

describe("repository-extensions", () => {
  let repository: string;
  beforeAll(async () => {
    repository = path.join(workspace, "repository");
    await mkdir(path.join(repository, ".omp", "extensions", "dirext"), { recursive: true });
    await mkdir(path.join(repository, ".omp", "extensions", "notes"), { recursive: true });
    await mkdir(path.join(repository, ".omp", "skills", "alpha"), { recursive: true });
    await mkdir(path.join(repository, ".omp", "skills", "stray"), { recursive: true });
    await mkdir(path.join(repository, ".claude", "skills", "beta"), { recursive: true });
    await writeFile(path.join(repository, ".omp", "extensions", "flat.ts"), "export {};\n");
    await writeFile(path.join(repository, ".omp", "extensions", "README.md"), "# not a module\n");
    await writeFile(
      path.join(repository, ".omp", "extensions", "dirext", "index.ts"),
      "export {};\n"
    );
    await writeFile(
      path.join(repository, ".omp", "extensions", "notes", "notes.txt"),
      "no index\n"
    );
    await writeFile(path.join(repository, ".omp", "skills", "alpha", "SKILL.md"), "# alpha\n");
    await writeFile(path.join(repository, ".claude", "skills", "beta", "SKILL.md"), "# beta\n");
  });

  test("passes when the repository carries nothing to load, asking the host nothing", async () => {
    const empty = path.join(workspace, "empty");
    await mkdir(empty, { recursive: true });
    const rows = await measure(
      fakeHost({
        discoverExtensionPaths: async () => {
          throw new Error("must not be asked");
        },
      }),
      { cwd: empty }
    );
    expect(rows["repository-extensions"]).toEqual({
      ok: true,
      detail: "the repository carries no .omp/extensions, .omp/skills or .claude/skills",
    });
  });

  test("passes when every module and skill on disk is among what the host discovers", async () => {
    const rows = await measure(
      fakeHost({
        discoverExtensionPaths: async (cwd) => [
          path.join(cwd, ".omp", "extensions", "flat.ts"),
          path.join(cwd, ".omp", "extensions", "dirext", "index.ts"),
        ],
        loadSkills: async (cwd) => ({
          skills: [
            { filePath: path.join(cwd, ".omp", "skills", "alpha", "SKILL.md") },
            { filePath: path.join(cwd, ".claude", "skills", "beta", "SKILL.md") },
          ],
        }),
      }),
      { cwd: repository }
    );
    expect(rows["repository-extensions"]).toEqual({
      ok: true,
      detail:
        "discovered: .claude/skills/beta/SKILL.md, .omp/extensions/dirext, .omp/extensions/flat.ts, .omp/skills/alpha/SKILL.md",
    });
  });

  test("fails naming what is on disk and not discovered", async () => {
    const rows = await measure(
      fakeHost({
        discoverExtensionPaths: async (cwd) => [
          path.join(cwd, ".omp", "extensions", "dirext", "index.ts"),
        ],
        loadSkills: async (cwd) => ({
          skills: [{ filePath: path.join(cwd, ".omp", "skills", "alpha", "SKILL.md") }],
        }),
      }),
      { cwd: repository }
    );
    expect(rows["repository-extensions"]).toEqual({
      ok: false,
      detail:
        "not discovered: .claude/skills/beta/SKILL.md, .omp/extensions/flat.ts; discovered: .omp/extensions/dirext, .omp/skills/alpha/SKILL.md",
    });
  });
});

describe("dispatch-envoy-tools", () => {
  test("passes when the eleven tools are active and dispatch read answers, naming PATH's dispatch", async () => {
    const bin = path.join(workspace, "bin");
    await mkdir(bin, { recursive: true });
    const dispatch = path.join(bin, "dispatch");
    await writeFile(dispatch, "#!/bin/sh\nexit 0\n");
    await chmod(dispatch, 0o755);
    process.env.PATH = `${path.join(workspace, "nowhere")}${path.delimiter}${bin}`;
    const host = fakeHost();

    const rows = await measure(host);
    expect(rows["dispatch-envoy-tools"]).toEqual({
      ok: true,
      detail: `11 Envoy tools registered; dispatch read LEGION-663 answered from ${dispatch}`,
    });
    const run = host.runs.find((candidate) => candidate.command === "dispatch");
    expect(run?.args).toEqual(["read", "--issue", "LEGION-663"]);
    expect(run?.cwd).toBe(workspace);
    expect(run?.signal).toBeInstanceOf(AbortSignal);
  });

  test("names dispatch itself when PATH has none", async () => {
    process.env.PATH = path.join(workspace, "nowhere");
    const rows = await measure(fakeHost());
    expect(rows["dispatch-envoy-tools"].detail).toBe(
      "11 Envoy tools registered; dispatch read LEGION-663 answered from dispatch"
    );
  });

  test("fails without running dispatch when Envoy tools are missing", async () => {
    const host = fakeHost();
    const rows = await measure(host, {
      activeTools: ["task", "web_search", ...ENVOY_TOOL_NAMES.slice(0, 8)],
    });
    expect(rows["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "Envoy tools missing: envoy_whoami, envoy_sessions, slack_post",
    });
    expect(host.runs.map((run) => run.command)).toEqual(["gh"]);
  });

  test("fails with dispatch's first stderr line, else its exit code", async () => {
    const withStderr = await measure(
      fakeHost({
        run: async (command) =>
          command === "dispatch"
            ? { code: 1, stdout: "", stderr: "dispatch: issue LEGION-663 not found\nrun --help\n" }
            : { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" },
      })
    );
    expect(withStderr["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "dispatch read failed: dispatch: issue LEGION-663 not found",
    });
    const silent = await measure(
      fakeHost({
        run: async (command) =>
          command === "dispatch"
            ? { code: 2, stdout: "", stderr: "" }
            : { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" },
      })
    );
    expect(silent["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "dispatch read failed: exit code 2",
    });
  });
});

describe("github", () => {
  test("passes with the login the viewer query answers", async () => {
    const host = fakeHost();
    const rows = await measure(host);
    expect(rows.github).toEqual({ ok: true, detail: "gh viewer login: legion-implementer[bot]" });
    const run = host.runs.find((candidate) => candidate.command === "gh");
    expect(run?.args).toEqual([
      "api",
      "graphql",
      "-f",
      "query={viewer{login}}",
      "--jq",
      ".data.viewer.login",
    ]);
    expect(run?.cwd).toBe(workspace);
  });

  test("fails with gh's first stderr line, else its exit code", async () => {
    const withStderr = await measure(
      fakeHost({
        run: async (command) =>
          command === "gh"
            ? {
                code: 1,
                stdout: "",
                stderr: "gh: HTTP 401: Bad credentials (https://api.github.com/graphql)\n",
              }
            : { code: 0, stdout: "", stderr: "" },
      })
    );
    expect(withStderr.github).toEqual({
      ok: false,
      detail:
        "gh viewer login failed: gh: HTTP 401: Bad credentials (https://api.github.com/graphql)",
    });
    const silent = await measure(
      fakeHost({
        run: async (command) =>
          command === "gh"
            ? { code: 4, stdout: "", stderr: "" }
            : { code: 0, stdout: "", stderr: "" },
      })
    );
    expect(silent.github).toEqual({ ok: false, detail: "gh viewer login failed: exit code 4" });
  });

  test("passes under a GitHub App installation token, the only credential a Legion role holds", async () => {
    // A role's gh runs on its App's installation token (appauth: the App's `<slug>[bot]`), and
    // GitHub answers REST `GET /user` to an installation token with 403 "Resource not accessible
    // by integration" — read in a Legion pod on 2026-10-10 through the reviewer App's token.
    // GraphQL `{viewer{login}}` is how such a token reads its own identity, and how the
    // repository's proofs read it (internal/api/real_github_test.go,
    // scripts/e2e/stage4b-sandbox-tree.sh), so the row passes with the bot login it answers.
    const installationToken = async (command: string, args: readonly string[]) => {
      if (command !== "gh") return { code: 0, stdout: "", stderr: "" };
      if (
        args[0] === "api" &&
        args[1] === "graphql" &&
        args.some((arg) => arg.includes("viewer"))
      ) {
        return { code: 0, stdout: "legion-reviewer[bot]\n", stderr: "" };
      }
      return {
        code: 1,
        stdout: "",
        stderr: "gh: Resource not accessible by integration (HTTP 403)\n",
      };
    };
    const rows = await measure(fakeHost({ run: installationToken }));
    expect(rows.github.ok).toBe(true);
    expect(rows.github.detail).toContain("legion-reviewer[bot]");
  });

  // The row's call is `legion gh` in a pod, and it tells that command where to write the step it
  // is in (LEGION_GH_STEP_FILE) and when the row gives up on it (LEGION_GH_DEADLINE, Unix
  // milliseconds), so that a miss names the step and the leak guard waits for no rate-limit reset
  // the row would not see. The command and its argv stay as above.
  test("the call's environment names a step file in a directory of mode 0700 and the row's own deadline; dispatch's names neither", async () => {
    const runs: RecordedRun[] = [];
    let stepDirectory:
      | { readonly mode: number; readonly uid: number; readonly link: boolean }
      | undefined;
    let ranAt = 0;
    const host = fakeHost({
      run: async (command, args, options) => {
        runs.push({ command, args, cwd: options.cwd, signal: options.signal, env: options.env });
        if (command !== "gh")
          return { code: 0, stdout: "LEGION-663 · Capability report\n", stderr: "" };
        ranAt = Date.now();
        const info = lstatSync(path.dirname(stepFile(options)));
        stepDirectory = { mode: info.mode & 0o7777, uid: info.uid, link: info.isSymbolicLink() };
        return { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" };
      },
    });
    const before = Date.now();
    const report = await measureCapabilities(host, input());

    expect(report.rows.find((row) => row.name === "github")?.ok).toBe(true);
    const gh = runs.find((run) => run.command === "gh");
    expect(gh?.args).toEqual([
      "api",
      "graphql",
      "-f",
      "query={viewer{login}}",
      "--jq",
      ".data.viewer.login",
    ]);
    expect(Object.keys(gh?.env ?? {}).sort()).toEqual([
      "LEGION_GH_DEADLINE",
      "LEGION_GH_STEP_FILE",
    ]);
    expect(path.isAbsolute(gh?.env?.LEGION_GH_STEP_FILE ?? "")).toBe(true);
    expect(stepDirectory).toEqual({ mode: 0o700, uid: process.getuid?.() ?? -1, link: false });
    // The check started between `before` and its call, and its deadline is that start plus its
    // budget, 8 s.
    expect(gh?.env?.LEGION_GH_DEADLINE).toMatch(/^[1-9][0-9]*$/);
    const deadline = Number(gh?.env?.LEGION_GH_DEADLINE);
    expect(deadline).toBeGreaterThanOrEqual(before + CAPABILITY_CHECK_TIMEOUT_MS);
    expect(deadline).toBeLessThanOrEqual(ranAt + CAPABILITY_CHECK_TIMEOUT_MS);
    expect(runs.find((run) => run.command === "dispatch")?.env).toBeUndefined();
  });

  test("a call that answers before its deadline reads its login, and its step file's directory is gone", async () => {
    const host = fakeHost();
    const rows = await measure(host);
    expect(rows.github).toEqual({ ok: true, detail: "gh viewer login: legion-implementer[bot]" });
    const file = host.runs.find((run) => run.command === "gh")?.env?.LEGION_GH_STEP_FILE ?? "";
    expect(file).not.toBe("");
    expect(existsSync(path.dirname(file))).toBe(false);
  });

  test("a call that fails before its deadline reads gh's first stderr line, and its step file's directory is gone", async () => {
    let file = "";
    const rows = await measure(
      ghHost((options) => {
        file = stepFile(options);
        writeFileSync(file, "gh 2210\n");
        return { code: 1, stdout: "", stderr: "gh: HTTP 502: Bad Gateway\nsecond line\n" };
      })
    );
    expect(rows.github).toEqual({
      ok: false,
      detail: "gh viewer login failed: gh: HTTP 502: Bad Gateway",
    });
    expect(existsSync(path.dirname(file))).toBe(false);
  });

  test("a call still running at its deadline names the step its step file holds, and the deadline removes its step file's directory", async () => {
    let file = "";
    const report = await measureCapabilities(
      ghHost((options) => {
        file = stepFile(options);
        writeFileSync(file, "visibility 6120\n");
        // A call that outlives its abort as well, so the directory is gone at the deadline and
        // not only once the call ends.
        return Promise.withResolvers<never>().promise;
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    expect(report.rows.find((row) => row.name === "github")).toEqual({
      name: "github",
      ok: false,
      detail: "did not finish within 20 ms; at visibility, 6120 ms after legion gh started",
    });
    expect(existsSync(path.dirname(file))).toBe(false);
  });

  test("a call still running at its deadline with no step file says legion gh wrote no step, read before the abort", async () => {
    let file = "";
    const report = await measureCapabilities(
      ghHost((options) => {
        file = stepFile(options);
        // A step written once the signal aborts is past the deadline's reading, which has
        // removed the directory by then, so the write finds no directory.
        const lateStep = () => {
          try {
            writeFileSync(file, "gh 9999\n");
          } catch {
            // The directory is gone.
          }
        };
        options.signal.addEventListener("abort", lateStep, { once: true });
        return untilAborted(options.signal);
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    expect(report.rows.find((row) => row.name === "github")).toEqual({
      name: "github",
      ok: false,
      detail: "did not finish within 20 ms; legion gh wrote no step",
    });
    expect(existsSync(path.dirname(file))).toBe(false);
  });

  // The file holds at most 64 bytes, one line `<step> <ms>`: one of the six steps `legion gh`
  // starts, one space, and a count of milliseconds as Go's %d writes it. Anything else is no step.
  const longest = `visibility ${"1".repeat(52)}`;
  for (const [name, text, detail] of [
    ["64 bytes", `${longest}\n`, `at visibility, ${"1".repeat(52)} ms after legion gh started`],
    ["65 bytes", `${longest}1\n`, "legion gh wrote no step"],
    ["empty", "", "legion gh wrote no step"],
    ["two lines", "check 10\nvisibility 20\n", "legion gh wrote no step"],
    ["no line end", "visibility 6120", "legion gh wrote no step"],
    ["a carriage return before the line end", "visibility 6120\r\n", "legion gh wrote no step"],
    ["a step legion gh does not start", "waiting 6120\n", "legion gh wrote no step"],
    ["a negative count", "visibility -1\n", "legion gh wrote no step"],
    ["a count with a leading zero", "visibility 06120\n", "legion gh wrote no step"],
    ["two spaces", "visibility  6120\n", "legion gh wrote no step"],
    ["no count", "visibility\n", "legion gh wrote no step"],
  ] as const) {
    test(`a call still running at its deadline whose step file holds ${name} reads ${detail}`, async () => {
      const report = await measureCapabilities(
        ghHost((options) => {
          writeFileSync(stepFile(options), text);
          return untilAborted(options.signal);
        }),
        input(),
        { checkMs: 20, reportMs: 500 }
      );
      expect(report.rows.find((row) => row.name === "github")?.detail).toBe(
        `did not finish within 20 ms; ${detail}`
      );
    });
  }

  test("every other row that misses its deadline reads the budget alone", async () => {
    const never = () => Promise.withResolvers<never>().promise;
    const report = await measureCapabilities(
      fakeHost({
        discoverAgents: never,
        runSearchQuery: never,
        loadMCPConfigs: never,
        run: async (command, _args, options) => {
          if (command === "gh") writeFileSync(stepFile(options), "gh 15\n");
          return untilAborted(options.signal);
        },
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    const details = Object.fromEntries(report.rows.map((row) => [row.name, row.detail]));
    for (const name of ["subagents", "web-search", "mcp", "dispatch-envoy-tools"])
      expect({ name, detail: details[name] }).toEqual({
        name,
        detail: "did not finish within 20 ms",
      });
    expect(details.github).toBe(
      "did not finish within 20 ms; at gh, 15 ms after legion gh started"
    );
  });
});

/** The cases both funnels are held to: `cleanDetail` here and the daemon's `capabilities.NewRow`.
 * `patterns` is a detail per credential shape or cleanup rule and what it reads cleaned, each
 * prefixed with `pad` letters `a` where it has one; `branches` is each branch of a check that quotes
 * upstream text, with that text, the credential in it, and the text redacted; `linear`, which only
 * this test reads since the daemon's RE2 cannot backtrack, is a unit that, repeated, would make a
 * backtracking engine read a run once per start in it. */
interface RedactionFixture {
  readonly patterns: readonly {
    readonly pattern: string;
    readonly name: string;
    readonly pad?: number;
    readonly detail: string;
    readonly want: string;
  }[];
  readonly branches: readonly {
    readonly name: string;
    readonly row: LiveCapability;
    readonly upstream: string;
    readonly credential: string;
    readonly redacted: string;
  }[];
  readonly linear: readonly { readonly name: string; readonly unit: string }[];
}

const fixtures = path.resolve(import.meta.dir, "../../contracts/fixtures");
const redaction = JSON.parse(
  readFileSync(path.join(fixtures, "capability-redaction.json"), "utf8")
) as RedactionFixture;

/** The report rows this plugin sends for the fixture's branches, which the daemon's
 * `TestNormalizeKeepsThePluginsReportAsItIs` runs through `Normalize`. `UPDATE_FIXTURES=1` rewrites
 * it from what the plugin sends now. */
const PLUGIN_REPORT = path.join(fixtures, "capability-redaction-plugin-report.json");

describe("cleanDetail", () => {
  for (const { pattern, name, pad = 0, detail, want } of redaction.patterns) {
    test(`${pattern}: ${name}`, () => {
      const padding = "a".repeat(pad);
      expect(cleanDetail(padding + detail)).toBe(padding + want);
      // The daemon cleans what this plugin sends again, so cleaning is idempotent.
      expect(cleanDetail(padding + want)).toBe(padding + want);
    });
  }

  // One test per case, as above, so each case's placements (its length plus 25, at most 1,033) have
  // Bun's per-test budget to themselves; every case's together are about ten thousand.
  for (const { pattern, name, pad, detail } of redaction.patterns) {
    test(`at the cut keeps what came before it and is idempotent: ${pattern}: ${name}, each byte cut once`, () => {
      const problems: string[] = [];
      const whole = cleanDetail(detail);
      for (
        let letters = Math.max(0, 1024 - Buffer.byteLength(detail) - 16);
        letters <= 1024 + 8;
        letters++
      ) {
        const padding = "a".repeat(letters);
        const once = cleanDetail(`${padding} ${detail}`);
        const label = `pushed by ${letters} letters`;
        if (cleanDetail(once) !== once) problems.push(`${label}: not idempotent`);
        if (!once.startsWith(padding.slice(0, 1024))) problems.push(`${label}: lost its padding`);
        if (pad === undefined && !`${padding} ${whole}`.startsWith(once)) {
          problems.push(`${label}: not a prefix of the case cleaned whole`);
        }
      }
      expect(problems).toEqual([]);
    });
  }

  // A rule that may start inside a run its own value class reads through makes JavaScriptCore,
  // which backtracks, read that run once per start: work quadratic in the detail, which the plugin
  // cleans before it cuts it to 1,024 bytes, so one such detail stalls a session's start. Each
  // fixture unit, repeated, holds a start for every repetition.
  //
  // Each row holds `cleanDetail`'s own count of the characters its rules' passes are handed
  // (`credentialRuleCharacters`) to one pass of every rule over the detail, one more after each of
  // `HELD_READINGS`, and one over what the cut keeps. A unit spends less than that where a reading
  // shrinks its text or changes nothing, which sends nothing to the rules again: the three units
  // where only `HELD_READINGS[2]` changes the text (the two U+FFFD units and BEL) spend two passes
  // fewer, so the count allows them two extra passes over the whole detail, and every other unit
  // at most one. A pass loop that did not stop at a pass that changes nothing would hand the rules
  // eight passes for each, which every row fails. JavaScriptCore counts no step a regular
  // expression takes, so the count does not catch a pass that backtracks within itself.
  //
  // The limit on each row is a hang guard. The JWT rule made quadratic by hand cost 13,570 to
  // 14,088 ms on a 2-CPU gVisor agent pod, which the 10 s guard fails; CI is about 4.5 times
  // faster, where that same regression would cost about 3 s and pass (an estimate from those
  // ratios, not a run on CI), so a rule made quadratic is not caught on CI, a risk INFRA-985's
  // architect accepted on 2026-11-07. On bun 1.4.4 a per-test limit is reported for a body that
  // blocks the thread only once that body comes back, so the guard ends a blow-up that returns at
  // all, and a rule that never returns is ended by the pi-legion job's own limit
  // (timeout-minutes: 28, .github/workflows/ci-job-pi-legion.yaml).
  const RULES = CREDENTIAL_RULES.length;
  const READINGS = HELD_READINGS.length;
  for (const { name, unit } of redaction.linear) {
    test(`cleans 64 KiB of ${name} in rule passes bounded by its length`, () => {
      const text = unit.repeat(Math.ceil((64 * 1024) / unit.length));
      const before = credentialRuleCharacters();
      const cleaned = cleanDetail(text);
      const handed = credentialRuleCharacters() - before;
      // No rule redacts anything in these texts, so each reading settles in its first pass.
      expect(cleaned).not.toContain("***");
      // The cut keeps at most MAX_DETAIL_BYTES UTF-8 bytes, and so at most that many characters.
      const allowed = RULES * ((1 + READINGS) * text.length + MAX_DETAIL_BYTES);
      expect(
        handed,
        `cleaning 64 KiB of ${name} handed the rules ${handed} characters, where at most ${allowed} (${RULES} rules, each over the ${text.length} characters once and once more after each of ${READINGS} readings, and over the ${MAX_DETAIL_BYTES} the cut keeps) are allowed`
      ).toBeLessThanOrEqual(allowed);
    }, 10_000);
  }
});

describe("credential redaction", () => {
  test("every row passes cleanDetail: each branch that quotes upstream text carries it redacted", async () => {
    const upstream = (row: LiveCapability): string => {
      const branch = redaction.branches.find((candidate) => candidate.row === row);
      if (branch === undefined) throw new Error(`the fixture has no branch on the ${row} row`);
      return branch.upstream;
    };
    const report = await measureCapabilities(
      fakeHost({
        runSearchQuery: async () => ({ content: [], details: { error: upstream("web-search") } }),
        discoverAgents: async () => {
          throw new Error(`${upstream("subagents")}\n    at discoverAgents`);
        },
        run: async (command) =>
          command === "dispatch"
            ? { code: 1, stdout: "", stderr: `${upstream("dispatch-envoy-tools")}\nsecond line\n` }
            : { code: 1, stdout: "", stderr: `${upstream("github")}\n` },
        loadMCPConfigs: async () => ({
          configs: { alpha: { url: "https://mcp.example/sse" } },
          sources: {},
        }),
        discoverMCPServers: async () => ({
          manager: {
            waitForPendingConnections: async () => undefined,
            getConnectionStatus: () => upstream("mcp"),
            disconnectAll: async () => undefined,
          },
          errors: [],
        }),
      }),
      input()
    );

    const details = Object.fromEntries(report.rows.map((row) => [row.name, row.detail]));
    for (const { name, row, redacted } of redaction.branches) {
      expect({ name, detail: details[row] }).toEqual({
        name,
        detail: expect.stringContaining(redacted),
      });
    }
    const wire = JSON.stringify(report);
    for (const { credential } of redaction.branches) expect(wire).not.toContain(credential);

    const sent = { rows: report.rows };
    const written = `${JSON.stringify(sent, null, 2)}\n`;
    if (process.env.UPDATE_FIXTURES === "1") {
      writeFileSync(PLUGIN_REPORT, written);
    }
    const committed = readFileSync(PLUGIN_REPORT, "utf8");
    expect(JSON.parse(committed)).toEqual(sent);
    // The file is what UPDATE_FIXTURES=1 writes, byte for byte: biome.json's formatter excludes it,
    // as it does the other generated JSON, so neither a formatter nor a hand edit moves it.
    expect(committed).toBe(written);
  });
});
