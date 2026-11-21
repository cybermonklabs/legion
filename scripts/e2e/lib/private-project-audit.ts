#!/usr/bin/env bun
/**
 * scripts/e2e/lib/private-project-audit.ts — the audit `private-project-devbox.sh` (check
 * c-private-legion) runs at the end of its run, over what the run recorded:
 *
 *   each connection's record, one JSON object per message it received (`subject`, `data`): the
 *     public project's connections must hold no message of the private project, and the private
 *     daemon's no message of the public project. A message is of a project when its subject holds
 *     the project's key as a token, a token opening with `<KEY>-` (an issue key) or
 *     `notifications.legion.<key in lower case>.` (its wake lane), or when its subject or data holds
 *     a marker of that project (what the run wrote there, its sessions and its issues). A GitHub
 *     event (`notifications.github.`) is a message of no project, which reaches both projects'
 *     daemons, and is not judged. A record that holds no message to judge is UNKNOWN, naming the
 *     connection, so an empty read never passes;
 *   the private controller's pod, as `kubectl get pods -o json` read it: it must run in the
 *     project's namespace under the project's controller account, and no pod read is UNKNOWN;
 *   the pods the private namespace's watch saw deleted during the run (`kubectl get pods --watch
 *     --output-watch-events -o json` lines), each with the principal the run read as its deleter:
 *     the controller must have deleted at least one finished issue pod of its project.
 *
 *   bun scripts/e2e/lib/private-project-audit.ts \
 *     --connection <name>,<record file>,<project it must not hold>,<that project's markers file> ... \
 *     --controller-pods <file> --deletions <file> \
 *     --namespace <legion-p> --controller-account <legion-p-controller>
 *
 * It prints one line per judgement, `PASS`, `FAIL` or `UNKNOWN` and why, and exits 0 only when every
 * judgement passed; an UNKNOWN fails the run as a FAIL does.
 */
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { connect, StringCodec } from "nats";
import { z } from "zod";

export type Verdict = { readonly kind: "PASS" | "FAIL" | "UNKNOWN"; readonly line: string };

const Received = z.looseObject({ subject: z.string().optional(), data: z.unknown() });
type Received = z.infer<typeof Received>;

const text = (value: unknown): string =>
  typeof value === "string"
    ? value
    : value === undefined || value === null
      ? ""
      : JSON.stringify(value);

/** Whether `message` is of `project`, by its subject's tokens, its wake lane or a marker. */
export function namesProject(
  message: { readonly subject?: string; readonly data?: unknown },
  project: string,
  markers: readonly string[]
): boolean {
  const subject = message.subject ?? "";
  const data = text(message.data);
  return (
    subject.split(".").some((token) => token === project || token.startsWith(`${project}-`)) ||
    subject.startsWith(`notifications.legion.${project.toLowerCase()}.`) ||
    markers.some((marker) => marker !== "" && (subject.includes(marker) || data.includes(marker)))
  );
}

/** The judgement of one connection's record, `records` its JSONL text (undefined: no file). */
export function auditConnection(
  name: string,
  records: string | undefined,
  project: string,
  markers: readonly string[]
): Verdict {
  if (records === undefined || records.trim() === "") {
    return { kind: "UNKNOWN", line: `UNKNOWN ${name}: its record is empty, so it read nothing` };
  }
  const messages: Received[] = [];
  for (const [index, row] of records.split("\n").entries()) {
    if (row.trim() === "") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(row);
    } catch {
      return {
        kind: "UNKNOWN",
        line: `UNKNOWN ${name}: line ${index + 1} of its record is not JSON`,
      };
    }
    const message = Received.safeParse(parsed);
    if (!message.success) {
      return {
        kind: "UNKNOWN",
        line: `UNKNOWN ${name}: line ${index + 1} of its record is no message`,
      };
    }
    messages.push(message.data);
  }
  const judged = messages.filter(
    (message) => !(message.subject ?? "").startsWith("notifications.github.")
  );
  const github = messages.length - judged.length;
  if (judged.length === 0) {
    return {
      kind: "UNKNOWN",
      line: `UNKNOWN ${name}: it read no message of any project (${github} GitHub events, which name none)`,
    };
  }
  const named = judged.filter((message) => namesProject(message, project, markers));
  const first = named[0];
  if (first !== undefined) {
    const quoted = `${first.subject ?? ""} ${text(first.data).slice(0, 200)}`
      .replace(/\s+/g, " ")
      .trim();
    return {
      kind: "FAIL",
      line: `FAIL ${name}: received ${named.length} messages of ${project}, the first: ${quoted}`,
    };
  }
  return {
    kind: "PASS",
    line: `PASS ${name}: ${judged.length} messages read, none of ${project} (${github} GitHub events not judged)`,
  };
}

