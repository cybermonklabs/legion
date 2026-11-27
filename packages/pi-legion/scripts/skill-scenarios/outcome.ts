#!/usr/bin/env bun
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { recordWorkspace, translateCommits } from "./git-world";
import { type OutcomeRecord, parseOutcome } from "./heldout-case";
import { parseInstant } from "./production-dispatch";
import {
  dispatchFirstInjected,
  resultText,
  type SessionEntry,
  session,
  sessionCalls,
  sessionId,
  skillReads,
  subagentSessions,
} from "./transcript";

type Action = OutcomeRecord["actions"][number];
type Unnumbered<T> = T extends Action ? Omit<T, "seq"> : never;
type Pending = Unnumbered<Action>;
const Fields = z.record(z.string(), z.json());
const DaemonLine = z.object({
  at: z.string(),
  method: z.string(),
  path: z.string(),
  status: z.number(),
  body: z.json(),
  answer: z.json(),
});
/** One line of calls.jsonl: gh-standin.ts's record of a gh call, with the request gh makes for it
 * as gh reads its arguments (`request`) and the change it kept to a pull request or a review
 * (`pull`), which it writes only for a call it took; or a line of rig.sh's other stand-ins. */
const Call = z.object({
  at: z.string(),
  as: z.string(),
  request: z
    .object({
      command: z.string().optional(),
      method: z.string().optional(),
      path: z.string().optional(),
      fields: Fields,
    })
    .optional(),
  pull: z.object({ action: z.string() }).catchall(z.json()).optional(),
});
const StageLine = z.object({ at: z.string(), tree: z.string(), files: z.array(z.string()) });
const Readback = z.object({
  seedThrough: z.number(),
  events: z.array(
    z.object({
      seq: z.number(),
      created_at: z.string(),
      type: z.string(),
      issue_key: z.string(),
      actor: z.object({ kind: z.string(), id: z.string() }),
      payload: Fields,
    })
  ),
  handoffs: z.array(z.object({ id: z.string(), body: z.union([z.string(), Fields]) })),
  results: z.object({
    checks: z.array(z.object({ results: z.array(Fields).nullable() })).nullable(),
  }),
});

function lines(file: string): string[] {
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "");
}
function jsonLines<T>(file: string, schema: z.ZodType<T>): T[] {
  return lines(file).map((line, i) => {
    try {
      return schema.parse(JSON.parse(line));
    } catch (error) {
      throw new Error(`${file}:${i + 1}: ${String(error)}`);
    }
  });
}

// Translate the daemon's write routes to the op names used by the Legion tool. Internal
// registration, readiness, grant, activity, exit and turn-outcome traffic is not a role action.
const daemonOps = new Map(
  Object.entries({
    "handoff/complete": "handoff_complete",
    "issues/status": "set_status",
    "claims/ready-for-update": "ready_for_update",
    "claims/fresh-session": "fresh_session",
    "claims/compact": "compact",
    "project/pause": "pause",
    "project/resume": "resume",
    "intake/replay": "replay_give_up",
    "intake/discard": "discard_give_up",
    "gates/register": "register_gate",
    "waves/release": "release_children",
    "phase/backward": "request_backward_move",
    "phase/retry": "retry_or_escalate",
    signoff: "sign_off",
    "roots/close": "close_root",
    "children/rerun": "rerun_child",
    "plans/register": "register_plan",
    "tasks/stop": "stop_task",
  })
);
const completeFields = new Map(
  Object.entries({
    checkOpenedAt: "check_opened_at",
    wakeAt: "wake_at",
    planRating: "plan_rating",
    planReason: "plan_reason",
    returnTo: "return_to",
  })
);

/** The daemon's answer as an outcome records it: its refusal code (`HTTP_<status>` for a refusal
 * that names none, null where the daemon took the request) and its body. */
function daemonAnswer(line: z.infer<typeof DaemonLine>) {
  const code = Fields.safeParse(line.answer).data?.code;
  return {
    code: typeof code === "string" ? code : line.status >= 400 ? `HTTP_${line.status}` : null,
    body: line.answer,
  };
}

