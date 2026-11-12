import { afterEach, expect, test } from "bun:test";
import { cp, mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import * as path from "node:path";
import { CLAIM_BOOT_BUDGET_MS } from "@legion/pi-shared/daemon-client";
import {
  type Block,
  type Cleanup,
  call,
  messageStream,
  ompRoot,
  type Request,
  type Rpc,
  refusingNats,
  serveStandin,
  spawnInteractive,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { z } from "zod";

// INFRA-2883 on the real binary, as a standing guard rather than one hand-run command at one
// head: the rows of dispatch://INFRA-2883/artifact/verification-md's k2, k3, k5 and k7. A Legion
// role's Oh My Pi process ends at every ending that is not a registered claim — a daemon that
// refuses the connection, one that accepts it and answers nothing, one that registers and never
// answers `claims/ready`, and a pi-legion root that threw at load — and every session that is
// not an agent the daemon launched for a role keeps running, a healthy role among them.
//
// Each pane is the pinned Oh My Pi as a pod runs one: both plugin package directories as its
// `--extension` roots (the roots, not their entry files, as `agentArgv` names them in
// packages/daemon/internal/runtime/sandbox/manifest.go, so the manifest's entry and the `agents/`
// beside it are the ones under test), the role's own LEGION_* marks, and one stand-in that is the
// model gateway, the daemon's claim routes and the Envoy listener. No row needs NATS: the Envoy
// address is one that refuses every connection (`refusingNats`), which leaves the role claim its
// two listener calls and takes no inbound delivery, so these rows run wherever the binary does.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which CI's
// pi-legion job installs (.github/actions/install-omp). A run without it skips, except on GitHub
// Actions, where a skip would hide the only run of the check on the host that ships it
// (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
//
// Where each of the two lines is printed is Oh My Pi's, not this plugin's: the claim boot's line
// goes to stderr (`console.error` in src/claim-session.ts), and the Envoy entry's refusal goes
// through `logger.error` (`@oh-my-pi/pi-utils`), which writes the rotating file under the run's
// own home and never stdout or stderr, because a line on either would corrupt the TUI. So an
// ending row reads stderr through the rejection of `Rpc.closed`, which names it, and the row for
// the missing plugin reads the run's log file.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = omp === undefined && !onActions;

/** The binary a row runs against; throws on GitHub Actions without one. */
function setup(): string {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  return omp;
}

/** The two plugin roots a pod names, pi-envoy first: the Legion entry refuses to run without the
 * Envoy entry, and the Envoy entry ends a role's agent that loaded no Legion entry. */
const ENVOY_ROOT = path.resolve(import.meta.dir, "../../pi-envoy");
const LEGION_ROOT = path.resolve(import.meta.dir, "..");

/** Who every pane of this file is, as its environment says and its one line reports. */
const ROLE = "tester";
const TREE = "INFRA-2883";
const ISSUE = "INFRA-2883";
/** The claim the stand-in daemon registers, and the boot token of this file's own. */
const CLAIM_TOKEN = "legion-claim-boot-ending-infra-2883-tester";
const BOOT_TOKEN = "claim-boot-ending-boot";

/**
 * Oh My Pi's own handler bound: the 30 000 ms at which the harness aborts an extension handler
 * that has not returned, prints one `extension_error` frame and runs the session on
 * (`OMP_HANDLER_BOUND_MS` in extensions/legion.ts, measured in this pod on 2026-11-11). Every
 * ending below is measured against it from the call that did not answer, which is also k2's
 * "within 30 seconds".
 */
const OMP_HANDLER_BOUND_MS = 30_000;

/** What that abort prints, which no row's output may hold: the boot decides its own ending
 * inside the bound rather than being aborted into a session that runs on with no `legion` tool. */
const HANDLER_ABORT = `handler timed out after ${OMP_HANDLER_BOUND_MS}ms`;

/** The one line every ending of the claim boot writes, as a row finds it on stderr. */
const BOOT_FAILURE = "[legion] claim boot failed: ";

/**
 * How long after the spawn a wait may last: Oh My Pi's startup, which loads both plugin roots
 * before any handler of theirs runs, took about 7 s of wall time in this pod and about 27 s in a
 * slow one (packages/pi-legion/extensions/claim-boot-order-omp.test.ts), so the waits that span
 * it get this much from the spawn.
 */
const STARTUP_DEADLINE_MS = 150_000;

/** How long one wait for something the started session owes a row may last once startup is over. */
const STEP_DEADLINE_MS = 30_000;

/**
 * A row's own timeout: its startup, the claim boot's whole budget — which no environment variable
 * shortens, so each ending row pays it once — and the cleanup of its processes.
 */
const CASE_TIMEOUT_MS = 240_000;

/**
 * How long a control row lets its session run on after the probe's report before it reads
 * whether the process is still there. A real clock, deliberately: the proposition is an absence
 * — this session was not ended — and a process that does nothing emits no signal to await. The
 * report already proves the Envoy entry's refusal did not fire, since that check stands before
 * the notices of the same `session_start` and the probe is loaded after both entries; this wait
 * is the margin for an ending from any later step of the startup.
 */
const CONTROL_LIVE_MS = 2_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/**
 * A third extension, loaded after both plugin roots, which reports the session's active tools the
 * moment Oh My Pi's startup reaches it: the probe k2, k5 and k7 name. It reports from every
 * session of the process, a `task` subagent's own among them, each under its own session id, and
 * it reports at all only if the two entries before it returned — so its report is also how a
 * control row knows the session it watches is alive and past the Envoy entry's refusal.
 */
const TOOLS_PROBE_EXTENSION = `export default function legionToolsProbe(pi) {
  pi.on("session_start", async (_event, context) => {
    await fetch(process.env.LEGION_TEST_PROBE_URL, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        session: context.sessionManager.getSessionId(),
        tools: pi.getActiveTools(),
      }),
    });
  });
}
`;

/** What the probe extension reports; the row reads nothing else of it. */
const ReportSchema = z.object({ session: z.string(), tools: z.array(z.string()) });
type Report = z.infer<typeof ReportSchema>;

/** One Messages request the stand-in holds until the row answers it. */
interface Turn {
  readonly request: Request;
  /** Answers it with `blocks`, so the session takes its next step. */
  reply(blocks: readonly Block[]): void;
}

/** What a row's stand-in does with the claim boot's two daemon calls. */
type Daemon =
  /** Answers `claims/register` and `claims/ready`: the boot settles and the pane runs on. */
  | { readonly kind: "registers" }
  /** Answers `claims/register` and holds every `claims/ready` until the row is over. */
  | { readonly kind: "hangs-ready" }
  /** The daemon is not this stand-in: a closed port, or a socket that answers nothing. */
  | { readonly kind: "elsewhere"; readonly url: string };

/** A row's scratch root, the stand-in it runs against, and what the stand-in has seen. */
interface Standin {
  readonly root: string;
  readonly home: string;
  readonly workspace: string;
  readonly sessions: string;
  readonly state: string;
  /** The stand-in's base URL: the model gateway, the Envoy listener and, where the row's daemon
   * is this stand-in, the daemon too. */
  readonly base: string;
  /** What the pane runs with as LEGION_DAEMON_URL. */
  readonly daemonUrl: string;
  /** The probe extension's file, loaded by the rows that watch a session that keeps running. */
  readonly probe: string;
  /** Every Messages request the gateway is still holding or has answered, in arrival order. */
  readonly turns: Turn[];
  /** Every report the probe extension sent, in arrival order. */
  readonly reports: Report[];
  /** When the pane's Envoy entry first called the listener: its `session_start` had started by
   * then, and the Legion entry's claim boot, which Oh My Pi runs after it, had not. */
  envoyUpAt(): number | undefined;
  /** When this stand-in answered `claims/register`. */
  registeredAt(): number | undefined;
}

/**
 * The scratch root, the stand-in and the profile one row runs against. The stand-in answers the
 * model gateway by holding each Messages request until the row answers it, the daemon's claim
 * routes as `daemon` says, the grant route the `legion` tool's operations mint against, and every
 * Envoy listener call with an interest, as the other real-binary tests' stand-ins do.
 */
async function standin(prefix: string, daemon: Daemon): Promise<Standin> {
  const binary = setup();
  const { root, home, workspace, sessions } = await ompRoot(binary, prefix, cleanup);
  const state = path.join(root, "state");
  await mkdir(state, { recursive: true, mode: 0o700 });
  const probe = path.join(root, "tools-probe.ts");
  await writeFile(probe, TOOLS_PROBE_EXTENSION);
  // The daemon's golden registration answer (packages/daemon/internal/api), so a field the daemon
  // adds to it reaches this stand-in's `claims/register` too.
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  // The `claims/ready` a hung daemon never answers. It is let go at cleanup, before the server is
  // stopped, so the stand-in stops at once rather than on its own force.
  const hungReady = Promise.withResolvers<Response>();
  cleanup.push(async () => hungReady.resolve(new Response(null, { status: 204 })));

  const turns: Turn[] = [];
  const reports: Report[] = [];
  let envoyUpAt = 0;
  let registeredAt = 0;
  let grants = 0;
  const { base } = serveStandin(cleanup, async (url, body) => {
    if (url.pathname === "/test/probe") {
      reports.push(ReportSchema.parse(body));
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/anthropic/v1/messages") {
      const index = turns.length;
      const answer = Promise.withResolvers<string>();
      turns.push({
        request: { path: url.pathname, body },
        reply: (blocks) => answer.resolve(messageStream(blocks, `msg_${index}`)),
      });
      return new Response(await answer.promise, {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      if (registeredAt === 0) registeredAt = Date.now();
      return Response.json({
        ...registered,
        claimToken: CLAIM_TOKEN,
        tree: TREE,
        issue: ISSUE,
        role: ROLE,
        generation: 1,
        secret: "claim-boot-ending-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") {
      if (daemon.kind === "hangs-ready") return await hungReady.promise;
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `claim-boot-ending-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    // An unavailable Dispatch arms no run-end self-check, so every Messages request is a turn.
    if (url.pathname.startsWith("/api/")) return new Response("unavailable", { status: 503 });
    // The Envoy listener: registration, the role claim, and any other read answer with an
    // interest. Its first call is the mark that the pane's startup has reached its extensions.
    if (envoyUpAt === 0) envoyUpAt = Date.now();
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "claim-boot-ending-machine",
      dir: workspace,
      topics: [],
    });
  });
  // A request still held at the end is answered, so the server stops at once.
  cleanup.push(async () => {
    for (const turn of turns) turn.reply([{ type: "text", text: "Row over." }]);
  });
  // `deep` is the model role agents/deep-worker.md asks for, which the subagent row's `task` call
  // needs and no other profile of the harness carries.
  await writeStandinProfile(home, base, ["  deep: standin/standin-model"]);

  return {
    root,
    home,
    workspace,
    sessions,
    state,
    base,
    daemonUrl: daemon.kind === "elsewhere" ? daemon.url : base,
    probe,
    turns,
    reports,
    envoyUpAt: () => (envoyUpAt === 0 ? undefined : envoyUpAt),
    registeredAt: () => (registeredAt === 0 ? undefined : registeredAt),
  };
}

/**
 * What every pane of this file runs with, whatever its role: the stand-in as its Envoy listener,
 * a NATS address that refuses every connection, the probe extension's route, and the file
 * session store — never this pod's own, whose variables no pane inherits, since the harness
 * hands Oh My Pi the environment it is given and nothing else.
 */
function hostEnvironment(served: Standin): Record<string, string> {
  return {
    ENVOY_URL: served.base,
    ENVOY_NATS_URL: refusingNats(cleanup),
    OMP_SESSION_STORAGE: "file",
    LEGION_TEST_PROBE_URL: `${served.base}/test/probe`,
  };
}

/** The environment of a pane the daemon launched for a role: its LEGION_* marks against the
 * row's daemon, and a boot token of this file's own. */
function roleEnvironment(served: Standin): Record<string, string> {
  return {
    LEGION_DAEMON_URL: served.daemonUrl,
    LEGION_ROLE: ROLE,
    LEGION_TREE: TREE,
    LEGION_ISSUE: ISSUE,
    LEGION_GENERATION: "1",
    LEGION_BOOT_TOKEN: BOOT_TOKEN,
    LEGION_STATE_DIR: served.state,
    LEGION_WORKSPACE: served.workspace,
    ...hostEnvironment(served),
  };
}

/** The environment of the controller an operator starts in a terminal: the controller's marks,
 * no boot token, and none of a phase worker's issue. */
function controllerEnvironment(served: Standin): Record<string, string> {
  return {
    LEGION_CONTROLLER: "1",
    LEGION_ROLE: "controller",
    LEGION_DAEMON_URL: served.daemonUrl,
    LEGION_STATE_DIR: served.state,
    ...hostEnvironment(served),
  };
}

/** A copy of the pi-legion package directory whose entry is one throwing statement: the root a
 * pod comes back with when its plugin install left a broken copy behind. Its dependencies are
 * left out of the copy, because the entry throws before it imports anything. */
async function throwingLegionRoot(served: Standin): Promise<string> {
  const root = path.join(served.root, "pi-legion");
  await cp(LEGION_ROOT, root, {
    recursive: true,
    filter: (source) => path.basename(source) !== "node_modules",
  });
  await writeFile(
    path.join(root, "extensions", "legion.ts"),
    'throw new Error("this pi-legion root throws at load");\n'
  );
  return root;
}

/** Starts one pane as the daemon launches a role's agent: `omp --mode rpc` with `extensions`
 * loaded, in the row's scratch root. Its frames are kept for the rows that read them. */
function startPane(
  served: Standin,
  options: {
    readonly extensions: readonly string[];
    readonly env: Record<string, string>;
  }
): { readonly rpc: Rpc; frames(): string } {
  const frames: object[] = [];
  const rpc = spawnRpc(
    setup(),
    {
      extensions: options.extensions,
      home: served.home,
      workspace: served.workspace,
      sessions: served.sessions,
      env: options.env,
      onFrame: (frame) => frames.push(frame),
    },
    cleanup
  );
  // Every ending row settles on the exit itself, so nothing of this pane is awaited as a step.
  rpc.closed.catch(() => undefined);
  return { rpc, frames: () => JSON.stringify(frames) };
}

/** What an ended pane left: the status it exited with and its stderr, which the harness names in
 * the rejection of `Rpc.closed` and a case can read nowhere else. */
async function ended(rpc: Rpc): Promise<{ readonly status: number; readonly stderr: string }> {
  const stderr = await rpc.closed.then(
    () => "",
    (error: unknown) => String(error)
  );
  return { status: await rpc.exited, stderr };
}

/** The claim boot's one line, out of whatever else the pane wrote to stderr. */
function bootFailureLine(stderr: string): string {
  const line = stderr.split("\n").find((candidate) => candidate.startsWith(BOOT_FAILURE));
  if (line === undefined) throw new Error(`no claim boot failure line on stderr:\n${stderr}`);
  return line;
}

/** Every line Oh My Pi's own logger wrote under a run's home, which is where the Envoy entry's
 * refusal goes and the only place it goes. */
async function ompLog(home: string): Promise<string> {
  const logs = path.join(home, ".omp", "logs");
  let text = "";
  for (const file of await readdir(logs)) text += await readFile(path.join(logs, file), "utf8");
  return text;
}

/** Polls `probe` until it has a value, failing at the deadline: what the rows wait on — a request
 * reaching the stand-in, a process exiting — has no event of its own to await. */
async function until<T>(what: string, spawnedAt: number, probe: () => T | undefined): Promise<T> {
  // The first wait of a row spans Oh My Pi's startup, which no row can shorten.
  const deadline = Math.max(Date.now() + STEP_DEADLINE_MS, spawnedAt + STARTUP_DEADLINE_MS);
  for (;;) {
    const value = probe();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await Bun.sleep(25);
  }
}

/** A Messages request's conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** The names of the tools a Messages request offered the model. */
const ToolsSchema = z.array(z.object({ name: z.string() }));
function toolsOf(request: Request): string[] {
  return ToolsSchema.parse(request.body.tools ?? []).map((tool) => tool.name);
}

// k2: the daemon's address refuses every connection, which is a pod that came back while its
// daemon was gone. The boot retries until its budget is spent and then ends the process on one
// line, rather than leaving the role to run its phase with no `legion` tool.
test.skipIf(skip)(
  "a role whose daemon refuses every connection ends its process on the claim boot's line",
  async () => {
    // A port taken and given back at once: nothing listens on it for the row's lifetime.
    const closed = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
    const port = closed.port;
    closed.stop(true);
    const served = await standin("claim-boot-ending-closed-", {
      kind: "elsewhere",
      url: `http://127.0.0.1:${port}`,
    });

    const spawnedAt = Date.now();
    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, LEGION_ROOT],
      env: roleEnvironment(served),
    });
    const { status, stderr } = await ended(pane.rpc);
    const endedAt = Date.now();
    const envoyUpAt = await until("the pane's first Envoy listener call", spawnedAt, () =>
      served.envoyUpAt()
    );

    expect(status).toBeGreaterThan(0);
    const line = bootFailureLine(stderr);
    expect(line).toStartWith(
      `${BOOT_FAILURE}role ${ROLE}, issue ${ISSUE}, tree ${TREE}, ` +
        `daemon ${served.daemonUrl}; claims/register failed: `
    );
    expect(line).toInclude(`budget of ${CLAIM_BOOT_BUDGET_MS} ms, which is spent`);
    expect(line).toInclude("ECONNREFUSED");
    expect(line).toEndWith("no claim was registered; exiting so the daemon launches it again");
    // k2's "within 30 seconds", read from the moment the pane's extensions started running.
    expect(endedAt - envoyUpAt).toBeLessThan(OMP_HANDLER_BOUND_MS);
    expect(`${pane.frames()}${stderr}`).not.toInclude(HANDLER_ABORT);
  },
  CASE_TIMEOUT_MS
);

// k3, first run: a daemon that accepts the connection and sends nothing. Every attempt is bounded
// and the budget ends the boot, so the ending is the boot's own and comes before Oh My Pi's
// handler bound, where the harness would abort the handler and run the session on without a tool.
test.skipIf(skip)(
  "a daemon that accepts a connection and answers nothing ends the role's process before Oh My Pi's handler bound",
  async () => {
    let connectedAt = 0;
    const silent = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open() {
          if (connectedAt === 0) connectedAt = Date.now();
        },
        data() {},
      },
    });
    cleanup.push(async () => silent.stop(true));
    const served = await standin("claim-boot-ending-silent-", {
      kind: "elsewhere",
      url: `http://127.0.0.1:${silent.port}`,
    });

    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, LEGION_ROOT],
      env: roleEnvironment(served),
    });
    const { status, stderr } = await ended(pane.rpc);
    const endedAt = Date.now();

    expect(status).toBeGreaterThan(0);
    expect(connectedAt).toBeGreaterThan(0);
    const line = bootFailureLine(stderr);
    expect(line).toStartWith(
      `${BOOT_FAILURE}role ${ROLE}, issue ${ISSUE}, tree ${TREE}, ` +
        `daemon ${served.daemonUrl}; claims/register failed: `
    );
    expect(line).toInclude(`budget of ${CLAIM_BOOT_BUDGET_MS} ms, which is spent`);
    expect(line).toEndWith("no claim was registered; exiting so the daemon launches it again");
    expect(endedAt - connectedAt).toBeLessThan(OMP_HANDLER_BOUND_MS);
    expect(`${pane.frames()}${stderr}`).not.toInclude(HANDLER_ABORT);
  },
  CASE_TIMEOUT_MS
);

