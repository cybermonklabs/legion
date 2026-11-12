import { afterEach, beforeAll, describe, expect, it, mock } from "bun:test";
import { symlinkSync } from "node:fs";
import path from "node:path";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// The module reaches the Oh My Pi host through `@legion/pi-shared/subagent-session`, which only
// the binary resolves; the import below must come after the mock.
mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));

const { attributionInputs, createClaimSession } = await import("./claim-session");

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";

// The pane's workspace reaches `attributionInputs` as the daemon set it; jj matches a scope
// against the root it resolved, so a link's own path must never reach the overlay. The path is
// reserved here and the directory made in the `beforeAll` the fixture registers, so a `bun test
// -t <pattern>` that selects no row of this file makes nothing and leaves nothing behind; the
// link is made in a hook of this file's own, which Bun runs after the fixture's.
const REAL_WORKSPACE = temporaryDirectory("claim-session-ws", { real: true });
const WORKSPACE = path.join(REAL_WORKSPACE, "link");
beforeAll(() => {
  symlinkSync(REAL_WORKSPACE, WORKSPACE);
});

describe("attributionInputs", () => {
  it("reads the session id, the state directory, the trailer key and the workspace", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
        LEGION_WORKSPACE: WORKSPACE,
      })
    ).toEqual({
      sessionId: ROOT_ID,
      stateDir: "/state",
      issueKey: "ACME-7",
      workspace: REAL_WORKSPACE,
    });
  });

  // jj matches a `[[--scope]]` against the workspace root it resolved, and a path that does not
  // resolve matches nothing and says nothing: the trailer is then the session line alone.
  it.each([
    ["no LEGION_WORKSPACE", undefined],
    ["an empty LEGION_WORKSPACE", ""],
    ["a workspace that does not exist", "/no/such/workspace"],
  ])("takes no workspace from %s", (_name, workspace) => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
        ...(workspace === undefined ? {} : { LEGION_WORKSPACE: workspace }),
      }).workspace
    ).toBeUndefined();
  });

  // The daemon sets LEGION_ISSUE for every pane; it sets LEGION_ISSUE_TRAILER only for an issue
  // whose repository the deployment names. A caller reading LEGION_ISSUE would stamp a Dispatch
  // key into the commits of a repository it never named (dispatch://INFRA-472).
  it("takes no key from LEGION_ISSUE", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, { LEGION_STATE_DIR: "/state", LEGION_ISSUE: "ACME-7" })
        .issueKey
    ).toBeUndefined();
  });

  it("takes no key from an empty LEGION_ISSUE_TRAILER", () => {
    expect(
      attributionInputs(ROOT_TRANSCRIPT, { LEGION_STATE_DIR: "/state", LEGION_ISSUE_TRAILER: "" })
        .issueKey
    ).toBeUndefined();
  });

  it("yields no session id for a subagent transcript", () => {
    expect(
      attributionInputs("/sessions/-x/2026-08-30T03-47-40-329Z_abc/Worker.jsonl", {
        LEGION_STATE_DIR: "/state",
        LEGION_ISSUE_TRAILER: "ACME-7",
      }).sessionId
    ).toBeUndefined();
  });

  it("fails the pane that has no state directory", () => {
    expect(() => attributionInputs(ROOT_TRANSCRIPT, {})).toThrow(
      "LEGION_STATE_DIR is required for Legion"
    );
  });
});

describe("createClaimSession.bootstrap", () => {
  // The boot never reaches the daemon in these rows: the transcript throws before the claim boot
  // is even called, which is the one failure that still comes back out of `bootstrap`.
  const unreachedDaemon = {} as LegionDaemonClient;

  const paneContext = (sessionId: string): SessionContext => ({
    cwd: "/workspace",
    hasUI: true,
    sessionManager: {
      getSessionId: () => sessionId,
      getSessionFile: () => undefined,
      ensureOnDisk: async () => undefined,
      getEntries: () => [],
    },
    setInterval: () => undefined,
    setTimeout: () => undefined,
    ui: { notify: () => undefined },
  });

  afterEach(() => {
    delete process.env.LEGION_BOOT_TOKEN;
  });

  // A boot that failed is no boot this session has. Memoised, the rejected promise would be
  // handed to every later `session_start` of this process, so a pane whose transcript was not
  // yet on disk at its first start would never register a claim and never be told why again. In
  // the daemon's agent the cover around `session_start` ends the process on that line; the
  // sessions it deliberately does not end are the ones this rule is for.
  it("boots again after a boot that failed, and keeps the failure", async () => {
    process.env.LEGION_BOOT_TOKEN = "boot-token";
    const failures = ["the transcript could not be written", "the transcript is still not there"];
    const asked: string[] = [];
    const session = createClaimSession({
      daemon: () => unreachedDaemon,
      persistedTranscript: async (context) => {
        asked.push(context.sessionManager.getSessionId());
        throw new Error(failures[asked.length - 1] ?? "asked once too often");
      },
      exitProcess: () => {
        throw new Error("the claim session ended the process");
      },
    });

    await expect(session.bootstrap(paneContext("ses_worker"))).rejects.toThrow(failures[0]);
    await expect(session.bootstrap(paneContext("ses_worker"))).rejects.toThrow(failures[1]);

    expect(asked).toEqual(["ses_worker", "ses_worker"]);
  });

  // A boot still running is still the boot this session has: the second caller waits on the
  // first rather than starting one of its own.
  it("waits on the boot in flight rather than starting a second", async () => {
    process.env.LEGION_BOOT_TOKEN = "boot-token";
    const started = Promise.withResolvers<void>();
    let asked = 0;
    const session = createClaimSession({
      daemon: () => unreachedDaemon,
      persistedTranscript: async () => {
        asked += 1;
        await started.promise;
        throw new Error("the transcript could not be written");
      },
      exitProcess: () => {
        throw new Error("the claim session ended the process");
      },
    });

    // Both are waited on before the boot settles: a rejection nothing is listening for yet is
    // an unhandled one, which the runner fails the row for.
    const first = session.bootstrap(paneContext("ses_worker"));
    const second = session.bootstrap(paneContext("ses_worker"));
    const both = Promise.allSettled([first, second]);
    started.resolve();

    expect((await both).map((settled) => settled.status)).toEqual(["rejected", "rejected"]);
    expect(asked).toBe(1);
  });
});
