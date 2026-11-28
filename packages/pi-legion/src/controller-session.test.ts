import { afterEach, expect, jest, mock, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { githubDefaultBranchWorkflowSubject, slackMentionTopic } from "@legion/contracts";
import type { LegionControllerRegistration } from "@legion/contracts/legion-api";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { resetEnvoyPluginInterfaceForTests } from "@legion/pi-shared/interface";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { legionRoleClaimBridge, RoleNotHeldError } from "@legion/pi-shared/role-claim-bridge";
import { RoleClaimStoppingError } from "@legion/pi-shared/role-claim-stopping";
import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { turnsUntil } from "@legion/test-fixtures/fake-clock";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { settled as settledWithin, until } from "@legion/test-fixtures/wait";
import { logger } from "@oh-my-pi/pi-utils";
import type { ControllerSession } from "./controller-session";
import { HOST_STOP_GRACE_MS } from "./host-stop";

// The module reaches the Oh My Pi host through `@legion/pi-shared/subagent-session`, which only
// the binary resolves; the import below must come after the mock.
mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));

const { createControllerSession } = await import("./controller-session");

restoreEnvironment([
  "LEGION_CONTROLLER",
  "LEGION_TREE",
  "LEGION_BOOT_TOKEN",
  "LEGION_BOOT_TOKEN_FILE",
  "LEGION_PROJECT",
  "LEGION_SLACK_CHANNELS",
  "LEGION_DAEMON_URL",
  "LEGION_CONTROLLER_START_MESSAGE",
  "LEGION_CONTROLLER_SECRET",
  "LEGION_CONTROLLER_SECRET_FILE",
]);
afterEach(() => {
  // A fake-clock row that its guard ended leaves no fake clock to the rows after it.
  jest.useRealTimers();
  resetEnvoyPluginInterfaceForTests();
});

const CONTROLLER_SESSION = "ses_controller_stopping";
const TRANSCRIPT = "/sessions/-legion-default/2026-08-30T03-47-40-329Z_controller.jsonl";

/** The daemon's own answers (`packages/daemon/internal/api`'s golden test): its project is
 * `legion`, and it registers this controller as `legion-legion-controller`. */
function daemonFixture(name: string): string {
  return readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name),
    "utf8"
  );
}

/** The listener's 503 to a role claim, read by name and status (`listenerAskedToClaimAgain`), as
 * pi-envoy's own copy of envoy-client throws it behind the bridge. */
class ListenerAskedToClaimAgain extends Error {
  override readonly name = "EnvoyApiError";
  readonly details = { status: 503 };
}

/** What Envoy's one bound instance does with each role claim the controller asks for, in order. */
type EnvoyStep = () => Promise<void>;

/** One message the controller's process handed its session (`pi.sendMessage`), with its options. */
interface Told {
  readonly message: { readonly customType?: string; readonly content?: unknown };
  readonly options: unknown;
}

/** What `daemonLaunchedController`'s daemon and Envoy answer, each optional. */
interface DaemonAnswers {
  readonly claims?: readonly EnvoyStep[];
  readonly subscribe?: (topic: string) => Promise<void>;
  readonly slackChannels?: string;
  readonly ready?: () => Response;
  readonly register?: () => Response | Promise<Response>;
  readonly hostStop?: () => Promise<void> | undefined;
  readonly steps?: string[];
  readonly tell?: (message: Told["message"]) => void;
}

/**
 * The controller the daemon launched (`LEGION_CONTROLLER=1` and a boot token, and
 * `answers.slackChannels` as its LEGION_SLACK_CHANNELS, none by default), against a daemon that
 * answers its health, its registration and its ready, the last with `answers.ready` (a 204 by
 * default). Envoy's one bound instance answers each role claim with the next of `claims` and each
 * subscription with `subscribe` of its topic (both default to landing), recording each. The host's
 * stop is `answers.hostStop`; by default none has begun. Every exit is recorded, in `exits` and as
 * `exit <code>` on `answers.steps`, and `exited` settles at the first. The session takes each
 * message the process hands it unless `answers.tell` throws for it, which refuses it as
 * `pi.sendMessage` throws. Every line the Oh My Pi log takes, every message the session took
 * (`told`), and every daemon route, role claim and subscription in the order they were asked
 * (`order`) is recorded too.
 */