// k3, second run: the registration answers and `claims/ready` never does. The claim exists, so
// the line says so, and the ending is the same one: a registered claim whose session holds no
// tool is what the daemon must launch again.
test.skipIf(skip)(
  "a daemon that registers and never answers ready ends the role's process before Oh My Pi's handler bound",
  async () => {
    const served = await standin("claim-boot-ending-ready-", { kind: "hangs-ready" });

    const spawnedAt = Date.now();
    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, LEGION_ROOT],
      env: roleEnvironment(served),
    });
    const { status, stderr } = await ended(pane.rpc);
    const endedAt = Date.now();
    const registeredAt = await until("the stand-in to answer claims/register", spawnedAt, () =>
      served.registeredAt()
    );

    expect(status).toBeGreaterThan(0);
    const line = bootFailureLine(stderr);
    expect(line).toStartWith(
      `${BOOT_FAILURE}role ${ROLE}, issue ${ISSUE}, tree ${TREE}, ` +
        `daemon ${served.daemonUrl}; claims/ready failed: `
    );
    expect(line).toInclude(`budget of ${CLAIM_BOOT_BUDGET_MS} ms, which is spent`);
    expect(line).toEndWith(
      `the claim ${CLAIM_TOKEN} was registered; exiting so the daemon launches it again`
    );
    expect(endedAt - registeredAt).toBeLessThan(OMP_HANDLER_BOUND_MS);
    expect(`${pane.frames()}${stderr}`).not.toInclude(HANDLER_ABORT);
  },
  CASE_TIMEOUT_MS
);

