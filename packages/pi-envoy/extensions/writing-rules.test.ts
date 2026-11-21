import { afterEach, beforeEach, expect, mock, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runDispatchCli } from "@legion/envoy-client/dispatch-cli";
import { forgetKeyedLayerMemory } from "@legion/envoy-client/dispatch-http";
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { createStandInHost, type SessionContext } from "../test/stand-in-host";

// The written rules under Oh My Pi (INFRA-3231, R6): the `dispatch` command refuses once the
// session's first write of a comment, and its first after each compaction, showing the rules
// Dispatch sends for that write's reader. The plugin's `session_compact` handler counts each
// compaction in the command's state file. The extension runs here on the stand-in host with
// Dispatch on and Envoy off, and the command runs in this process against a stand-in Dispatch, as
// `extensions/envoy.test.ts` drives them.

// The host package is not installed in this workspace: the entry reads these two of its modules,
// stood in for as `extensions/envoy.test.ts` stands them in.
mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));
mock.module("@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler", () => ({
  listUserInputBuiltinCommands: () => [],
}));

restoreEnvironment([
  "PATH",
  "HOME",
  "TMUX",
  "TMUX_PANE",
  "ENVOY_CONFIG_FILE",
  "ENVOY_URL",
  "ENVOY_NATS_URL",
  "ENVOY_TOKEN",
  "ENVOY_TOKEN_FILE",
  "NATS_NKEY_SEED",
  "NATS_NKEY_SEED_FILE",
  "OMP_PROFILE",
  "PI_CODING_AGENT_DIR",
  "XDG_CONFIG_HOME",
  "DISPATCH_URL",
  "DISPATCH_TOKEN",
  "DISPATCH_TOKEN_FILE",
  "DISPATCH_HOST",
  "DISPATCH_SESSION_ID",
  "DISPATCH_STATE_DIR",
]);

const RULES_TOKEN = fixtureSecret("dsp");
const SESSION = "ses_writing_rules";
const RULES =
  "# Write so a reader understands it on the first read\n\nThe rules the stand-in Dispatch sends.\n";

/** What the stand-in Dispatch says about the refusal, and every request it answered. */
const dispatch = {
  refusal: "on" as "on" | "off",
  requests: [] as { readonly method: string; readonly path: string; readonly body: unknown }[],
};

const standInDispatch = (async (input: string | URL | Request, init?: RequestInit) => {
  const request = new Request(input, init);
  const { pathname } = new URL(request.url);
  const text = await request.text();
  const body: unknown = text === "" ? undefined : JSON.parse(text);
  dispatch.requests.push({ method: request.method, path: pathname, body });
  if (pathname === "/api/v1/settings/writing-rules" && request.method === "GET") {
    return Response.json({ refusal: dispatch.refusal, extra: [] });
  }
  if (pathname === "/api/v1/writing-rules" && request.method === "POST") {
    return Response.json({ reader: "ann@example.com", text: RULES });
  }
  if (pathname === "/api/v1/issues/DSP-1/comments" && request.method === "POST") {
    return Response.json({
      id: `comment-${dispatch.requests.length}`,
      issue_key: "DSP-1",
      advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
    });
  }
  return Response.json(
    { code: "NOT_FOUND", error: `no route for ${request.method} ${pathname}` },
    { status: 404 }
  );
}) as typeof fetch;

const originalFetch = globalThis.fetch;
let scratch = "";

beforeEach(() => {
  scratch = mkdtempSync(join(tmpdir(), "pi-envoy-writing-rules-"));
  dispatch.refusal = "on";
  dispatch.requests.length = 0;
  globalThis.fetch = standInDispatch;
  // Dispatch on and Envoy off: a file with a Dispatch section alone, AskGate off, and no Envoy
  // address, so the session registers nothing and the extension's own reads reach the stand-in.
  process.env.HOME = scratch;
  process.env.DISPATCH_STATE_DIR = join(scratch, "state");
  process.env.ENVOY_CONFIG_FILE = join(scratch, "envoy.json");
  for (const name of [
    "ENVOY_URL",
    "ENVOY_NATS_URL",
    "ENVOY_TOKEN",
    "ENVOY_TOKEN_FILE",
    "NATS_NKEY_SEED",
    "NATS_NKEY_SEED_FILE",
    "OMP_PROFILE",
    "PI_CODING_AGENT_DIR",
    "XDG_CONFIG_HOME",
    "DISPATCH_URL",
    "DISPATCH_TOKEN",
    "DISPATCH_TOKEN_FILE",
    "DISPATCH_SESSION_ID",
    "TMUX",
    "TMUX_PANE",
  ]) {
    delete process.env[name];
  }
  writeFileSync(
    process.env.ENVOY_CONFIG_FILE,
    `${JSON.stringify({
      askgate: { enabled: false },
      dispatch: { enabled: true, serverUrl: "http://dispatch.test", token: RULES_TOKEN },
    })}\n`
  );
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  forgetKeyedLayerMemory();
  rmSync(scratch, { recursive: true, force: true });
});

const context: SessionContext = {
  cwd: "/tmp/pi-envoy-writing-rules",
  agent: { kind: "main" },
  sessionManager: {
    getSessionId: () => SESSION,
    getSessionFile: () => undefined,
    ensureOnDisk: async () => undefined,
    getEntries: () => [],
  },
  setInterval: () => undefined,
  setTimeout: () => undefined,
  ui: {
    notify: () => undefined,
    onTerminalInput: () => () => undefined,
    getEditorText: () => "",
  },
};

/** The extension, loaded fresh under `row`, with its session started. The import is dynamic for
 *  two reasons: the entry reads the host package, so it loads only below the stand-ins above, and
 *  it picks its envoy.json when its module loads, so each row needs an instance of its own. */
