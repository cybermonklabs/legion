// The topics a deployment names for a role (`ENVOY_ROLE_TOPICS`), end to end: a real natsd and
// listener built from this checkout, the listener's own Slack webhook taking a signed event, and
// real Oh My Pi extension processes (`scripts/role-takeover/omp-client.ts`), one client instance
// each, as `scripts/smoke-role-takeover.sh` drives them. The post is the shape a Cloud Monitoring
// incident takes in a private alert channel: a bot's message, no mention of the app, which Envoy
// publishes on `notifications.slack.<team>.<channel>.message`. Every id in it is invented.
//
// The rows run only with LEGION_E2E_ENVOY_STACK=1, which the envoy-session-stall job sets
// (`.github/workflows/ci-job-envoy-session-stall.yaml`): they build the stack with Go.
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHmac, randomUUID } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "@legion/envoy-client/test/envoy-stack";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { settled, until } from "@legion/test-fixtures/wait";
import { z } from "zod";

const stackDirectory = useEnvoyStackDirectory();
const work = temporaryDirectory("role-topics");

const PACKAGE = path.resolve(import.meta.dir, "..");
const DRIVERS = path.join(PACKAGE, "scripts", "role-takeover");
const SIGNING_SECRET = fixtureSecret("slack");
const TEAM = "T0ACME0001";
const ALERTS_CHANNEL = "C0ALERTS01";
const ALERTS_TOPIC = `notifications.slack.*.${ALERTS_CHANNEL}.message`;
const OWNER_ROLE = "ops-owner";
const CONTROLLER_ROLE = "legion-acme-controller";
const UNRELATED_ROLE = "legion-acme-1-architect";
// What every pod of the deployment carries, the bystander's included: the variable names topics
// for two roles, and only the session holding one of them follows them.
const ROLE_TOPICS = JSON.stringify({
  [OWNER_ROLE]: [ALERTS_TOPIC],
  [CONTROLLER_ROLE]: [ALERTS_TOPIC],
});
// How long a registration keeps a claim renewed: a relaunched process under the same session id
// is granted the role once the killed one's claim lapses, so seconds rather than the deployment's
// five minutes.
const LIVE_WINDOW = "5s";
const RUN = randomUUID().replaceAll("-", "").slice(0, 12);

/** One line a client answered on its stdout: what every answer carries, and the rest kept. */
const AnswerSchema = z.looseObject({
  id: z.string().optional(),
  ok: z.boolean().optional(),
  error: z.string().optional(),
  ready: z.boolean().optional(),
});
type Answer = z.infer<typeof AnswerSchema>;

/** The `entries` answer: the session's branch, of which the rows read the role claim records. */
const EntriesSchema = z.object({
  entries: z.array(
    z.looseObject({
      customType: z.string().optional(),
      data: z
        .looseObject({
          role: z.string().nullable().optional(),
          client_instance_id: z.string().optional(),
        })
        .optional(),
    })
  ),
});

/** The listener's interest registry row for a session (`GET /v1/registry/<id>`). */
const RegistrySchema = z.object({ topics: z.array(z.string()) });

/** The listener's role lookup (`GET /v1/roles/<role>`): the holding session and process. */
const RoleSchema = z.object({
  holder: z.string(),
  client_instance_id: z.string().optional(),
});

/** What a tool answer carries of the fact this file reads: `envoy_role_set`'s `details.following`,
 *  the topics the session follows for the role it took. */
const ToolResultSchema = z.looseObject({
  details: z.looseObject({ following: z.array(z.string()).default([]) }),
});
/**
 * Starts `omp-client.ts` with an environment of the row's alone, as `clients.sh`'s `start_omp`
 * starts one: the stack's listener and natsd (`config`), no Dispatch, the heartbeat left to the row
 * to tick. Its stderr goes to `log`, read when it fails: a pipe nobody drained would stop the
 * client once its buffer filled.
 */