function daemonAction(line: z.infer<typeof DaemonLine>): Pending[] {
  const op = daemonOps.get(line.path.replace(/^\/legion\/v1\//, ""));
  if (line.method !== "POST" || op === undefined) return [];
  const { grantId: _grant, secret: _secret, ...rest } = Fields.safeParse(line.body).data ?? {};
  const args =
    op === "handoff_complete"
      ? Object.fromEntries(
          Object.entries(rest).map(([key, value]) => [completeFields.get(key) ?? key, value])
        )
      : rest;
  return [{ kind: "daemon", op, at: line.at, args, answer: daemonAnswer(line) }];
}

/** Whether a GET's path is the route of a Legion tool's daemon read, for the issue the call names
 * (`undefined`: the claim's own, any issue's record). A case can score the read that anchors a
 * wait. */
const daemonReads: Record<string, (path: string, issue: string | undefined) => boolean> = {
  read_record: (path, issue) =>
    issue === undefined
      ? /^\/legion\/v1\/issues\/[^/]+\/record$/.test(path)
      : path === `/legion/v1/issues/${issue}/record`,
  read_state: (path) => path === "/legion/v1/state",
};

/** The role's own reads of the daemon. The Legion extension checks the issue's record on the same
 * route, so a GET is the role's only inside the window of a legion tool call of a read op, from
 * the call's message to its result; each GET answers one call. */
function daemonReadActions(
  entries: readonly SessionEntry[],
  log: readonly z.infer<typeof DaemonLine>[]
): Pending[] {
  const resultAt = new Map(
    entries.flatMap((entry) =>
      entry.message?.role === "toolResult" &&
      entry.message.toolCallId !== undefined &&
      entry.timestamp !== undefined
        ? [[entry.message.toolCallId, nanos(entry.timestamp)] as const]
        : []
    )
  );
  const taken = new Set<number>();
  return sessionCalls(entries).flatMap(({ entry, call }): Pending[] => {
    const { op, i: _intent, ...args } = Fields.safeParse(call.arguments).data ?? {};
    const name = typeof op === "string" ? op : "";
    const route = daemonReads[name];
    const until = call.id === undefined ? undefined : resultAt.get(call.id);
    if (call.name?.replace(/^_+/, "") !== "legion" || route === undefined) return [];
    if (entry.timestamp === undefined || until === undefined) return [];
    const from = nanos(entry.timestamp);
    const issue = typeof args.issue === "string" ? args.issue : undefined;
    const index = log.findIndex(
      (line, i) =>
        !taken.has(i) &&
        line.method === "GET" &&
        route(line.path, issue) &&
        nanos(line.at) >= from &&
        nanos(line.at) <= until
    );
    const line = log[index];
    if (line === undefined) return [];
    taken.add(index);
    return [{ kind: "daemon", op: name, at: line.at, args, answer: daemonAnswer(line) }];
  });
}

// Each Dispatch event the server writes (packages/envoy/internal/dispatch), by the command that
// makes it. A new version of a document is `artifact.version` (docs.ArtifactVersionEventPayload),
// whether `dispatch doc-edit` or a new upload under the document's name made it; a person's edit
// in the browser is `artifact.human_edited` (docs/edit_notices.go), which no command makes.
const commands = new Map(
  Object.entries({
    "issue.created": "issue",
    "issue.updated": "issue-update",
    "issue.closed": "issue-update",
    "issue.claimed": "claim",
    "issue.released": "claim",
    "ask.opened": "ask",
    "ask.edited": "edit-ask",
    "ask.resolved": "resolve-ask",
    "comment.created": "comment",
    "comment.edited": "edit-comment",
    "comment.deleted": "delete-comment",
    "comment.resolved": "resolve-comment",
    "message.created": "message",
    "message.edited": "edit-message",
    "message.deleted": "delete-message",
    "artifact.created": "artifact",
    "artifact.version": "doc-edit",
    "handoff.recorded": "handoff-write",
    "result.recorded": "result",
  })
);

type DispatchWrite = Omit<Extract<Pending, { kind: "dispatch" }>, "kind"> & {
  actor: { kind: string; id: string };
};

/** The Dispatch writes after the seed, each with the actor Dispatch names as its writer: those of
 * the role's own sessions (`own`) are its actions, and every other actor's (Dispatch itself, a
 * seeded person, another session) is kept apart, as no action of the role's. */
function dispatchWrites(
  readback: z.infer<typeof Readback>,
  own: ReadonlySet<string>
): { actions: Pending[]; others: DispatchWrite[] } {
  const handoffs = new Map(readback.handoffs.map(({ id, body }) => [id, body]));
  const results = new Map(
    (readback.results.checks ?? [])
      .flatMap((check) => check.results ?? [])
      .map((result) => [result.id, result])
  );
  // A `*.delivery` event is Dispatch's own attempt to hand a comment or an ask to its target
  // through Envoy, and a run's Dispatch has no Envoy (start_dispatch's FAKE_ENVOY_PORT), so each
  // one failed there: no write of the role's. The write it delivers keeps its targets.
  const writes = readback.events
    .filter((event) => event.seq > readback.seedThrough && !event.type.endsWith(".delivery"))
    .map((event): DispatchWrite => {
      const payload = event.payload;
      let fields = payload;
      let target = event.issue_key;
      switch (event.type) {
        case "handoff.recorded": {
          const body = handoffs.get(String(payload.handoff_id));
          if (body === undefined)
            throw new Error(`missing handoff ${payload.handoff_id} at event ${event.seq}`);
          fields = {
            ...payload,
            body: typeof body === "string" ? Fields.parse(JSON.parse(body)) : body,
          };
          break;
        }
        case "result.recorded": {
          const result = results.get(payload.result_id);
          fields = {
            ...payload,
            ...result,
            check: payload.check_block_id ?? "",
            commit: payload.commit_sha ?? "",
          };
          if (typeof payload.issue === "string") target = payload.issue;
          break;
        }
      }
      const { kind, id } = event.actor;
      return {
        command: commands.get(event.type) ?? event.type,
        target,
        fields,
        actor: { kind, id },
        at: event.created_at,
      };
    });
  const ours = (write: DispatchWrite) => write.actor.kind === "session" && own.has(write.actor.id);
  return {
    actions: writes.filter(ours).map((write) => ({ kind: "dispatch", ...write })),
    others: writes.filter((write) => !ours(write)),
  };
}

const Method = z.enum(["GET", "POST", "PATCH", "PUT", "DELETE"]);

/** A gh call as gh-standin.ts recorded it: the request, as gh reads its arguments (gh-standin's
 * readGhArgs, the one reading of them), and the change the stand-in kept, which is what the call
 * did. Nothing here reads the arguments again. */
function githubAction(call: z.infer<typeof Call>): Pending[] {
  if (call.as !== "gh") return [];
  const { request, pull } = call;
  if (request === undefined)
    throw new Error(`calls.jsonl's gh call at ${call.at} records no request`);
  const common = {
    kind: "github" as const,
    at: call.at,
    fields: request.fields,
    ...(pull === undefined ? {} : { change: pull }),
  };
  // Reads are recorded as writes are: a case can score the read that anchors a wait (a pull
  // request's head, its approval). GraphQL reads use POST, so only a mutation is recorded, and
  // gh's own login and dashboard are no GitHub action.
  if (request.method !== undefined) {
    const query = request.fields.query;
    if (request.path === "graphql" && (typeof query !== "string" || !/\bmutation\b/.test(query)))
      return [];
    if (!request.path) return [];
    return [{ ...common, method: Method.parse(request.method), path: request.path }];
  }
  const command = request.command ?? "";
  if (command.trim() === "gh" || /^gh (auth|status)( |$)/.test(command)) return [];
  return [{ ...common, command }];
}

function nanos(at: string): bigint {
  const parsed = parseInstant(at);
  if (parsed === undefined) throw new Error(`not an RFC 3339 timestamp: ${at}`);
  return parsed;
}
function shifted(at: bigint, offset: bigint): string {
  const ts = at + offset;
  const seconds = ts / 1_000_000_000n;
  const fractional = (ts % 1_000_000_000n).toString().padStart(9, "0");
  return new Date(Number(seconds) * 1000).toISOString().replace(/\.000Z$/, `.${fractional}Z`);
}

/** Raw logs use the same host clock. Compare full precision, breaking exact ties by input order,
 * then move actions onto the fixture's timeline. No seeded write is scored, and of the Dispatch
 * writes after the seed only those of the role's own sessions are its actions: the run's session
 * and each subagent it started, by the session id each transcript's header names. */
export function writeOutcome(dir: string, agent?: string): OutcomeRecord {
  const file = (name: string) => path.join(dir, name);
  const parent = session(dir);
  if (parent.length === 0) throw new Error(`${dir}: no session transcript`);
  const subagents = subagentSessions(dir);
  const role =
    agent === undefined
      ? parent
      : (subagents.find(({ name }) => name === agent)?.entries ?? parent);
  const own = new Set(
    [parent, ...subagents.map(({ entries }) => entries)].flatMap(
      (entries) => sessionId(entries) ?? []
    )
  );
  if (sessionId(parent) === undefined)
    throw new Error(
      `${dir}: the session transcript names no session, so the role's own writes are unknown`
    );
  const dispatch = dispatchWrites(
    Readback.parse(JSON.parse(readFileSync(file("dispatch-readback.json"), "utf8"))),
    own
  );
  const daemonLog = jsonLines(file("daemon.log"), DaemonLine);
  const actions: Pending[] = [
    ...daemonLog.flatMap(daemonAction),
    ...daemonReadActions(parent, daemonLog),
    ...dispatch.actions,
    ...jsonLines(file("calls.jsonl"), Call).flatMap(githubAction),
    ...lines(file("pushes.log")).map((line): Pending => {
      const hit = /^(\S+) (\S+) [0-9a-f]{40} ([0-9a-f]{40})$/.exec(line);
      if (!hit?.[1] || !hit[2] || !hit[3]) throw new Error(`bad pushes.log row: ${line}`);
      return {
        kind: "push",
        at: hit[1],
        ref: hit[2].replace(/^refs\/heads\//, ""),
        commit: hit[3],
      };
    }),
    ...jsonLines(file("staging.jsonl"), StageLine).map(
      (line): Pending => ({ kind: "staging", ...line })
    ),
  ];
  let agentReport: string | undefined;
  if (agent !== undefined) {
    const calls = sessionCalls(parent);
    const [task] = calls;
    const args = Fields.safeParse(task?.call.arguments).data;
    const selected =
      Array.isArray(args?.tasks) && args.tasks.length === 1
        ? Fields.safeParse(args.tasks[0]).data
        : args;
    if (calls.length !== 1 || task?.call.name !== "task" || selected?.agent !== agent)
      throw new Error(`${dir}: A case did not make exactly one task call to ${agent}`);
    const report = parent.find(
      ({ message }) => message?.role === "toolResult" && message.toolCallId === task.call.id
    )?.message;
    const details = z
      .object({
        results: z.tuple([
          z.object({
            output: z.string(),
            exitCode: z.number(),
            aborted: z.boolean().optional(),
            error: z.unknown().optional(),
          }),
        ]),
      })
      .safeParse(report?.details);
    const text = resultText(report).trim();
    const completed = details.data?.results[0];
    if (completed !== undefined) {
      if (completed.exitCode !== 0 || completed.aborted || completed.error)
        throw new Error(`${dir}: ${agent}'s task failed`);
      agentReport = completed.output.trim();
    } else {
      agentReport =
        /^\s*<task-result\b[^>]*status="completed"[^>]*>\s*[\s\S]*?<(output|preview)\b[^>]*>([\s\S]*?)<\/\1>/
          .exec(text)?.[2]
          ?.trim() ?? (text.startsWith("<task-result") || text.includes("(job ") ? "" : text);
    }
    if (!agentReport || agentReport === "(no output)" || report?.isError)
      throw new Error(`${dir}: ${agent} returned no report`);
  }
  const lastText =
    parent
      .filter(({ message }) => message?.role === "assistant")
      .map(({ message }) =>
        (message?.content ?? [])
          .filter(({ type }) => type === "text")
          .map(({ text }) => text ?? "")
          .join("")
      )
      .findLast((text) => text.trim() !== "") ?? "";
  const offset =
    nanos(readFileSync(file("clock.txt"), "utf8").trim()) -
    nanos(readFileSync(file("clock-real.txt"), "utf8").trim());
  const mapping = z
    .record(z.string(), z.string())
    .parse(JSON.parse(readFileSync(file("commit-map.json"), "utf8")));
  const inverse = Object.fromEntries(
    Object.entries(mapping).map(([caseId, actual]) => [actual, caseId])
  );
  const timed = actions
    .map((action) => ({ action, ns: nanos(action.at) }))
    .sort((a, b) => (a.ns < b.ns ? -1 : a.ns > b.ns ? 1 : 0));
  const ordered = timed.map(({ action, ns }, i) => ({
    ...action,
    at: shifted(ns, offset),
    seq: i + 1,
  }));
  const others = dispatch.others
    .map((write) => ({ write, ns: nanos(write.at) }))
    .sort((a, b) => (a.ns < b.ns ? -1 : a.ns > b.ns ? 1 : 0))
    .map(({ write, ns }) => ({ ...write, at: shifted(ns, offset) }));
  const deployments = file("deployments.jsonl");
  const onWire =
    existsSync(deployments) &&
    jsonLines(deployments, z.object({ dispatchFirst: z.boolean() })).some(
      ({ dispatchFirst }) => dispatchFirst
    );
  const result = parseOutcome(
    JSON.stringify({
      id: randomUUID(),
      actions: translateCommits(ordered, inverse),
      ...(others.length === 0 ? {} : { otherWrites: translateCommits(others, inverse) }),
      ...(agentReport === undefined ? {} : { agentReport: translateCommits(agentReport, inverse) }),
      // A read is before the first scored action by the host clock both records share: the time
      // of the outcome's first action, before it moves onto the case's timeline.
      skillReads: skillReads(role, timed[0]?.ns),
      dispatchFirstInjected: dispatchFirstInjected(role, onWire),
      lastText: translateCommits(lastText, inverse),
    })
  );
  // The jj operation record of the run's workspace, beside the other records lane D's replay
  // places against the run's calls: the workspace's own moves are recorded nowhere else.
  if (existsSync(file("workspace/.jj"))) recordWorkspace(file("workspace"), dir);
  writeFileSync(file("outcome.json.new"), `${JSON.stringify(result, null, 2)}\n`);
  renameSync(file("outcome.json.new"), file("outcome.json"));
  return result;
}

if (import.meta.main) {
  const [dir, agent] = Bun.argv.slice(2);
  if (!dir || Bun.argv.length > 4) throw new Error("usage: outcome.ts <run dir> [agent]");
  writeOutcome(dir, agent);
}
