import { afterEach, expect, test } from "bun:test";
import { mkdir, readFile, symlink, writeFile } from "node:fs/promises";
import * as path from "node:path";
import type { LegionState } from "@legion/contracts/legion-api";
import { sessionIdFromTranscript } from "@legion/pi-shared/jj-attribution";
import {
  type Block,
  type Cleanup,
  call,
  deliveryFor,
  messageStream,
  ompNatsGuard,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  toolResultsIn,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";

// On the real Oh My Pi: the `task` subagent of a Legion role pane makes a jj commit, and the
// commit carries the two trailers of the pane: `Omp-Session` with the session of the pane, and
// `Legion-Role` with its role. The plugin writes both lines into a jj config overlay when the
// claim registers (`exportJjSessionAttribution`, `@legion/pi-shared/jj-attribution`) and puts
// the overlay on `JJ_CONFIG`. A subagent runs in the process of its parent, so its shell gets the
// same overlay. Only the real binary can show that the shell of the subagent gets it. The case
// runs once for each of the implementer, the tester and the reviewer.
// The shell of the subagent also prints `DISPATCH_HOST`, `DISPATCH_SESSION_ID` and `LEGION_ROLE`.
// `legion git` (packages/daemon/cmd/legion/git_commit.go) reads these three to add the same two
// trailers to a `git commit`.
// The pane is a role session as a pod runs one: pi-envoy's extension and the root of this
// package (the root, so that Oh My Pi finds `agents/deep-worker.md`), the LEGION_* environment
// of the role against a stand-in daemon and listener, and real NATS. Its PATH holds the jj of
// this test process, and its LEGION_STATE_DIR is in the scratch root, so the plugin writes the
// overlay there. It has no LEGION_ISSUE_TRAILER, so the overlay has no `Dispatch:` line.
// LEGION_TEST_OMP names the binary and LEGION_TEST_NATS_URL the NATS server, as in
// legion-background-subagent-omp.test.ts. A run without both skips, except on GitHub Actions,
// where the pi-legion job supplies both (`ompNatsGuard`).
const { skip, setup } = ompNatsGuard();

/** The roles whose subagents the case runs. */
type Role = "implementer" | "tester" | "reviewer";

/** The tree and the issue of the pane. The state of the daemon below puts the issue in a phase
 * that none of the three roles works. */
const TREE = "TRAILERS-1";
const ISSUE = "TRAILERS-2";

/**
 * The text that marks the assignment of the daemon, which the case sends as the RPC prompt. Each
 * turn of the role's own conversation carries it, and no other request that the pane sends to the
 * gateway does. So it tells a turn of the role from a turn of the subagent and from a side request.
 */
const ASSIGNMENT_MARK = "TRAILERS-ROLE-ASSIGNMENT";

/**
 * The first line of the body of agents/deep-worker.md, which Oh My Pi puts in the system prompt
 * of the subagent. This tells the Messages requests of the subagent from the others, never their
 * place in the order: the pane also sends side requests (the task label, the run-end self-check),
 * and those arrive among the turns.
 */
const DEEP_WORKER_SYSTEM = "You are an autonomous coding worker";

/** The message of the commit that the subagent makes. */
const COMMIT_MESSAGE = "subagent commit";

/** What the subagent reports. The finished task brings it back to the role. */
const SUBAGENT_REPORT = "TRAILERS-SUBAGENT-REPORT: the commit is made.";

/** The variables that `legion git` reads, in the order that the subagent's shell prints them. */
const PRINTED = ["DISPATCH_HOST", "DISPATCH_SESSION_ID", "LEGION_ROLE"] as const;

/** How long the case waits for one thing that the pane owes it before the case fails on it. */
const DEADLINE_MS = 60_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Whether a Messages request is the subagent's own: only its system prompt is deep-worker's. */
function isSubagentRequest(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(DEEP_WORKER_SYSTEM);
}

/**
 * Whether a Messages request is a side turn: pi-envoy's run-end self-check. The host sends it as
 * an ordinary Messages request over a snapshot of a conversation, and its last message is the
 * `<btw>` block.
 */
function isSideTurn(request: Request): boolean {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null).includes("<btw>");
}