function spawnOmpClient(
  home: string,
  config: string,
  log: string,
  env: Readonly<Record<string, string>>
) {
  return Bun.spawn(
    [
      process.execPath,
      "--preload",
      path.join(DRIVERS, "host-stub.ts"),
      path.join(DRIVERS, "omp-client.ts"),
    ],
    {
      cwd: PACKAGE,
      env: {
        PATH: process.env.PATH ?? "",
        HOME: home,
        TMPDIR: process.env.TMPDIR ?? "/tmp",
        ENVOY_CONFIG_FILE: config,
        ENVOY_HEARTBEAT_MS: "600000",
        ENVOY_PASS_DEADLINE_MS: "30000",
        ROLE_TAKEOVER_CWD: path.join(home, "cwd"),
        ...env,
      },
      stdin: "pipe",
      stdout: "pipe",
      stderr: Bun.file(log),
    }
  );
}

/** One Oh My Pi Envoy extension instance in a process of its own (`omp-client.ts`). */
class OmpClient {
  private readonly pending = new Map<string, (answer: Answer) => void>();
  private next = 0;

  private constructor(
    readonly label: string,
    private readonly child: ReturnType<typeof spawnOmpClient>
  ) {}

  static async start(
    stack: EnvoyStack,
    label: string,
    env: Readonly<Record<string, string>>
  ): Promise<OmpClient> {
    const home = path.join(work, `${label}-home`);
    mkdirSync(path.join(home, "cwd"), { recursive: true });
    const config = path.join(home, "envoy.json");
    writeFileSync(
      config,
      JSON.stringify({
        envoy: { url: stack.envoyUrl, natsUrls: [stack.natsUrl] },
        dispatch: { enabled: false },
        askgate: { enabled: false },
      })
    );
    const log = path.join(work, `${label}.err`);
    const child = spawnOmpClient(home, config, log, env);
    const ready = Promise.withResolvers<void>();
    const client = new OmpClient(label, child);
    void client.read(ready.resolve);
    const exited = await Promise.race([ready.promise.then(() => undefined), child.exited]);
    if (exited !== undefined) {
      throw new Error(
        `the ${label} client exited ${exited} before it was up: ${await Bun.file(log).text()}`
      );
    }
    return client;
  }

  private async read(markReady: () => void): Promise<void> {
    const decoder = new TextDecoder();
    let buffered = "";
    for await (const chunk of this.child.stdout) {
      buffered += decoder.decode(chunk, { stream: true });
      let end = buffered.indexOf("\n");
      while (end >= 0) {
        const line = buffered.slice(0, end).trim();
        buffered = buffered.slice(end + 1);
        end = buffered.indexOf("\n");
        if (line === "") continue;
        const answer = AnswerSchema.parse(JSON.parse(line));
        if (answer.ready === true) markReady();
        const settle = answer.id === undefined ? undefined : this.pending.get(answer.id);
        if (settle !== undefined && answer.id !== undefined) {
          this.pending.delete(answer.id);
          settle(answer);
        }
      }
    }
  }

  /** Sends one command and answers the client's reply, failing on a reply that says it failed. */
  async send(command: Record<string, unknown>): Promise<Answer> {
    this.next += 1;
    const id = String(this.next);
    const answered = Promise.withResolvers<Answer>();
    this.pending.set(id, answered.resolve);
    this.child.stdin.write(`${JSON.stringify({ id, ...command })}\n`);
    this.child.stdin.flush();
    const answer = await settled(`the ${this.label} client's answer`, answered.promise);
    if (answer.ok !== true) {
      throw new Error(
        `the ${this.label} client refused ${JSON.stringify(command)}: ${answer.error}`
      );
    }
    return answer;
  }

  /** Whether a card the extension showed holds `marker`. */
  async shown(marker: string): Promise<boolean> {
    const answer = await this.send({ op: "deliveries" });
    return JSON.stringify(answer.deliveries ?? []).includes(marker);
  }