function daemonLaunchedController(answers: DaemonAnswers) {
  process.env.LEGION_CONTROLLER = "1";
  delete process.env.LEGION_TREE;
  delete process.env.LEGION_BOOT_TOKEN_FILE;
  delete process.env.LEGION_CONTROLLER_START_MESSAGE;
  process.env.LEGION_BOOT_TOKEN = fixtureSecret("boot");
  process.env.LEGION_PROJECT = "legion";
  process.env.LEGION_SLACK_CHANNELS = answers.slackChannels ?? "";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  const posted: string[] = [];
  const order: string[] = [];
  const daemon = createLegionDaemonClient("http://daemon.test", {
    pluginContract: 60,
    fetch: (async (input) => {
      const route = new URL(input.toString()).pathname;
      posted.push(route);
      order.push(`post ${route}`);
      if (route === "/healthz") {
        return new Response(daemonFixture("health.json"), {
          headers: { "content-type": "application/json" },
        });
      }
      if (route === "/legion/v1/claims/register") {
        if (answers.register !== undefined) return answers.register();
        return new Response(daemonFixture("register-controller.json"), {
          headers: { "content-type": "application/json" },
        });
      }
      if (route === "/legion/v1/claims/ready" && answers.ready !== undefined)
        return answers.ready();
      return new Response(null, { status: 204 });
    }) as typeof fetch,
  });
  const claims: string[] = [];
  const subscriptions: string[] = [];
  legionRoleClaimBridge().instances.push({
    claim: async (_sessionID, role) => {
      claims.push(role);
      order.push(`claim ${role}`);
      await (answers.claims?.[claims.length - 1] ?? (async () => undefined))();
    },
    subscribe: async (_sessionID, topic) => {
      subscriptions.push(topic);
      order.push(`subscribe ${topic}`);
      await answers.subscribe?.(topic);
    },
    sessionID: () => CONTROLLER_SESSION,
  });
  const exits: number[] = [];
  const exited = Promise.withResolvers<void>();
  const told: Told[] = [];
  const controller = createControllerSession({
    daemon: () => daemon,
    persistedTranscript: async () => ({ sessionFile: TRANSCRIPT, agentId: CONTROLLER_SESSION }),
    pi: {
      sendMessage: (message: Told["message"], options: unknown) => {
        answers.tell?.(message);
        told.push({ message, options });
      },
    } as unknown as PiApi,
    exitProcess: (code) => {
      exits.push(code);
      answers.steps?.push(`exit ${code}`);
      exited.resolve();
      return undefined as never;
    },
    hostStop: answers.hostStop ?? (() => undefined),
  });
  const context = {
    sessionManager: { getSessionId: () => CONTROLLER_SESSION },
    ui: { notify: () => undefined, setWidget: () => undefined },
  } as unknown as SessionContext;
  const lines: { readonly level: string; readonly message: string }[] = [];
  const stopSink = logger.registerLogSink((entry) => {
    lines.push({ level: entry.level, message: entry.message });
  });
  return {
    controller,
    context,
    claims,
    subscriptions,
    exits,
    exited: exited.promise,
    lines,
    posted,
    order,
    told,
    stopSink,
  };
}