/** Whether a Messages request is a turn of the role's own conversation: it carries the assignment
 * of the daemon, and it is not a side turn. */
function isRoleTurn(request: Request): boolean {
  if (isSubagentRequest(request) || isSideTurn(request)) return false;
  return JSON.stringify(request.body.messages ?? null).includes(ASSIGNMENT_MARK);
}

/** One Messages request of the subagent that the stand-in holds until the case answers it. */
interface Held {
  readonly request: Request;
  /** Answers the request with `blocks`. */
  reply(blocks: readonly Block[]): void;
}

/** Polls `probe` until it returns a value. Fails on `what` at the deadline or when omp exits. */
async function until<T>(
  what: string,
  probe: () => T | undefined,
  closed: { readonly error?: unknown }
): Promise<T> {
  const deadline = Date.now() + DEADLINE_MS;
  for (;;) {
    // The pane is a real process with no event that the case can await, so the case polls it.
    if (closed.error !== undefined) throw closed.error;
    const value = probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await Bun.sleep(10);
  }
}

/** The role pane that the case runs, and what the case can do with it. */
interface RolePane {
  /** Each request that the stand-in served, in the order of arrival. */
  readonly requests: readonly Request[];
  /** The scratch root of the pane. The subagent makes its repository here, out of the workspace. */
  readonly root: string;
  /** The home of the pane, which its jj read and wrote. */
  readonly home: string;
  /** The jj of this test process, which the pane's PATH finds too. */
  readonly jj: string;
  /** Waits for the next Messages request of the subagent that the case has not taken. */
  next(what: string): Promise<Held>;
  /** Polls `probe` until it returns a value. Fails at the deadline or when omp exits. */
  until<T>(what: string, probe: () => T | undefined): Promise<T>;
}

/**
 * Starts one Legion pane of `role` on the real Oh My Pi: pi-envoy's extension and the root of
 * this package, the LEGION_* environment of the role against a stand-in that is the model
 * gateway, the claim, grant and state routes of the daemon, and the Envoy listener, and the real
 * NATS server. The state of the daemon puts the issue in the merging phase, which none of the
 * three roles works. So the phase-stall follow-up (src/phase-stall.ts) sends no reminder and adds
 * no turn. The stand-in answers the turns of the role at once: the first one with the `task`
 * call, and each later one with text. It holds each Messages request of the subagent for the
 * case. It answers a side request at once with one word.
 */