  /** The client instance id of the newest role claim this process recorded for `role`. */
  async claimRecordedFor(role: string): Promise<string | undefined> {
    const { entries } = EntriesSchema.parse(await this.send({ op: "entries" }));
    return entries.findLast(
      (entry) => entry.customType === "envoy-role-claim" && entry.data?.role === role
    )?.data?.client_instance_id;
  }

  /** The host going away for good: its input ends, and the extension's shutdown runs. */
  async close(): Promise<void> {
    this.child.stdin.end();
    await Promise.race([this.child.exited, Bun.sleep(15_000)]);
    this.child.kill("SIGKILL");
  }

  /** The pod evicted: the process dies at once, deregistering nothing and handing nothing back. */
  async kill(): Promise<void> {
    this.child.kill("SIGKILL");
    await this.child.exited;
  }
}

/** Waits until `holds` answers true, failing with `what` at the row's deadline. The facts these
 *  rows wait for are a real listener's and real processes', which no fake clock reaches, so the
 *  probe asks the real one; `between` runs before each ask again, for a row that must tick a
 *  heartbeat. */
async function waitFor(
  what: string,
  holds: () => Promise<boolean>,
  between: () => Promise<void> = async () => {}
): Promise<void> {
  let first = true;
  await until(what, async () => {
    if (first) first = false;
    else await between();
    return holds();
  });
}

/** The topics the listener's interest registry holds for `session`. */
async function registeredTopics(stack: EnvoyStack, session: string): Promise<readonly string[]> {
  const response = await fetch(`${stack.envoyUrl}/v1/registry/${session}`);
  if (!response.ok) return [];
  return RegistrySchema.parse(await response.json()).topics;
}

/** The session and client instance the listener's claim of `role` names, if a live claim stands. */
async function roleHolder(
  stack: EnvoyStack,
  role: string
): Promise<z.infer<typeof RoleSchema> | undefined> {
  const response = await fetch(`${stack.envoyUrl}/v1/roles/${role}`);
  if (!response.ok) return undefined;
  return RoleSchema.parse(await response.json());
}

/**
 * Posts `text` to the listener's Slack webhook as Slack posts an incident a monitoring app wrote
 * in a private channel: an `event_callback` holding a `message` with subtype `bot_message`,
 * `channel_type` `group` and a `bot_id`, mentioning nobody, signed as Slack signs it.
 */
async function postAlert(stack: EnvoyStack, text: string): Promise<void> {
  const now = Date.now() / 1000;
  const body = JSON.stringify({
    token: "unused",
    team_id: TEAM,
    api_app_id: "A0LEGION01",
    type: "event_callback",
    event_id: `Ev${randomUUID().replaceAll("-", "").slice(0, 10).toUpperCase()}`,
    event_time: Math.floor(now),
    event: {
      type: "message",
      subtype: "bot_message",
      channel: ALERTS_CHANNEL,
      channel_type: "group",
      bot_id: "B0MONITOR1",
      username: "Google Cloud Monitoring",
      text,
      ts: now.toFixed(6),
      event_ts: now.toFixed(6),
    },
  });
  const timestamp = String(Math.floor(now));
  const signature = `v0=${createHmac("sha256", SIGNING_SECRET).update(`v0:${timestamp}:${body}`).digest("hex")}`;
  const response = await fetch(`${stack.envoyUrl}/webhook/slack`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Slack-Request-Timestamp": timestamp,
      "X-Slack-Signature": signature,
    },
    body,
  });
  expect(response.status).toBe(200);
}