/** Lets every callback already queued run. */
function settled(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

const answered503: EnvoyStep = async () => {
  throw new ListenerAskedToClaimAgain("envoy: the role's claim met a conflict at every try");
};
const refusedForStop: EnvoyStep = async () => {
  throw new RoleClaimStoppingError("legion-legion-controller");
};

// What the stop rows below hold the controller to: any other refusal of its claim still exits Oh
// My Pi, so the daemon launches it again, and says why in the log.
test("a daemon-launched controller whose role claim Envoy refuses for another reason exits, saying why", async () => {
  const run = daemonLaunchedController({
    claims: [
      async () => {
        throw new Error("Envoy role legion-legion-controller is held by live session ses_other");
      },
    ],
  });
  try {
    await run.controller.handleSessionStart(run.context);

    expect(run.exits).toEqual([1]);
    expect(run.lines.filter(({ level }) => level === "error")).toHaveLength(1);
  } finally {
    run.stopSink();
  }
});

// The daemon-launched controller runs in RPC mode, where a SIGTERM stops Envoy's session with no
// `session_shutdown` (R24 of dispatch://INFRA-3589). Its role claim is answered 503, and the stop
// begins while it waits to claim again: the next claim is refused naming the stop
// (`RoleClaimStoppingError`). The controller exits nothing and writes no line from then on, so the
// stop's role release and DELETE run to their end.
test("a daemon-launched controller whose claim, answered 503, is refused naming the stop exits nothing and writes no line", async () => {
  const run = daemonLaunchedController({ claims: [answered503, refusedForStop] });
  try {
    await run.controller.handleSessionStart(run.context);
    await settled();

    expect(run.claims).toEqual(["legion-legion-controller", "legion-legion-controller"]);
    expect(run.exits).toEqual([]);
    // The one line is the wait after the 503, written before the stop began.
    expect(run.lines.map(({ message }) => message)).toEqual([
      expect.stringContaining("was answered 503 at attempt 1"),
    ]);
    // The row "… ready the daemon refuses with no stop begun exits at once" shows it posted.
    expect(run.posted).not.toContain("/legion/v1/claims/ready");
  } finally {
    run.stopSink();
  }
});

// The same wait, and the session's `session_shutdown` stops the controller session
// (`ControllerSession.stop`): the wait ends at once, no claim is made after it, and nothing exits
// or is written.
test("a daemon-launched controller whose session stops while its claim waits after a 503 claims no more, exits nothing and writes no line", async () => {
  const run = daemonLaunchedController({ claims: [answered503] });
  try {
    const claiming = run.controller.handleSessionStart(run.context);
    while (run.lines.length === 0) await settled();
    run.controller.stop();
    await claiming;
    await settled();

    expect(run.claims).toEqual(["legion-legion-controller"]);
    expect(run.exits).toEqual([]);
    expect(run.lines.map(({ message }) => message)).toEqual([
      expect.stringContaining("was answered 503 at attempt 1"),
    ]);
  } finally {
    run.stopSink();
  }
});

// The role claim landed, and the stop begins before the controller topic's subscription: Envoy
// refuses that subscription naming the stop. The controller exits nothing, writes no line, and
// reports no ready.
test("a daemon-launched controller whose controller-topic subscription is refused naming the stop exits nothing, writes no line and reports no ready", async () => {
  const run = daemonLaunchedController({
    subscribe: (topic) =>
      topic === "notifications.legion.legion.controller" ? refusedForStop() : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await settled();

    expect(run.claims).toEqual(["legion-legion-controller"]);
    expect(run.subscriptions).toEqual(["notifications.legion.legion.controller"]);
    expect(run.exits).toEqual([]);
    expect(run.lines).toEqual([]);
    // The row "… ready the daemon refuses with no stop begun exits at once" shows it posted.
    expect(run.posted).not.toContain("/legion/v1/claims/ready");
  } finally {
    run.stopSink();
  }
});

// The tester's red test (R24 of dispatch://INFRA-3589, round 11): the session's stop begins while
// the controller's `claims/register` is on the wire, and the daemon then refuses the registration.
// A claim the stop overtook says nothing and exits nothing (`extensions/legion.ts`'s
// `session_shutdown`), so the refusal writes no line either. The control row below is the same
// refusal with no stop begun: it is logged and exits, so this row's absence can fail.
const registerRefused = (): Response =>
  Response.json({ error: "this boot token names no controller" }, { status: 409 });

test("a daemon-launched controller whose session stops while claims/register is on the wire, and whose registration the daemon then refuses, exits nothing and writes no line", async () => {
  const asked = Promise.withResolvers<void>();
  const held = Promise.withResolvers<Response>();
  const run = daemonLaunchedController({
    register: () => {
      asked.resolve();
      return held.promise;
    },
  });
  try {
    const claiming = run.controller.handleSessionStart(run.context);
    await asked.promise;
    run.controller.stop();
    held.resolve(registerRefused());
    await claiming;
    await settled();

    expect(run.exits).toEqual([]);
    expect(run.lines).toEqual([]);
  } finally {
    run.stopSink();
  }
});

test("a daemon-launched controller whose registration the daemon refuses with no stop begun logs it and exits", async () => {
  const run = daemonLaunchedController({ register: registerRefused });
  try {
    await run.controller.handleSessionStart(run.context);
    await settled();

    expect(run.exits).toEqual([1]);
    expect(run.lines.map(({ message }) => message)).toContainEqual(
      expect.stringContaining("claims/register for the controller failed (409)")
    );
  } finally {
    run.stopSink();
  }
});

/** The daemon refuses the controller's ready: its registration deadline retired the controller's
 * process and stopped it (`registrationDeadline`, `packages/daemon/internal/supervise/table.go`).
 * A 4xx is a refusal that no retry changes. */
const readyRefused = (): Response =>
  Response.json({ error: "this claim's process was retired" }, { status: 409 });

/** The host's stop, begun before the daemon's refusal comes: pi-envoy's role release, and then,
 * when the row calls `end`, its DELETE, which ends the stop. Each step is recorded on `steps`.
 * `asked` settles when the controller's exit asks for the stop, which is when its wait begins. */
function hostStopRunning(steps: string[]) {
  steps.push("release");
  const ended = Promise.withResolvers<void>();
  const asked = Promise.withResolvers<void>();
  return {
    hostStop: () => {
      asked.resolve();
      return ended.promise;
    },
    asked: asked.promise,
    end: () => {
      steps.push("delete");
      ended.resolve();
    },
  };
}

// R16 of dispatch://INFRA-3589: the daemon's registration deadline retired the daemon-launched
// controller and stopped its process. In RPC mode the SIGTERM runs pi-envoy's stop (its role
// release, then its DELETE) and emits no `session_shutdown`, so the controller has not heard of the
// stop when the daemon refuses its `claims/ready`. The exit that the refusal makes waits for the
// host's stop to end, at most its bound, and keeps its status. With no stop begun, it exits at
// once.
test("a daemon-launched controller whose ready the daemon refuses while the host's stop runs exits only after the stop's DELETE, with status 1", async () => {
  const steps: string[] = [];
  const stop = hostStopRunning(steps);
  const run = daemonLaunchedController({ ready: readyRefused, hostStop: stop.hostStop, steps });
  try {
    const claiming = run.controller.handleSessionStart(run.context);
    await Promise.race([stop.asked, run.exited]);
    stop.end();
    await claiming;
    await run.exited;

    expect(steps).toEqual(["release", "delete", "exit 1"]);
    expect(run.lines.filter(({ level }) => level === "error")).toHaveLength(1);
  } finally {
    run.stopSink();
  }
});

// The control of the `claims/ready` absence in the stop rows above: a controller whose claim and
// subscription land, with no stop begun, posts its ready.
test("a daemon-launched controller whose ready the daemon refuses with no stop begun exits at once", async () => {
  const steps: string[] = [];
  const run = daemonLaunchedController({ ready: readyRefused, steps });
  try {
    await run.controller.handleSessionStart(run.context);

    expect(run.posted).toContain("/legion/v1/claims/ready");
    expect(steps).toEqual(["exit 1"]);
    expect(run.lines.filter(({ level }) => level === "error")).toHaveLength(1);
  } finally {
    run.stopSink();
  }
});

test("a daemon-launched controller whose ready the daemon refuses while the host's stop runs exits at the bound where the stop does not end", async () => {
  const steps: string[] = [];
  const stop = hostStopRunning(steps);
  const run = daemonLaunchedController({ ready: readyRefused, hostStop: stop.hostStop, steps });
  jest.useFakeTimers();
  try {
    const claiming = run.controller.handleSessionStart(run.context);
    await Promise.race([stop.asked, run.exited]);
    jest.advanceTimersByTime(HOST_STOP_GRACE_MS - 1);
    await Promise.resolve();
    expect(steps).toEqual(["release"]);
    jest.advanceTimersByTime(1);
    await claiming;

    expect(steps).toEqual(["release", "exit 1"]);
  } finally {
    jest.useRealTimers();
    run.stopSink();
  }
});

// The project's watched workflows, which the daemon names in the controller's registration
// (`watchedWorkflows`, contract entry 20261127193422). The controller follows one default-branch
// topic for each event of each entry, the completed runs of that workflow on its repository's
// default branch, for as long as it holds its role. It subscribes them once the rest of its claim
// is done (the daemon's controller after its ready), in a task of its own: the claim does not wait
// for them, and nothing Envoy answers about one of them reaches the claim, so no such answer exits
// the process, costs the role, the notice topic or a Slack topic, or keeps the ready from the
// daemon, whose registration deadline would retire a controller that never reported ready. A 503
// (an edge listener that cannot read the project's repositories answers `repositories_unknown`)
// is waited out as a role claim's is, the wait doubling from 1 s to 30 s, and the session is told
// once that the topic waits and once that it landed; any other refusal gives that topic up and
// tells the session so. The session is told as Envoy tells it its own notices, as a message that
// starts no turn; a notice the session refuses is logged, and reaches the claim no more than an
// answer of Envoy's does.

/** One entry of the registration's `watchedWorkflows`. */
type WatchedWorkflow = NonNullable<LegionControllerRegistration["watchedWorkflows"]>[number];

/** The entry register-controller.json names: one workflow, two events. */
const NIGHTLY: WatchedWorkflow = {
  owner: "acme",
  repo: "widgets",
  workflow: "nightly.yaml",
  events: ["schedule", "workflow_dispatch"],
};
const RELEASE: WatchedWorkflow = {
  owner: "acme",
  repo: "gadgets",
  workflow: "release.yml",
  events: ["push"],
};
const watchedTopic = (entry: WatchedWorkflow, event: string): string =>
  githubDefaultBranchWorkflowSubject(entry.owner, entry.repo, entry.workflow, "completed", event);
const NIGHTLY_SCHEDULE = watchedTopic(NIGHTLY, "schedule");
const NIGHTLY_DISPATCH = watchedTopic(NIGHTLY, "workflow_dispatch");
const RELEASE_PUSH = watchedTopic(RELEASE, "push");
/** The project's notice topic, which the controller follows right after its role claim. */
const NOTICE_TOPIC = "notifications.legion.legion.controller";
/** A Slack channel the daemon names the controller it launches, and the topic it follows for it. */
const SLACK_CHANNEL = "C00000000";
const SLACK_TOPIC = slackMentionTopic(SLACK_CHANNEL);
/** The `customType` of what the controller's process tells its session of its watched workflows. */
const WATCHED_NOTICE = "legion-watched-workflows";
/** The delivery Envoy's own notices take (`handNotice` in packages/pi-envoy/extensions/envoy.ts):
 * shown in the transcript, and no turn of its own. */
const NO_TURN = { deliverAs: "steer", triggerTurn: false };

/** The daemon's registration of the controller (register-controller.json) naming `watched` as the
 * project's watched workflows. */
function registeredWith(watched: readonly WatchedWorkflow[]): () => Response {
  const registration: unknown = JSON.parse(daemonFixture("register-controller.json"));
  return () => Response.json({ ...(registration as object), watchedWorkflows: watched });
}

/** The listener's 503 to a registration while an edge listener cannot read the project's
 * repositories, as envoy-client's registration rejects with it. */
const WITHHELD = "the project's repositories could not be read (reason: repositories_unknown)";
const withheld = (): Error => new ListenerAskedToClaimAgain(WITHHELD);

/** The real time a fake-clock row's `turnsUntil` waits before it names a hang: inside a plain
 * `bun test` row's 5000 ms guard, so the wait fails by what it waited for, not at the guard. */
const ROW_TURNS = { boundMs: 3_000 } as const;

/** What the watched rows read of a controller `daemonLaunchedController` started. */
interface WatchedRun {
  readonly subscriptions: readonly string[];
  readonly order: readonly string[];
  readonly told: readonly Told[];
  readonly exits: readonly number[];
}

/** How many times a row's controller subscribed `topic` through Envoy. */
function asked(run: WatchedRun, topic: string): number {
  return run.subscriptions.filter((subscribed) => subscribed === topic).length;
}

/** What the session was told of its watched workflows (`told` of `WATCHED_NOTICE`), as text. */
function toldWatched(run: WatchedRun): string[] {
  return run.told
    .filter(({ message }) => message.customType === WATCHED_NOTICE)
    .map(({ message }) => String(message.content));
}

/** A row's controller launched with a Slack channel, so its claim follows a Slack topic too. */
function watchingController(answers: Omit<DaemonAnswers, "slackChannels"> = {}) {
  return daemonLaunchedController({ slackChannels: SLACK_CHANNEL, ...answers });
}

/**
 * What every row holds the claim to: its daemon routes, its role claim, its notice topic, its
 * Slack topic and its ready, in the order they come with no watched workflow, before anything
 * of a watched topic is asked, and no exit. After them comes nothing but watched subscriptions.
 */
function expectTheClaimAsBefore(run: WatchedRun): void {
  const claim = [
    "post /healthz",
    "post /legion/v1/claims/register",
    "claim legion-legion-controller",
    `subscribe ${NOTICE_TOPIC}`,
    `subscribe ${SLACK_TOPIC}`,
    "post /legion/v1/claims/ready",
  ];
  expect(run.order.slice(0, claim.length)).toEqual(claim);
  const watched = [NIGHTLY_SCHEDULE, NIGHTLY_DISPATCH, RELEASE_PUSH].map((t) => `subscribe ${t}`);
  expect(run.order.slice(claim.length).filter((step) => !watched.includes(step))).toEqual([]);
  expect(run.exits).toEqual([]);
}

test("watched workflows: a controller launched with two entries, one with two events, subscribes exactly their three default-branch topics", async () => {
  const run = watchingController({ register: registeredWith([NIGHTLY, RELEASE]) });
  try {
    await run.controller.handleSessionStart(run.context);
    await until("the controller's three watched subscriptions", () => asked(run, RELEASE_PUSH));
    await settled();

    expect(run.subscriptions).toEqual([
      NOTICE_TOPIC,
      SLACK_TOPIC,
      NIGHTLY_SCHEDULE,
      NIGHTLY_DISPATCH,
      RELEASE_PUSH,
    ]);
    expectTheClaimAsBefore(run);
    expect(run.told).toEqual([]);
  } finally {
    run.stopSink();
  }
});

// A relaunch is a new process, whose registration names the list as the project names it now:
// it follows that list and no topic of the list its earlier process followed.
test("watched workflows: launched again with one entry, it subscribes only that entry's topic", async () => {
  const first = watchingController({ register: registeredWith([NIGHTLY, RELEASE]) });
  try {
    await first.controller.handleSessionStart(first.context);
    await until("the first launch's watched subscriptions", () => asked(first, RELEASE_PUSH));
  } finally {
    first.stopSink();
  }
  resetEnvoyPluginInterfaceForTests();

  const again = watchingController({ register: registeredWith([RELEASE]) });
  try {
    await again.controller.handleSessionStart(again.context);
    await until("the relaunch's watched subscription", () => asked(again, RELEASE_PUSH));
    await settled();

    expect(again.subscriptions).toEqual([NOTICE_TOPIC, SLACK_TOPIC, RELEASE_PUSH]);
    expectTheClaimAsBefore(again);
    expect(again.told).toEqual([]);
  } finally {
    again.stopSink();
  }
});

// The session's prompt names the list of the process's first registration, and a later claim in
// the same process can be registered with another: the session is then told the list it hears
// now, since the prompt still names the first. A later claim whose list is the same tells nothing.
// No bridge call takes one topic back, so the notice also says that a run of what the new list
// leaves out still arrives until the role ends, and that the session drops it.
test("watched workflows: a later claim in this process whose list differs tells the session the new list", async () => {
  const lists = [[NIGHTLY], [NIGHTLY], [NIGHTLY, RELEASE]];
  let registrations = 0;
  const run = watchingController({
    register: () => registeredWith(lists[registrations++] ?? [])(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await until("the first claim's watched subscriptions", () => asked(run, NIGHTLY_DISPATCH));
    await run.controller.claim(run.context);
    await until("the second claim's watched subscriptions", () => asked(run, NIGHTLY_DISPATCH) > 1);
    await settled();
    expect(toldWatched(run)).toEqual([]);

    await run.controller.claim(run.context);
    await until("the third claim's watched subscriptions", () => asked(run, RELEASE_PUSH));
    await settled();

    expect(run.told).toEqual([
      {
        message: expect.objectContaining({
          customType: WATCHED_NOTICE,
          content: expect.stringContaining(
            "acme/gadgets/release.yml@push, acme/widgets/nightly.yaml@schedule,workflow_dispatch"
          ),
        }),
        options: NO_TURN,
      },
    ]);
    for (const words of [
      "run of an entry or an event that the new list does not name still arrives until this session's controller role ends",
      "drop that run",
    ]) {
      expect(toldWatched(run)[0]).toContain(words);
    }
    expect(run.exits).toEqual([]);
  } finally {
    run.stopSink();
  }
});

// What the session refuses to take is no failure of the claim: a list-change notice that
// `pi.sendMessage` throws on is logged, and the claim, its ready and the new list's topics go on.
// A refused notice told the session nothing, so it still knows the list it knew: the next claim
// with the same list, once the session takes notices again, tells it that list.
test("watched workflows: a session that refuses the list-change notice still holds its claim, its topics are still asked for, and the next claim with that list tells it the list", async () => {
  const refused = "the session takes no message now";
  const lists = [[NIGHTLY], [NIGHTLY, RELEASE], [NIGHTLY, RELEASE]];
  let registrations = 0;
  let refusing = true;
  const run = watchingController({
    register: () => registeredWith(lists[registrations++] ?? [])(),
    tell: () => {
      if (refusing) throw new Error(refused);
    },
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await until("the first claim's watched subscriptions", () => asked(run, NIGHTLY_DISPATCH));
    await run.controller.claim(run.context);
    await until(
      "the second claim's watched subscriptions, or an exit",
      () => asked(run, RELEASE_PUSH) > 0 || run.exits.length > 0
    );
    await settled();

    expect(run.exits).toEqual([]);
    expect(run.claims).toEqual(["legion-legion-controller", "legion-legion-controller"]);
    expect(run.posted.filter((route) => route === "/legion/v1/claims/ready")).toHaveLength(2);
    expect(run.subscriptions).toEqual([
      NOTICE_TOPIC,
      SLACK_TOPIC,
      NIGHTLY_SCHEDULE,
      NIGHTLY_DISPATCH,
      NOTICE_TOPIC,
      SLACK_TOPIC,
      NIGHTLY_SCHEDULE,
      NIGHTLY_DISPATCH,
      RELEASE_PUSH,
    ]);
    expect(run.told).toEqual([]);
    expect(run.lines).toEqual([{ level: "warn", message: expect.stringContaining(refused) }]);
    expect(run.lines[0]?.message).toContain(
      "acme/gadgets/release.yml@push, acme/widgets/nightly.yaml@schedule,workflow_dispatch"
    );

    refusing = false;
    await run.controller.claim(run.context);
    await until(
      "the third claim's watched subscriptions, or an exit",
      () => asked(run, RELEASE_PUSH) > 1 || run.exits.length > 0
    );
    await settled();

    expect(run.exits).toEqual([]);
    expect(run.told).toEqual([
      {
        message: expect.objectContaining({
          customType: WATCHED_NOTICE,
          content: expect.stringContaining(
            "watched workflows as acme/gadgets/release.yml@push, acme/widgets/nightly.yaml@schedule,workflow_dispatch, where this session knew them as acme/widgets/nightly.yaml@schedule,workflow_dispatch"
          ),
        }),
        options: NO_TURN,
      },
    ]);
  } finally {
    run.stopSink();
  }
});

// State table "one watched topic", one row per state and event. Each row's controller follows the
// fixture's one entry, two topics, asked one after the other; the first is the topic the row is
// about. Rows that wait out a 503 run on Bun's fake clock, which the row moves itself.

test("watched workflows: once the claim is done its watched topics are asked for, and the claim has returned already", async () => {
  const held = Promise.withResolvers<void>();
  const run = watchingController({
    subscribe: (topic) => (topic === NIGHTLY_SCHEDULE ? held.promise : Promise.resolve()),
  });
  try {
    await settledWithin(
      "the claim, while its first watched subscription is held",
      run.controller.handleSessionStart(run.context)
    );
    await until("the first watched topic asked for", () => asked(run, NIGHTLY_SCHEDULE));

    expect(run.subscriptions).toEqual([NOTICE_TOPIC, SLACK_TOPIC, NIGHTLY_SCHEDULE]);
    expectTheClaimAsBefore(run);
  } finally {
    held.resolve();
    run.stopSink();
  }
});

test("watched workflows: a watched topic Envoy takes is subscribed, and nothing is told or logged", async () => {
  const run = watchingController();
  try {
    await run.controller.handleSessionStart(run.context);
    await until("both watched topics asked for", () => asked(run, NIGHTLY_DISPATCH));
    await settled();

    expect(run.subscriptions).toEqual([
      NOTICE_TOPIC,
      SLACK_TOPIC,
      NIGHTLY_SCHEDULE,
      NIGHTLY_DISPATCH,
    ]);
    expectTheClaimAsBefore(run);
    expect(run.told).toEqual([]);
    expect(run.lines).toEqual([]);
  } finally {
    run.stopSink();
  }
});

test("watched workflows: a watched topic Envoy answers 503 is asked again after 1 s, with one log line and one notice naming the entry and Envoy's answer", async () => {
  jest.useFakeTimers();
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE ? Promise.reject(withheld()) : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await turnsUntil(() => run.lines.length > 0, "the 503's log line", ROW_TURNS);
    for (let spare = 0; spare < 20; spare++) await settled();

    expect(run.lines).toEqual([
      {
        level: "warn",
        message: expect.stringContaining(
          "was answered 503 at attempt 1; subscribing it again in 1000 ms"
        ),
      },
    ]);
    expect(run.lines[0]?.message).toContain(NIGHTLY_SCHEDULE);
    expect(run.lines[0]?.message).toContain(WITHHELD);
    expect(run.told).toEqual([
      { message: expect.objectContaining({ customType: WATCHED_NOTICE }), options: NO_TURN },
    ]);
    for (const named of ["acme/widgets/nightly.yaml", NIGHTLY_SCHEDULE, WITHHELD]) {
      expect(toldWatched(run)[0]).toContain(named);
    }

    jest.advanceTimersByTime(999);
    for (let spare = 0; spare < 20; spare++) await settled();
    expect(asked(run, NIGHTLY_SCHEDULE)).toBe(1);
    jest.advanceTimersByTime(1);
    await turnsUntil(
      () => asked(run, NIGHTLY_SCHEDULE) === 2,
      "the topic asked again at 1 s",
      ROW_TURNS
    );

    expect(asked(run, NIGHTLY_DISPATCH)).toBe(0);
    expectTheClaimAsBefore(run);
  } finally {
    jest.useRealTimers();
    run.controller.stop();
    run.stopSink();
  }
});

test("watched workflows: each wait of a watched topic Envoy keeps answering 503 doubles, to 30 s at most, and asks again as it ends", async () => {
  jest.useFakeTimers();
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE ? Promise.reject(withheld()) : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    const waits = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000];
    for (const [index, wait] of waits.entries()) {
      await turnsUntil(
        () => run.lines.length > index,
        `the log line of wait ${index + 1}`,
        ROW_TURNS
      );
      expect(run.lines[index]?.message).toContain(
        `was answered 503 at attempt ${index + 1}; subscribing it again in ${wait} ms`
      );
      jest.advanceTimersByTime(wait - 1);
      for (let spare = 0; spare < 20; spare++) await settled();
      expect(asked(run, NIGHTLY_SCHEDULE)).toBe(index + 1);
      jest.advanceTimersByTime(1);
      await turnsUntil(
        () => asked(run, NIGHTLY_SCHEDULE) === index + 2,
        `the topic asked again at the end of wait ${index + 1}`,
        ROW_TURNS
      );
    }

    expect(toldWatched(run)).toHaveLength(1);
    expectTheClaimAsBefore(run);
  } finally {
    jest.useRealTimers();
    run.controller.stop();
    run.stopSink();
  }
});

test("watched workflows: a watched topic Envoy takes after its 503s is told once that it landed", async () => {
  jest.useFakeTimers();
  let refusals = 2;
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE && refusals-- > 0 ? Promise.reject(withheld()) : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    for (const [index, wait] of [1_000, 2_000].entries()) {
      await turnsUntil(
        () => run.lines.length > index,
        `the log line of wait ${index + 1}`,
        ROW_TURNS
      );
      jest.advanceTimersByTime(wait);
    }
    await turnsUntil(
      () => asked(run, NIGHTLY_DISPATCH) === 1,
      "the next watched topic asked for",
      ROW_TURNS
    );
    for (let spare = 0; spare < 20; spare++) await settled();

    expect(asked(run, NIGHTLY_SCHEDULE)).toBe(3);
    expect(run.lines).toHaveLength(2);
    const told = toldWatched(run);
    expect(told).toHaveLength(2);
    expect(told[0]).toContain(WITHHELD);
    expect(told[1]).not.toContain(WITHHELD);
    for (const named of ["acme/widgets/nightly.yaml", NIGHTLY_SCHEDULE]) {
      expect(told[1]).toContain(named);
    }
    expect(run.told.map(({ options }) => options)).toEqual([NO_TURN, NO_TURN]);
    expectTheClaimAsBefore(run);
  } finally {
    jest.useRealTimers();
    run.stopSink();
  }
});

test("watched workflows: a notice the session refuses after a 503 is logged, and the topic is still asked again", async () => {
  jest.useFakeTimers();
  const refused = "the session takes no message now";
  let refusals = 1;
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE && refusals-- > 0 ? Promise.reject(withheld()) : Promise.resolve(),
    tell: (message) => {
      if (String(message.content).includes(WITHHELD)) throw new Error(refused);
    },
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await turnsUntil(
      () => run.lines.length > 1,
      "the 503's log line and the refused notice's",
      ROW_TURNS
    );
    jest.advanceTimersByTime(1_000);
    await turnsUntil(
      () => asked(run, NIGHTLY_DISPATCH) === 1,
      "the next watched topic asked for",
      ROW_TURNS
    );
    for (let spare = 0; spare < 20; spare++) await settled();

    expect(asked(run, NIGHTLY_SCHEDULE)).toBe(2);
    expect(run.lines).toEqual([
      {
        level: "warn",
        message: expect.stringContaining(
          "was answered 503 at attempt 1; subscribing it again in 1000 ms"
        ),
      },
      { level: "warn", message: expect.stringContaining(refused) },
    ]);
    for (const named of [NIGHTLY_SCHEDULE, WITHHELD]) {
      expect(run.lines[1]?.message).toContain(named);
    }
    const told = toldWatched(run);
    expect(told).toHaveLength(1);
    expect(told[0]).not.toContain(WITHHELD);
    expect(told[0]).toContain(NIGHTLY_SCHEDULE);
    expectTheClaimAsBefore(run);
  } finally {
    jest.useRealTimers();
    run.stopSink();
  }
});