async function startRolePane(role: Role): Promise<RolePane> {
  const { binary, nats } = setup();
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { root, home, workspace, sessions } = await ompRoot(binary, "legion-trailers-", cleanup);
  const state = path.join(root, "state");
  const bin = path.join(root, "bin");
  await mkdir(bin, { recursive: true });
  await mkdir(state, { recursive: true, mode: 0o700 });
  // The pane's PATH is the harness's fixed one and `bin`, and CI's jj is in ~/.local/bin
  // (.github/actions/install-jj). So the jj of this process goes in `bin`: the plugin finds it for
  // the base of the overlay chain (`userConfigChain`), and the subagent's shell commits with it.
  const jj = Bun.which("jj");
  if (jj === null) throw new Error("jj is not on PATH");
  await symlink(jj, path.join(bin, "jj"));

  const held: Held[] = [];
  let sides = 0;
  let roleTurns = 0;
  let grants = 0;
  const { requests, base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      if (isSubagentRequest(request) && !isSideTurn(request)) {
        const answer = Promise.withResolvers<string>();
        const index = held.length;
        held.push({
          request,
          reply(blocks) {
            answer.resolve(messageStream(blocks, `msg_${index}`));
          },
        });
        return new Response(await answer.promise, {
          headers: { "content-type": "text/event-stream" },
        });
      }
      if (isRoleTurn(request)) {
        roleTurns += 1;
        return new Response(messageStream(roleReply(roleTurns), `role_${roleTurns}`), {
          headers: { "content-type": "text/event-stream" },
        });
      }
      sides += 1;
      return new Response(messageStream([{ type: "text", text: "PROCEEDING" }], `side_${sides}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: `legion-trailers-${ISSUE.toLowerCase()}-${role}`,
        tree: TREE,
        issue: ISSUE,
        role,
        generation: 1,
        secret: "trailers-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `trailers-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname === "/legion/v1/state") return Response.json(MERGING);
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // The Envoy listener: the registration, the role claim, and each read, answered with an
    // interest. The answer to a registration gives a consumer on the session's own relay subject.
    // Without one the pane is deaf, and its claim boot fails (INFRA-771).
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "trailers-machine",
      dir: workspace,
      topics: [],
      ...deliveryFor(url.pathname, body, "ses_trailers"),
    });
  });
  // A request that the stand-in still holds when the case ends gets an answer, so the stand-in
  // stops at once.
  cleanup.push(async () => {
    for (const request of held) request.reply([{ type: "text", text: "Case over." }]);
  });
  // The profile maps the `deep` model role, which agents/deep-worker.md asks for as `@deep`.
  await writeStandinProfile(home, base, ["  deep: standin/standin-model"]);

  const closed: { error?: unknown } = {};
  const rpc = spawnRpc(
    binary,
    {
      extensions: [
        path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
        // The package root, not its extension file: Oh My Pi finds the `agents/` of an extension
        // package from its root, and `deep-worker` is there.
        path.resolve(import.meta.dir, ".."),
      ],
      home,
      workspace,
      sessions,
      bin,
      env: {
        ENVOY_URL: base,
        ENVOY_NATS_URL: nats,
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: role,
        LEGION_TREE: TREE,
        LEGION_ISSUE: ISSUE,
        LEGION_GENERATION: "1",
        LEGION_PROJECT: "trailers",
        LEGION_BOOT_TOKEN: "trailers-boot",
        LEGION_STATE_DIR: state,
        LEGION_WORKSPACE: workspace,
        // The git identity that the daemon puts on each pane (runtime.GitIdentity), so the jj of
        // the subagent commits as a named author.
        JJ_USER: "Legion Test",
        JJ_EMAIL: "legion-test@example.invalid",
      },
      onFrame: () => undefined,
    },
    cleanup
  );
  rpc.closed.catch((error: unknown) => {
    closed.error = error;
  });
  rpc.send({ type: "prompt", message: `Work ${ISSUE} as the ${role}. ${ASSIGNMENT_MARK}.` });

  let taken = 0;
  return {
    requests,
    root,
    home,
    jj,
    async next(what) {
      const request = await until(what, () => held[taken], closed);
      taken += 1;
      return request;
    },
    until: (what, probe) => until(what, probe, closed),
  };
}

/** The answer to turn `turn` of the role: the first calls `task` with one `deep-worker` task, and
 * each later one is text. */
function roleReply(turn: number): readonly Block[] {
  if (turn > 1) return [{ type: "text", text: "Noted." }];
  return [
    call("task", {
      context: `The commit check of ${ISSUE}.`,
      tasks: [
        {
          name: "DeepWorker",
          agent: "deep-worker",
          task: "Make one jj commit in a new repository and report what the shell printed.",
          solutionSpace: "one command: the command is given",
        },
      ],
    }),
  ];
}

/**
 * The description of `@-` in `repo`, as this process's own jj reads it. `JJ_CONFIG` names an empty
 * file, so no config of the runner takes part in the read. `HOME` is the home of the pane, so jj
 * finds the config of the repository where the pane's jj put it, and writes nothing in the home of
 * the runner. `--ignore-working-copy` stops the read from making a snapshot of its own.
 */