async function startedSession(row: string) {
  const { default: envoyExtension } = await import(`./envoy.ts?${row}`);
  const host = createStandInHost();
  envoyExtension(host.pi);
  await host.handlers.get("session_start")?.({}, context);
  expect(process.env.DISPATCH_SESSION_ID).toBe(SESSION);
  return {
    compact: async () => {
      await host.handlers.get("session_compact")?.({}, context);
    },
    shutdown: async () => {
      await host.handlers.get("session_shutdown")?.({}, context);
    },
  };
}

/** One `dispatch comment` in the session's shell, as the agent sends it. */
async function comment(
  body: string,
  extra: Record<string, string> = {}
): Promise<{ readonly code: number; readonly stdout: string }> {
  let stdout = "";
  const code = await runDispatchCli(
    ["comment", "--issue", "DSP-1", "--body", body],
    { ...process.env, DISPATCH_HOST: "omp", ...extra },
    {
      stdout: (text) => {
        stdout += text;
      },
      stderr: () => undefined,
      readText: (path) => readFileSync(path, "utf8"),
      cwd: scratch,
      fetchImpl: standInDispatch,
    }
  );
  return { code, stdout };
}

const posted = () =>
  dispatch.requests.filter(
    (request) => request.method === "POST" && request.path === "/api/v1/issues/DSP-1/comments"
  );
const asked = (path: string) => dispatch.requests.filter((request) => request.path === path);

// Each row loads the extension and runs the command up to four times, so it takes the 30 s bound
// `extensions/envoy.test.ts` gives such rows: Bun's 5 s default aborts a row on a busy machine, and
// an aborted row skips its `finally` and leaves its requests to the next row's counts.
const ROW_MS = 30_000;

test(
  "the first write is refused once with the rules, the second posts, and a compaction brings the refusal back once",
  async () => {
    const session = await startedSession("writing-rules-on");
    try {
      const first = await comment("The deploy finished at 14:02.");
      expect(first.code).toBe(1);
      expect(first.stdout).toStartWith(
        "dispatch comment was not called: this call is the first write of this session"
      );
      expect(first.stdout).toContain(RULES.trimEnd());
      expect(posted()).toEqual([]);
      expect(asked("/api/v1/writing-rules").map((request) => request.body)).toEqual([
        {
          route: "POST /api/v1/issues/DSP-1/comments",
          body: expect.objectContaining({ body: "The deploy finished at 14:02." }),
        },
      ]);

      const second = await comment("The deploy finished at 14:02.");
      expect(second.code).toBe(0);
      expect(second.stdout).toContain("Posted comment");
      expect(posted()).toHaveLength(1);

      await session.compact();

      const third = await comment("The rollback took 3 minutes.");
      expect(third.code).toBe(1);
      expect(third.stdout).toStartWith(
        "dispatch comment was not called: this call is the first write of this session"
      );
      expect(third.stdout).toContain(RULES.trimEnd());
      expect(posted()).toHaveLength(1);
      expect(asked("/api/v1/writing-rules").map((request) => request.body)).toEqual([
        {
          route: "POST /api/v1/issues/DSP-1/comments",
          body: expect.objectContaining({ body: "The deploy finished at 14:02." }),
        },
        {
          route: "POST /api/v1/issues/DSP-1/comments",
          body: expect.objectContaining({ body: "The rollback took 3 minutes." }),
        },
      ]);

      const fourth = await comment("The rollback took 3 minutes.");
      expect(fourth.code).toBe(0);
      expect(posted()).toHaveLength(2);
      // The command kept Dispatch's answer about the refusal: one read served all four calls.
      expect(asked("/api/v1/settings/writing-rules")).toHaveLength(1);
    } finally {
      await session.shutdown();
    }
  },
  ROW_MS
);

test(
  "with the refusal off, nothing is refused, before or after a compaction",
  async () => {
    dispatch.refusal = "off";
    const session = await startedSession("writing-rules-off");
    try {
      // The steps of the row above: write, write, compaction, write, write. Each write posts.
      const first = await comment("The deploy finished at 14:02.");
      expect(first.code).toBe(0);
      expect(first.stdout).toContain("Posted comment");
      expect(posted()).toHaveLength(1);

      const second = await comment("The deploy finished at 14:02.");
      expect(second.code).toBe(0);
      expect(second.stdout).toContain("Posted comment");
      expect(posted()).toHaveLength(2);

      await session.compact();

      const third = await comment("The rollback took 3 minutes.");
      expect(third.code).toBe(0);
      expect(third.stdout).toContain("Posted comment");
      expect(posted()).toHaveLength(3);

      const fourth = await comment("The rollback took 3 minutes.");
      expect(fourth.code).toBe(0);
      expect(fourth.stdout).toContain("Posted comment");
      expect(posted()).toHaveLength(4);
      expect(asked("/api/v1/writing-rules")).toEqual([]);
    } finally {
      await session.shutdown();
    }
  },
  ROW_MS
);

test(
  "a call a person typed (DISPATCH_PERSON_TYPED=1) is never refused, and asks nothing about the rules",
  async () => {
    const session = await startedSession("writing-rules-person-typed");
    try {
      const typed = await comment("The deploy finished at 14:02.", { DISPATCH_PERSON_TYPED: "1" });
      expect(typed.code).toBe(0);
      expect(posted()).toHaveLength(1);
      expect(asked("/api/v1/settings/writing-rules")).toEqual([]);
      expect(asked("/api/v1/writing-rules")).toEqual([]);
    } finally {
      await session.shutdown();
    }
  },
  ROW_MS
);