test("watched workflows: the session's stop while a watched topic waits after a 503 asks, says and exits nothing more", async () => {
  jest.useFakeTimers();
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE ? Promise.reject(withheld()) : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await turnsUntil(() => run.lines.length > 0, "the 503's log line", ROW_TURNS);
    run.controller.stop();
    jest.advanceTimersByTime(120_000);
    for (let spare = 0; spare < 50; spare++) await settled();

    expect(asked(run, NIGHTLY_SCHEDULE)).toBe(1);
    expect(asked(run, NIGHTLY_DISPATCH)).toBe(0);
    expect(run.lines).toHaveLength(1);
    expect(toldWatched(run)).toHaveLength(1);
    expectTheClaimAsBefore(run);
  } finally {
    jest.useRealTimers();
    run.stopSink();
  }
});

test("watched workflows: a watched subscription Envoy refuses naming the session's stop stops there and says nothing", async () => {
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE
        ? Promise.reject(new RoleClaimStoppingError("legion-legion-controller"))
        : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await until("the first watched topic asked for", () => asked(run, NIGHTLY_SCHEDULE));
    for (let spare = 0; spare < 20; spare++) await settled();

    expect(asked(run, NIGHTLY_DISPATCH)).toBe(0);
    expect(run.lines).toEqual([]);
    expect(run.told).toEqual([]);
    expectTheClaimAsBefore(run);
  } finally {
    run.stopSink();
  }
});