const Pod = z.looseObject({
  metadata: z.looseObject({ name: z.string(), namespace: z.string().optional() }),
  spec: z.looseObject({ serviceAccountName: z.string().optional() }).optional(),
  status: z.looseObject({ phase: z.string().optional() }).optional(),
});
const PodList = z.looseObject({ items: z.array(Pod) });

/** The judgement of the controller's pods, `listed` the JSON `kubectl get pods -o json` printed. */
export function auditControllerPods(
  listed: string | undefined,
  namespace: string,
  account: string
): Verdict {
  let pods: z.infer<typeof Pod>[] = [];
  if (listed !== undefined && listed.trim() !== "") {
    let raw: unknown;
    try {
      raw = JSON.parse(listed);
    } catch {
      return { kind: "UNKNOWN", line: "UNKNOWN controller pod: the pod list read is not JSON" };
    }
    const parsed = PodList.safeParse(raw);
    if (!parsed.success)
      return { kind: "UNKNOWN", line: "UNKNOWN controller pod: the pod list read is no pod list" };
    pods = parsed.data.items;
  }
  if (pods.length === 0)
    return { kind: "UNKNOWN", line: "UNKNOWN controller pod: no controller pod was read" };
  for (const pod of pods) {
    const runsAs = pod.spec?.serviceAccountName ?? "";
    const runsIn = pod.metadata.namespace ?? "";
    if (runsAs !== account || runsIn !== namespace) {
      return {
        kind: "FAIL",
        line: `FAIL controller pod: ${pod.metadata.name} runs as ${runsAs || "no account"} in ${runsIn || "no namespace"}, want ${account} in ${namespace}`,
      };
    }
  }
  return {
    kind: "PASS",
    line: `PASS controller pod: ${pods.map((pod) => pod.metadata.name).join(", ")} runs as ${account} in ${namespace}`,
  };
}

const Deletion = z.looseObject({
  type: z.string(),
  object: Pod,
  deleted_by: z.string().optional(),
});

/** The judgement of the deletions the namespace's watch saw, `watched` its JSONL text. */
export function auditFinishedPodDeleted(
  watched: string | undefined,
  namespace: string,
  account: string
): Verdict {
  const deleted: z.infer<typeof Deletion>[] = [];
  for (const row of (watched ?? "").split("\n")) {
    if (row.trim() === "") continue;
    let raw: unknown;
    try {
      raw = JSON.parse(row);
    } catch {
      return {
        kind: "UNKNOWN",
        line: "UNKNOWN finished pod deleted: a line of the pod watch is not JSON",
      };
    }
    const event = Deletion.safeParse(raw);
    if (event.success && event.data.type === "DELETED") deleted.push(event.data);
  }
  const principal = `system:serviceaccount:${namespace}:${account}`;
  const finished = deleted.filter(
    (event) =>
      event.object.metadata.namespace === namespace &&
      (event.object.status?.phase === "Succeeded" || event.object.status?.phase === "Failed")
  );
  const byController = finished.find((event) => event.deleted_by === principal);
  if (byController !== undefined) {
    return {
      kind: "PASS",
      line: `PASS finished pod deleted: ${account} deleted ${byController.object.metadata.name}, a finished pod of ${namespace}`,
    };
  }
  const others = finished.map(
    (event) => `${event.object.metadata.name} by ${event.deleted_by ?? "no principal read"}`
  );
  return {
    kind: "FAIL",
    line: `FAIL finished pod deleted: ${account} deleted no finished pod of ${namespace} during the run (${deleted.length} deletions watched${others.length > 0 ? `; finished: ${others.join(", ")}` : ""})`,
  };
}