// k5, first run: the pi-legion root threw at load, which Oh My Pi reports and runs on from. The
// Envoy entry reads the three marks of a role's agent and ends the process itself, so the role
// does not run its phase with every `envoy_*` tool and no `legion` tool.
test.skipIf(skip)(
  "an agent the daemon launched whose pi-legion root threw at load ends on the Envoy entry's line",
  async () => {
    const served = await standin("claim-boot-ending-missing-", { kind: "registers" });
    const broken = await throwingLegionRoot(served);

    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, broken],
      env: roleEnvironment(served),
    });
    const { status, stderr } = await ended(pane.rpc);

    expect(status).toBeGreaterThan(0);
    expect(stderr).toInclude(`Failed to load extension ${path.join(broken, "extensions")}`);
    // The refusal names the daemon it would have registered with, and reaches both logs: Oh My
    // Pi's file under the role's home and stderr, which is the pod's log (R1, R2).
    const refusal =
      `is running in an agent the Legion daemon launched (LEGION_ROLE=${ROLE}, ` +
      `LEGION_ISSUE=${ISSUE}, LEGION_TREE=${TREE}, LEGION_DAEMON_URL=${served.daemonUrl}) where ` +
      "@sjawhar/pi-legion did not load, so this session has no legion tool and claims no role; " +
      "ending the process so the daemon launches it again";
    expect(await ompLog(served.home)).toInclude(refusal);
    expect(stderr).toInclude(refusal);
  },
  CASE_TIMEOUT_MS
);