describe.skipIf(!envoyStackEnabled)("a role's topics across a relaunch", () => {
  let stack: EnvoyStack | undefined;
  const clients: OmpClient[] = [];
  let bystander: OmpClient | undefined;
  const BYSTANDER = `ses_rt_bystander_${RUN}`;
  // Every alert the rows post: the bystander's control reads that it was shown none of them.
  const posted: string[] = [];

  const requireStack = (): EnvoyStack => {
    if (stack === undefined) throw new Error("the Envoy stack did not start");
    return stack;
  };
  const start = async (label: string, env: Readonly<Record<string, string>>) => {
    const client = await OmpClient.start(requireStack(), label, env);
    clients.push(client);
    return client;
  };
  const post = async (label: string): Promise<string> => {
    const marker = `incident ${label} ${RUN}: refusals over the threshold for 5 minutes`;
    posted.push(marker);
    await postAlert(requireStack(), marker);
    return marker;
  };

  beforeAll(async () => {
    stack = await startEnvoyStack({
      work: stackDirectory,
      listenerEnv: {
        ENVOY_WEBHOOKS: "slack",
        ENVOY_SLACK_SIGNING_SECRET: SIGNING_SECRET,
        ENVOY_SESSION_LIVE_WINDOW: LIVE_WINDOW,
        ENVOY_ROLE_CLAIM_REAP_INTERVAL: "2s",
      },
    });
    // A session holding a role the variable names no topic for, in a pod carrying the same
    // variable as every other: the control.
    bystander = await start("bystander", {
      ENVOY_ROLE_TOPICS: ROLE_TOPICS,
      ENVOY_SESSION_ROLE: UNRELATED_ROLE,
      ENVOY_SESSION_ROLE_SESSION: BYSTANDER,
    });
    await bystander.send({ op: "start", session: BYSTANDER });
  }, 180_000);

  afterAll(async () => {
    for (const client of clients) await client.close();
    await stack?.stop();
  }, 120_000);

  // The control, after each row's last post: the bystander follows nothing of the alert channel,
  // so it was shown no alert. A message sent to it after the last post shows its consumer
  // delivered everything the stream stored before that message, in order, so an alert it
  // followed would have been shown first.
  const bystanderShownNoAlert = async (sender: OmpClient): Promise<void> => {
    const live = requireStack();
    if (bystander === undefined) throw new Error("the bystander did not start");
    const witness = bystander;
    const sentinel = `sentinel ${RUN} ${posted.length}`;
    await sender.send({
      op: "tool",
      name: "envoy_send",
      args: { session_id: BYSTANDER, message: sentinel },
    });
    await waitFor("the bystander's sentinel", () => witness.shown(sentinel));
    const shown: string[] = [];
    for (const marker of posted) if (await witness.shown(marker)) shown.push(marker);
    expect(shown).toEqual([]);
    expect(await registeredTopics(live, BYSTANDER)).not.toContain(ALERTS_TOPIC);
  };

  test("a session pod's role keeps its topic when the pod is killed and its session relaunched", async () => {
    const live = requireStack();
    const owner = `ses_rt_owner_${RUN}`;
    const launch = {
      ENVOY_ROLE_TOPICS: ROLE_TOPICS,
      ENVOY_SESSION_ROLE: OWNER_ROLE,
      ENVOY_SESSION_ROLE_SESSION: owner,
    };
    const first = await start("owner-1", launch);
    await first.send({ op: "start", session: owner });
    await waitFor("the owner's registration of the alert topic", async () =>
      (await registeredTopics(live, owner)).includes(ALERTS_TOPIC)
    );
    const beforeKill = await post("before-kill");
    await waitFor("the first process to show the alert posted before the kill", () =>
      first.shown(beforeKill)
    );

    await first.kill();
    const whileDown = await post("while-down");

    const second = await start("owner-2", launch);
    await second.send({ op: "start", session: owner });
    // The killed process's claim stands until the live window lapses; each pass the row ticks
    // claims the role again, and the claim this process records is the one the listener names.
    await waitFor(
      "the relaunched process to hold the owner role",
      async () => {
        const recorded = await second.claimRecordedFor(OWNER_ROLE);
        const held = await roleHolder(live, OWNER_ROLE);
        return (
          recorded !== undefined && held?.holder === owner && held.client_instance_id === recorded
        );
      },
      async () => {
        await second.send({ op: "heartbeat" });
      }
    );
    await waitFor("the relaunched process to show the alert posted while the pod was down", () =>
      second.shown(whileDown)
    );
    const afterRelaunch = await post("after-relaunch");
    await waitFor("the relaunched process to show the alert posted after it took the role", () =>
      second.shown(afterRelaunch)
    );
    expect(await registeredTopics(live, owner)).toContain(ALERTS_TOPIC);

    await bystanderShownNoAlert(second);
  }, 240_000);

  test("a claim on a new session after a relaunch follows the role's topic again", async () => {
    const live = requireStack();
    const before = `ses_rt_controller_a_${RUN}`;
    const after = `ses_rt_controller_b_${RUN}`;
    const env = { ENVOY_ROLE_TOPICS: ROLE_TOPICS };

    const first = await start("controller-1", env);
    await first.send({ op: "start", session: before });
    const claimed = await first.send({
      op: "tool",
      name: "envoy_role_set",
      args: { role: CONTROLLER_ROLE },
    });
    // The fact as data: the sentence beside it is the tool's wording, which a reword may change.
    expect(ToolResultSchema.parse(claimed.result).details.following).toEqual([ALERTS_TOPIC]);
    // Registered by the claim itself, not left to the next heartbeat, which this row never ticks.
    expect(await registeredTopics(live, before)).toContain(ALERTS_TOPIC);
    const firstAlert = await post("controller-first");
    await waitFor("the first controller session to show its alert", () => first.shown(firstAlert));

    // The relaunch: the process ends, and a new process starts a new session that takes the role.
    await first.close();
    const second = await start("controller-2", env);
    await second.send({ op: "start", session: after });
    expect(await registeredTopics(live, after)).not.toContain(ALERTS_TOPIC);
    await second.send({ op: "tool", name: "envoy_role_set", args: { role: CONTROLLER_ROLE } });
    expect(await registeredTopics(live, after)).toContain(ALERTS_TOPIC);
    const secondAlert = await post("controller-second");
    await waitFor("the new controller session to show its alert", () => second.shown(secondAlert));

    await bystanderShownNoAlert(second);
  }, 240_000);

  test("a session that resumes without the role follows none of its topics", async () => {
    const live = requireStack();
    const holder = `ses_rt_left_${RUN}`;
    const taker = `ses_rt_taker_${RUN}`;
    const env = { ENVOY_ROLE_TOPICS: ROLE_TOPICS };

    // A session takes the role, follows its topic, and its process is killed: nothing closes the
    // topic, so the listener's row for that session still holds it.
    const held = await start("left-1", env);
    await held.send({ op: "start", session: holder });
    await held.send({ op: "tool", name: "envoy_role_set", args: { role: CONTROLLER_ROLE } });
    expect(await registeredTopics(live, holder)).toContain(ALERTS_TOPIC);
    await held.kill();

    // Another session takes the role, so the resumed one cannot have it back.
    const other = await start("left-taker", env);
    await other.send({ op: "start", session: taker });
    await waitFor(
      "the other session to hold the controller role",
      async () => (await roleHolder(live, CONTROLLER_ROLE))?.holder === taker,
      async () => {
        await other.send({ op: "tool", name: "envoy_role_set", args: { role: CONTROLLER_ROLE } });
      }
    );

    // The killed session's id resumes. It does not get the role, so it follows none of its
    // topics, and the pass takes the left-over out of its row.
    const resumed = await start("left-2", env);
    await resumed.send({ op: "start", session: holder, roles: [CONTROLLER_ROLE] });
    await waitFor(
      "the resumed session's row to lose the role's topic",
      async () => !(await registeredTopics(live, holder)).includes(ALERTS_TOPIC),
      async () => {
        await resumed.send({ op: "heartbeat" });
      }
    );
    expect((await roleHolder(live, CONTROLLER_ROLE))?.holder).toBe(taker);

    const afterResume = await post("after-resume");
    await waitFor("the role's holder to show the alert", () => other.shown(afterResume));
    expect(await resumed.shown(afterResume)).toBe(false);

    await bystanderShownNoAlert(other);
  }, 240_000);
});