const ConsumerReading = z.looseObject({
  durable: z.string(),
  filters: z.array(z.string()),
  delivered: z.number(),
  ack_pending: z.number(),
  num_pending: z.number(),
});
type ConsumerReading = z.infer<typeof ConsumerReading>;

function readings(text: string | undefined): ConsumerReading[] | undefined {
  const rows: ConsumerReading[] = [];
  for (const row of (text ?? "").split("\n")) {
    if (row.trim() === "") continue;
    let raw: unknown;
    try {
      raw = JSON.parse(row);
    } catch {
      return undefined;
    }
    const parsed = ConsumerReading.safeParse(raw);
    if (!parsed.success) return undefined;
    rows.push(parsed.data);
  }
  return rows;
}

/** Whether `filter` is a subject of no project every daemon is granted: a GitHub event, or the
 *  shared wake subject that carries only `listener_ready`. */
const noProject = (filter: string): boolean =>
  filter === "notifications.github.>" || filter === "notifications.legion.wake";

/**
 * The judgement of a deployed daemon's consumers, read twice with its own credential
 * (CONSUMER.INFO, which takes no message): every filter is its own project's or a subject of no
 * project and names no other project, and some consumer was delivered more between the two
 * readings, while the run wrote to that project. No reading is UNKNOWN.
 */
export function auditConsumerInfo(
  name: string,
  before: string | undefined,
  after: string | undefined,
  project: string,
  other: string
): Verdict {
  const first = readings(before);
  const last = readings(after);
  if (first === undefined || last === undefined) {
    return {
      kind: "UNKNOWN",
      line: `UNKNOWN ${name}: a consumer reading is not one reading per line`,
    };
  }
  if (last.length === 0) return { kind: "UNKNOWN", line: `UNKNOWN ${name}: no consumer was read` };
  for (const reading of last) {
    const foreign = reading.filters.find(
      (filter) =>
        !noProject(filter) &&
        (namesProject({ subject: filter }, other, []) ||
          !namesProject({ subject: filter }, project, []))
    );
    if (foreign !== undefined) {
      return {
        kind: "FAIL",
        line: `FAIL ${name}: ${reading.durable} filters ${foreign}, which is not ${project}'s`,
      };
    }
  }
  const moved = last.filter((reading) => {
    const was = first.find((earlier) => earlier.durable === reading.durable);
    return was !== undefined && reading.delivered > was.delivered;
  });
  if (moved.length === 0) {
    return {
      kind: "FAIL",
      line: `FAIL ${name}: no consumer of ${project} was delivered anything while the run wrote to ${project}`,
    };
  }
  return {
    kind: "PASS",
    line: `PASS ${name}: ${last.length} consumers filter ${project}'s subjects or none's; ${moved.map((reading) => reading.durable).join(", ")} delivered more during the run`,
  };
}

function readIfThere(path: string): string | undefined {
  return existsSync(path) ? readFileSync(path, "utf8") : undefined;
}

const ConsumerInfoAnswer = z.looseObject({
  config: z.looseObject({
    filter_subject: z.string().optional(),
    filter_subjects: z.array(z.string()).optional(),
  }),
  delivered: z.looseObject({ consumer_seq: z.number() }),
  num_ack_pending: z.number(),
  num_pending: z.number(),
});

/**
 * `consumer-info --servers <url> --token-file <file> --out <file> --durable <name>...`: reads each
 * named consumer of ENVOY_NOTIFICATIONS with CONSUMER.INFO, which takes no message, connected as
 * the service whose platform token the file holds (user `service`, the token as the password), and
 * writes one reading per line to --out. A refusal or a missing consumer exits 1, naming it.
 */