// k5, second run: the same broken root with no boot token, which is the shape the daemon's image
// probe runs. It is no agent of a role, so nothing ends it: the session starts, the probe reports
// its tools — `legion` not among them, since the entry never loaded — and it is still running
// when the row ends it.
test.skipIf(skip)(
  "a session with no boot token keeps running where the pi-legion root threw at load",
  async () => {
    const served = await standin("claim-boot-ending-probe-", { kind: "registers" });
    const broken = await throwingLegionRoot(served);
    const { LEGION_BOOT_TOKEN: _token, ...withoutToken } = roleEnvironment(served);

    const spawnedAt = Date.now();
    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, broken, served.probe],
      env: withoutToken,
    });
    let exitCode: number | undefined;
    void pane.rpc.exited.then((code) => {
      exitCode = code;
    });

    const report = await until("the probe extension's report", spawnedAt, () => served.reports[0]);
    expect(report.tools).not.toContain("legion");
    expect(report.tools).toContain("envoy_send");
    await Bun.sleep(CONTROL_LIVE_MS);
    expect(exitCode).toBeUndefined();
  },
  CASE_TIMEOUT_MS
);

// k5, third and fifth runs: the two shapes a person starts, which have no `--mode rpc` on their
// command line — the operator's controller, and a person's own Oh My Pi inside a role's pod,
// where the role's whole environment is inherited. Neither is an agent the daemon launched, so
// neither is ended, however broken the pi-legion root it loaded is. They run under a terminal, as
// a person runs them (`spawnInteractive`).
for (const shape of ["the operator's controller", "a person's own session"] as const) {
  test.skipIf(skip)(
    `${shape} keeps running where the pi-legion root threw at load`,
    async () => {
      const served = await standin("claim-boot-ending-person-", { kind: "registers" });
      const broken = await throwingLegionRoot(served);
      const environment =
        shape === "the operator's controller"
          ? controllerEnvironment(served)
          : roleEnvironment(served);

      const spawnedAt = Date.now();
      const terminal = spawnInteractive(
        setup(),
        {
          extensions: [ENVOY_ROOT, broken, served.probe],
          home: served.home,
          workspace: served.workspace,
          sessions: served.sessions,
          env: environment,
          args: [],
        },
        cleanup
      );
      let exitCode: number | undefined;
      void terminal.exited.then((code) => {
        exitCode = code;
      });

      const report = await until(
        "the probe extension's report",
        spawnedAt,
        () => served.reports[0]
      );
      expect(report.tools).not.toContain("legion");
      await Bun.sleep(CONTROL_LIVE_MS);
      expect(exitCode, terminal.screen()).toBeUndefined();
    },
    CASE_TIMEOUT_MS
  );
}