test("watched workflows: a watched subscription Envoy refuses because the session holds the role no more stops there with one log line", async () => {
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE
        ? Promise.reject(new RoleNotHeldError(CONTROLLER_SESSION, "legion-legion-controller"))
        : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await until("the refusal's log line", () => run.lines.length > 0);
    for (let spare = 0; spare < 20; spare++) await settled();

    expect(asked(run, NIGHTLY_DISPATCH)).toBe(0);
    expect(run.lines).toHaveLength(1);
    expect(run.lines[0]?.message).toContain("legion-legion-controller");
    expect(run.told).toEqual([]);
    expectTheClaimAsBefore(run);
  } finally {
    run.stopSink();
  }
});

test("watched workflows: any other refusal of a watched topic gives that topic up with one log line and one notice, and the other topics go on", async () => {
  const refusal = "POST /v1/interests/subscribe failed with 400: the topic is not a subject";
  const run = watchingController({
    subscribe: (topic) =>
      topic === NIGHTLY_SCHEDULE ? Promise.reject(new Error(refusal)) : Promise.resolve(),
  });
  try {
    await run.controller.handleSessionStart(run.context);
    await until("the next watched topic asked for", () => asked(run, NIGHTLY_DISPATCH));
    for (let spare = 0; spare < 20; spare++) await settled();

    expect(asked(run, NIGHTLY_SCHEDULE)).toBe(1);
    expect(run.lines).toHaveLength(1);
    expect(run.lines[0]?.message).toContain(NIGHTLY_SCHEDULE);
    expect(run.lines[0]?.message).toContain(refusal);
    expect(run.told).toEqual([
      { message: expect.objectContaining({ customType: WATCHED_NOTICE }), options: NO_TURN },
    ]);
    for (const named of ["acme/widgets/nightly.yaml", NIGHTLY_SCHEDULE, refusal]) {
      expect(toldWatched(run)[0]).toContain(named);
    }
    expectTheClaimAsBefore(run);
  } finally {
    run.stopSink();
  }
});