async function consumerInfo(args: readonly string[]): Promise<number> {
  const durables: string[] = [];
  const single = new Map<string, string>();
  for (let i = 0; i < args.length; i += 2) {
    const flag = args[i] ?? "";
    const value = args[i + 1] ?? "";
    if (flag === "--durable") durables.push(value);
    else single.set(flag.replace(/^--/, ""), value);
  }
  const servers = single.get("servers") ?? "";
  const tokenFile = single.get("token-file") ?? "";
  const out = single.get("out") ?? "";
  if (servers === "" || tokenFile === "" || out === "" || durables.length === 0) {
    console.error(
      "private-project-audit consumer-info: needs --servers, --token-file, --out and a --durable"
    );
    return 2;
  }
  const codec = StringCodec();
  const connection = await connect({
    servers,
    user: "service",
    pass: readFileSync(tokenFile, "utf8").trim(),
    timeout: 20_000,
  });
  const rows: string[] = [];
  try {
    for (const durable of durables) {
      const reply = await connection.request(
        `$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.${durable}`,
        codec.encode(""),
        {
          timeout: 5_000,
        }
      );
      const answer = ConsumerInfoAnswer.safeParse(JSON.parse(codec.decode(reply.data)));
      if (!answer.success) {
        console.error(
          `private-project-audit consumer-info: ${durable}: ${codec.decode(reply.data).slice(0, 300)}`
        );
        return 1;
      }
      const { config, delivered, num_ack_pending, num_pending } = answer.data;
      const filters =
        config.filter_subjects ??
        (config.filter_subject === undefined ? [] : [config.filter_subject]);
      rows.push(
        JSON.stringify({
          durable,
          filters,
          delivered: delivered.consumer_seq,
          ack_pending: num_ack_pending,
          num_pending,
        })
      );
    }
  } catch (error) {
    console.error(
      `private-project-audit consumer-info: ${error instanceof Error ? error.message : String(error)}`
    );
    return 1;
  } finally {
    await connection.close();
  }
  writeFileSync(out, `${rows.join("\n")}\n`);
  return 0;
}

function main(args: readonly string[]): number {
  const connections: string[] = [];
  const consumerArgs: string[] = [];
  const single = new Map<string, string>();
  for (let i = 0; i < args.length; i += 2) {
    const flag = args[i] ?? "";
    const value = args[i + 1];
    if (!flag.startsWith("--") || value === undefined) {
      console.error(`private-project-audit: expected --<flag> <value> at ${flag}`);
      return 2;
    }
    if (flag === "--connection") connections.push(value);
    else if (flag === "--consumers") consumerArgs.push(value);
    else single.set(flag.slice(2), value);
  }
  const need = ["controller-pods", "deletions", "namespace", "controller-account"];
  const missing = need.filter((name) => !single.has(name));
  if (connections.length === 0 || missing.length > 0) {
    console.error(
      `private-project-audit: needs --connection and ${need.map((name) => `--${name}`).join(", ")}; missing ${[...(connections.length === 0 ? ["--connection"] : []), ...missing.map((name) => `--${name}`)].join(", ")}`
    );
    return 2;
  }
  const namespace = single.get("namespace") ?? "";
  const account = single.get("controller-account") ?? "";
  const verdicts: Verdict[] = [];
  for (const connection of connections) {
    const [name, record, project, markersFile] = connection.split(",");
    if (!name || !record || !project || !markersFile) {
      console.error(
        `private-project-audit: --connection takes <name>,<record>,<project>,<markers>, got ${connection}`
      );
      return 2;
    }
    const markers = (readIfThere(markersFile) ?? "").split("\n").filter((marker) => marker !== "");
    verdicts.push(auditConnection(name, readIfThere(record), project, markers));
  }
  verdicts.push(
    auditControllerPods(readIfThere(single.get("controller-pods") ?? ""), namespace, account)
  );
  verdicts.push(
    auditFinishedPodDeleted(readIfThere(single.get("deletions") ?? ""), namespace, account)
  );
  // A deployed daemon's consumers, by `--consumers <name>,<before>,<after>,<project>,<other>`.
  for (const consumers of consumerArgs) {
    const [name, before, after, project, other] = consumers.split(",");
    if (!name || !before || !after || !project || !other) {
      console.error(
        `private-project-audit: --consumers takes <name>,<before>,<after>,<project>,<other>, got ${consumers}`
      );
      return 2;
    }
    verdicts.push(auditConsumerInfo(name, readIfThere(before), readIfThere(after), project, other));
  }
  for (const verdict of verdicts) console.log(verdict.line);
  return verdicts.every((verdict) => verdict.kind === "PASS") ? 0 : 1;
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  process.exit(args[0] === "consumer-info" ? await consumerInfo(args.slice(1)) : main(args));
}