// k5, sixth run: a `task` subagent in a healthy pane. The subagent runs the same two entries in
// the same process, under a session of its own, and neither ends it: the pane's own turn after
// the `task` call still offers the model the `legion` tool, and the pane is still running when
// the row ends it.
test.skipIf(skip)(
  "a task subagent leaves the pane running with its legion tool",
  async () => {
    const served = await standin("claim-boot-ending-subagent-", { kind: "registers" });
    const assignment = "Dispatch the worker. CLAIM-BOOT-ENDING-ASSIGNMENT.";

    const spawnedAt = Date.now();
    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, LEGION_ROOT, served.probe],
      env: roleEnvironment(served),
    });
    let exitCode: number | undefined;
    void pane.rpc.exited.then((code) => {
      exitCode = code;
    });
    const paneReport = await until(
      "the pane's own probe report",
      spawnedAt,
      () => served.reports[0]
    );
    expect(paneReport.tools).toContain("legion");
    pane.rpc.send({ type: "prompt", message: assignment });

    const isPaneTurn = (turn: Turn): boolean => conversation(turn.request).includes(assignment);
    const dispatched = await until("the pane's first turn", spawnedAt, () =>
      served.turns.find(isPaneTurn)
    );
    dispatched.reply([
      call("task", {
        context: "The row's subagent.",
        tasks: [
          {
            name: "Subagent",
            agent: "deep-worker",
            task: "Report what the workspace holds.",
            solutionSpace: "one report",
          },
        ],
      }),
    ]);

    // The subagent's own session, reported by the probe extension from inside it: a second
    // session of this process, with the agent's own tools and no `legion` among them.
    const subagent = await until("the subagent's own probe report", spawnedAt, () =>
      served.reports.find((report) => report.session !== paneReport.session)
    );
    expect(subagent.tools).not.toContain("legion");

    // The pane itself ran on: the turn its `task` call's result started is the pane's own, and
    // the model is still offered the `legion` tool in it.
    const after = await until("the pane's turn after the task call", spawnedAt, () =>
      served.turns.find(
        (turn) => isPaneTurn(turn) && conversation(turn.request).includes("Spawned")
      )
    );
    expect(toolsOf(after.request)).toContain("legion");
    after.reply([{ type: "text", text: "The worker is running." }]);
    expect(exitCode).toBeUndefined();
  },
  CASE_TIMEOUT_MS
);

// k7, the control for every ending above: a daemon that answers both calls. The session starts,
// keeps the `legion` tool the Legion entry registered and activated, and is still running when
// the row ends it. Without this row, a change that ended every session would pass all of them.
test.skipIf(skip)(
  "a role whose daemon answers keeps running and holds its legion tool",
  async () => {
    const served = await standin("claim-boot-ending-healthy-", { kind: "registers" });

    const spawnedAt = Date.now();
    const pane = startPane(served, {
      extensions: [ENVOY_ROOT, LEGION_ROOT, served.probe],
      env: roleEnvironment(served),
    });
    let exitCode: number | undefined;
    void pane.rpc.exited.then((code) => {
      exitCode = code;
    });

    const report = await until("the probe extension's report", spawnedAt, () => served.reports[0]);
    expect(report.tools).toContain("legion");
    expect(served.registeredAt()).toBeGreaterThan(0);
    await Bun.sleep(CONTROL_LIVE_MS);
    expect(exitCode).toBeUndefined();
  },
  CASE_TIMEOUT_MS
);