/**
 * The controller `legion controller start` launched for the operator (`LEGION_CONTROLLER=1` and a
 * controller capability, no boot token), against a daemon that answers its health and answers its
 * registration with `answers.register`. Every notice, every widget line and every line the Oh My
 * Pi log takes is recorded. Its process exit is a stand-in that does nothing: a claim of this
 * controller never exits the process, whatever refuses it (`claimOnce`).
 */
function operatorLaunchedController(answers: {
  readonly register: () => Response | Promise<Response>;
}) {
  process.env.LEGION_CONTROLLER = "1";
  delete process.env.LEGION_TREE;
  delete process.env.LEGION_BOOT_TOKEN;
  delete process.env.LEGION_BOOT_TOKEN_FILE;
  delete process.env.LEGION_CONTROLLER_SECRET_FILE;
  process.env.LEGION_CONTROLLER_SECRET = fixtureSecret("ctl");
  process.env.LEGION_CONTROLLER_START_MESSAGE = "Run the controller's start procedure.";
  process.env.LEGION_PROJECT = "legion";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  const daemon = createLegionDaemonClient("http://daemon.test", {
    pluginContract: 60,
    fetch: (async (input) => {
      const route = new URL(input.toString()).pathname;
      if (route === "/healthz") {
        return new Response(daemonFixture("health.json"), {
          headers: { "content-type": "application/json" },
        });
      }
      if (route === "/legion/v1/claims/register") return answers.register();
      return new Response(null, { status: 204 });
    }) as typeof fetch,
  });
  const controller = createControllerSession({
    daemon: () => daemon,
    persistedTranscript: async () => ({ sessionFile: TRANSCRIPT, agentId: CONTROLLER_SESSION }),
    pi: {} as PiApi,
    exitProcess: () => undefined as never,
    hostStop: () => undefined,
  });
  const shown: string[] = [];
  const context = {
    sessionManager: {
      getSessionId: () => CONTROLLER_SESSION,
      getSessionFile: () => TRANSCRIPT,
    },
    ui: {
      notify: (message: string) => shown.push(message),
      setWidget: (_key: string, lines: readonly string[] | undefined) => {
        if (lines !== undefined) shown.push(...lines);
      },
    },
  } as unknown as SessionContext & CommandContext;
  const lines: string[] = [];
  const stopSink = logger.registerLogSink((entry) => {
    lines.push(entry.message);
  });
  return { controller, context, shown, lines, stopSink };
}