async function committedDescription(pane: RolePane, repo: string): Promise<string> {
  const emptyConfig = path.join(pane.root, "empty-jj-config.toml");
  await writeFile(emptyConfig, "");
  const child = Bun.spawn(
    [
      pane.jj,
      "--ignore-working-copy",
      "--no-pager",
      "--color=never",
      "-R",
      repo,
      "log",
      "-r",
      "@-",
      "--no-graph",
      "-T",
      "description",
    ],
    {
      env: { PATH: process.env.PATH ?? "", HOME: pane.home, JJ_CONFIG: emptyConfig },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj log in ${repo} exited ${code}:\n${stderr}`);
  return stdout;
}

/** The state of the daemon: the issue is in the merging phase, which is the merger's. */
const MERGING = {
  daemon: {
    project: "TRAILERS",
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-10-30T14:03:27Z",
    startedAt: "2026-10-31T09:15:02Z",
  },
  admission: { cap: 1, active: [TREE], waiting: [], trees: [TREE], blocked: [] },
  issues: {
    [ISSUE]: {
      key: ISSUE,
      generation: 1,
      phase: "merging",
      status: "in_progress",
      repo: "acme/widgets",
      defaultBranch: "main",
      workers: {},
    },
  },
  pendingStatusWrites: [],
  heldOutboxRows: [],
  strandedClaims: [],
  wakesWaiting: [],
  capabilities: [],
  repositories: {
    owners: ["acme"],
    mapped: [{ repo: "acme/widgets", default: true }],
    refused: [],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { review_round_cap: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
} satisfies LegionState;

for (const role of ["implementer", "tester", "reviewer"] as const) {
  test.skipIf(skip)(
    `${role === "implementer" ? "an" : "a"} ${role}'s task subagent commits with the ${role}'s session and Legion-Role: ${role}`,
    async () => {
      const pane = await startRolePane(role);
      const repo = path.join(pane.root, "subagent-repo");

      // The first turn of the role calls `task`. The first request of the subagent gets one bash
      // call. The call makes a new jj repository out of the workspace and commits one file in it.
      // Then it prints the description of the commit and the three variables of `PRINTED`.
      const first = await pane.next("the subagent's first model request");
      const command = [
        `jj git init '${repo}'`,
        `cd '${repo}'`,
        "printf 'probe\\n' > probe.txt",
        `jj commit -m '${COMMIT_MESSAGE}'`,
        "jj --no-pager --color=never log -r @- --no-graph -T description",
        ...PRINTED.map((name) => `printf '${name}=%s\\n' "$${name}"`),
      ].join(" && ");
      first.reply([call("bash", { command })]);

      // (1) The next request of the subagent carries the result of that call: no error, and the
      // description of the commit.
      const after = await pane.next("the subagent's request after its bash call");
      const result = toolResultsIn(after.request).at(-1);
      if (result === undefined) throw new Error("the subagent's request carried no tool result");
      expect(result.is_error).toBe(false);
      const description = await committedDescription(pane, repo);
      expect(result.text).toContain(description.trimEnd());

      // (2) The commit carries the session that the pane registered and the role of the pane. The
      // session id is the one in the name of the transcript that the registration gave, which is
      // the id that the overlay writes, and it is the registration's `sessionId` too.
      const registration = await pane.until(
        "the pane's claim registration",
        () => pane.requests.find((request) => request.path === "/legion/v1/claims/register")?.body
      );
      const transcript = registration.ompSessionFile;
      const sessionId =
        typeof transcript === "string" ? sessionIdFromTranscript(transcript) : undefined;
      const lines = description.split("\n");
      expect(lines).toContain(`Omp-Session: ${sessionId}`);
      expect(lines).toContain(`Legion-Role: ${role}`);
      expect(registration.sessionId).toBe(sessionId);

      // (3) The shell of the subagent holds the three variables that `legion git` reads.
      const printed = Object.fromEntries(
        PRINTED.map((name) => [
          name,
          new RegExp(`^${name}=([^\\r\\n]*)`, "m").exec(result.text)?.[1],
        ])
      );
      expect(printed).toEqual({
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: sessionId,
        LEGION_ROLE: role,
      });

      // (4) The subagent ends, and its report reaches the role. No request of the role's process
      // other than the subagent's holds the result of the bash call.
      after.reply([{ type: "text", text: SUBAGENT_REPORT }]);
      await pane.until("the role's turn that carries the subagent's report", () =>
        pane.requests.find(
          (request) =>
            isRoleTurn(request) &&
            JSON.stringify(request.body.messages ?? null).includes(SUBAGENT_REPORT)
        )
      );
      const rootResults = pane.requests
        .filter((request) => request.path === "/anthropic/v1/messages")
        .filter((request) => !isSubagentRequest(request))
        .flatMap(toolResultsIn);
      expect(
        rootResults.filter(
          (rootResult) =>
            rootResult.tool_use_id === result.tool_use_id ||
            rootResult.text.includes(description.trimEnd())
        )
      ).toEqual([]);
    },
    120_000
  );
}
