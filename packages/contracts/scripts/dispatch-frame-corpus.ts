// The payloads fixtures/dispatch-frame-verdicts.json records a verdict for (readDispatchPayload):
// four payloads a host reads as events - a targeted message, a targeted comment mention, an
// outbox event and a person's message on no issue - and every one-step change of them: each
// field at depth one and two removed, set to null, to a number, to a fraction, to a string, to
// true, to an object and to an array, a field added beside them, the event's type swapped and
// the two delivery lines swapped, and a few payloads that are no object. The edge listener's Go
// reader of the same shapes (envoy internal/contracts, DispatchPayloadIsEvent) must agree with
// each verdict, so a shape the TypeScript changes without the Go, or the other way round, fails a
// test on one side.

const uuid = "11111111-1111-4111-8111-111111111111";
const person = { kind: "user", id: "alice" };
const messageDelivery = { attempt: 1, mode: "btw" };
const commentDelivery = {
  attempt: 1,
  mode: "steer",
  comment_id: uuid,
  target: "session:ses_target",
};

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

const targetedMessage: Json = {
  event: {
    id: 0,
    issue_key: "CORE-1",
    artifact_id: null,
    project: "",
    seq: 0,
    type: "message.created",
    actor: person,
    notify: false,
    created_at: "0001-01-01T00:00:00Z",
    payload: {
      id: uuid,
      issue_key: "CORE-1",
      author: person,
      body: "Can this ship?",
      target: "session:ses_target",
      in_reply_to: null,
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [],
    },
  },
  delivery: messageDelivery,
};

const conversationMessage: Json = {
  event: {
    id: 0,
    issue_key: null,
    artifact_id: null,
    project: "",
    seq: 0,
    type: "message.created",
    actor: person,
    notify: false,
    created_at: "0001-01-01T00:00:00Z",
    payload: {
      id: uuid,
      issue_key: null,
      author: person,
      body: "Look at the build.",
      target: "session:ses_target",
      in_reply_to: null,
      broadcast_id: null,
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [],
      reply_block: null,
    },
  },
  delivery: { attempt: 1, mode: "steer" },
};

const targetedComment: Json = {
  event: {
    id: 0,
    issue_key: "CORE-1",
    artifact_id: null,
    project: "",
    seq: 0,
    type: "comment.created",
    actor: person,
    notify: true,
    created_at: "0001-01-01T00:00:00Z",
    payload: {
      id: uuid,
      issue_key: "CORE-1",
      artifact_id: null,
      author: person,
      body: "@ses_target please look",
      reply_to: null,
      ask_id: null,
      mentions: [],
      deliveries: [],
      created_at: "2026-09-12T00:00:00Z",
      artifact_name: "",
    },
  },
  delivery: commentDelivery,
};

const outboxEvent: Json = {
  id: 41,
  issue_key: "CORE-1",
  artifact_id: null,
  project: "CORE",
  seq: 7,
  type: "comment.created",
  actor: person,
  notify: true,
  created_at: "2026-09-12T00:00:00Z",
  payload: { id: "c-1", body: "a note" },
  quiet: false,
};

const replacements: readonly [string, Json | undefined][] = [
  ["removed", undefined],
  ["null", null],
  ["number", 7],
  ["fraction", 1.5],
  ["string", "x"],
  ["true", true],
  ["object", {}],
  ["array", []],
];

function clone(value: Json): Json {
  return JSON.parse(JSON.stringify(value)) as Json;
}

function isRecord(value: Json | undefined): value is { [key: string]: Json } {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Every path to an object field at depth one and two below `value`'s top. */
function paths(value: Json, depth: number, prefix: string[] = []): string[][] {
  if (!isRecord(value) || depth === 0) return [];
  return Object.keys(value).flatMap((key) => [
    [...prefix, key],
    ...paths(value[key] as Json, depth - 1, [...prefix, key]),
  ]);
}

function at(value: Json, path: readonly string[]): { [key: string]: Json } {
  let node = value;
  for (const key of path) node = (node as { [key: string]: Json })[key] as Json;
  return node as { [key: string]: Json };
}

function changed(base: Json, path: readonly string[], replacement: Json | undefined): string {
  const copy = clone(base);
  const parent = at(copy, path.slice(0, -1));
  const key = path[path.length - 1] as string;
  if (replacement === undefined) delete parent[key];
  else parent[key] = replacement;
  return JSON.stringify(copy);
}

export type DispatchFrameCase = { readonly name: string; readonly payload: string };

export function dispatchFrameCorpus(): DispatchFrameCase[] {
  const bases: readonly [string, Json, number][] = [
    ["targeted message", targetedMessage, 3],
    ["conversation message", conversationMessage, 3],
    ["targeted comment", targetedComment, 3],
    ["outbox event", outboxEvent, 2],
  ];
  const cases: DispatchFrameCase[] = [];
  for (const [name, base, depth] of bases) {
    cases.push({ name, payload: JSON.stringify(base) });
    for (const path of paths(base, depth)) {
      for (const [label, replacement] of replacements) {
        cases.push({
          name: `${name}: ${path.join(".")} ${label}`,
          payload: changed(base, path, replacement),
        });
      }
    }
    for (const path of [[], ...paths(base, depth - 1)]) {
      const node = path.length === 0 ? base : at(base, path);
      if (!isRecord(node)) continue;
      cases.push({
        name: `${name}: a field added beside ${path.join(".") || "the top"}`,
        payload: changed(base, [...path, "zz_added"], "PUB"),
      });
    }
  }
  for (const type of ["message.created", "comment.created", "message.delivery", "issue.updated"]) {
    for (const [name, base] of [
      ["targeted message", targetedMessage],
      ["targeted comment", targetedComment],
      ["conversation message", conversationMessage],
    ] as const) {
      cases.push({
        name: `${name}: type ${type}`,
        payload: changed(base, ["event", "type"], type),
      });
    }
  }
  cases.push(
    {
      name: "targeted message: a comment's delivery line",
      payload: changed(targetedMessage, ["delivery"], commentDelivery),
    },
    {
      name: "targeted comment: a message's delivery line",
      payload: changed(targetedComment, ["delivery"], messageDelivery),
    },
    { name: "null", payload: "null" },
    { name: "an array", payload: "[]" },
    { name: "a string", payload: '"a note"' },
    { name: "a number", payload: "7" }
  );
  return cases;
}