// The operator-launched controller claims from its start command (`/legion-controller-start`) and
// from a session change's re-claim, and each shows its failure to the operator. A claim the
// session's stop overtook says nothing there either (R24 of dispatch://INFRA-3589): the stop
// begins while `claims/register` is on the wire, and the daemon then refuses the registration. The
// control rows are the same refusals with no stop begun, which the operator is shown.
for (const { name, run, failed } of [
  {
    name: "start command",
    run: (controller: ControllerSession, context: SessionContext & CommandContext) =>
      controller.startCommand(context),
    failed: "legion: the controller claim failed: ",
  },
  {
    name: "re-claim after a session change",
    run: (controller: ControllerSession, context: SessionContext & CommandContext) =>
      controller.reclaimAfterSessionChange(context),
    failed: "legion: re-claiming the controller for this session failed (",
  },
] as const) {
  test(`an operator-launched controller's ${name} whose session stops while claims/register is on the wire, and whose registration the daemon then refuses, logs and shows nothing`, async () => {
    const asked = Promise.withResolvers<void>();
    const held = Promise.withResolvers<Response>();
    const operator = operatorLaunchedController({
      register: () => {
        asked.resolve();
        return held.promise;
      },
    });
    try {
      const claiming = run(operator.controller, operator.context);
      await asked.promise;
      operator.controller.stop();
      held.resolve(registerRefused());
      await claiming;
      await settled();

      expect(operator.shown).toEqual([]);
      expect(operator.lines).toEqual([]);
    } finally {
      operator.stopSink();
    }
  });

  test(`control: an operator-launched controller's ${name} whose registration the daemon refuses with no stop begun shows the failure`, async () => {
    const operator = operatorLaunchedController({ register: registerRefused });
    try {
      await run(operator.controller, operator.context);
      await settled();

      expect(operator.shown).toHaveLength(1);
      expect(operator.shown[0]).toStartWith(failed);
      expect(operator.lines).toContainEqual(
        expect.stringContaining("claims/register for the controller failed (409)")
      );
    } finally {
      operator.stopSink();
    }
  });
}
