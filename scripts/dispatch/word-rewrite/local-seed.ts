import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { parseArgs } from "node:util";
import { z } from "zod";
import type { DispatchReads } from "./dispatch-api.ts";
import { readContract } from "./name-map.ts";
import { emptyState, newestEventId, PROJECT_KEY, readLog, serializeState } from "./state.ts";

/**
 * The seed of check k6 on a local Dispatch (`bun run dispatch:local`): every item k6 lists, the
 * stub daemons below, at and above the contract this checkout records and one whose state has no
 * `contract` key, an address no daemon answers on, and the items the fixtures of
 * `__fixtures__/word-rewrite/capture.sh` are captured from. It writes through the routes
 * `packages/dispatch/e2e/api.ts` wraps, with plain `fetch`: as a session (the local harness's agent
 * bearer, `e2e-token`, naming a session in `actor`), as the person alice (the dev sign-in's cookie
 * and the dashboard's `Origin`), and as an agent acting through alice's personal token.
 *
 * Each document the tool writes to stands on an issue of its own, where k6 lets it, since the pace
 * writes one item of an issue in 15 minutes. The tree's root (`--tree`) stays open: k6 closes it
 * after the write mode's first run, as the merge closes INFRA-2017. Four paragraphs end with the
 * sentence "The checks ran on Tuesday.": k6's proxy of the model gateway answers each of them
 * with a rewrite that changes that sentence, which the check of each answer refuses twice.
 *
 * Each stub daemon answers `GET /legion/v1/state`; `PUT /contract` with `{"daemon": <n>}` sets the
 * contract its state reports from then on (k6 raises the daemon below the contract this way).
 *
 * It prints one JSON object (and writes it to `--out` when named): every key, id and address the
 * runs and the capture need. Then it keeps the stub daemons serving until it is stopped.
 *
 * `--field-only` seeds what k6 seeds again on each start of a stamped Dispatch build: an issue
 * document that names a field of Dispatch's priorities answer under its old name, and the state
 * document of SEED that a fully read state leaves for it (the document waiting for Dispatch's
 * build to hold the merge, the cursor after its upload). It starts no daemon and exits.
 *
 * The seeded texts hold the word: they are the test input of the tool that rewrites it.
 *
 *   bun scripts/dispatch/word-rewrite/local-seed.ts --url http://127.0.0.1:<port> [--out <file>]
 *     [--field-only]
 */

const { values } = parseArgs({
  args: Bun.argv.slice(2),
  options: {
    url: { type: "string" },
    out: { type: "string" },
    token: { type: "string" },
    "field-only": { type: "boolean" },
  },
  strict: true,
});
if (values.url === undefined) {
  console.error(
    "usage: local-seed.ts --url <local Dispatch base URL> [--out <file>] [--token <t>] [--field-only]"
  );
  process.exit(2);
}
const base = values.url.replace(/\/+$/, "");
const agentToken = values.token ?? "e2e-token";
const session = { kind: "session", id: crypto.randomUUID() };
const contract = readContract(resolve(import.meta.dir, "../../.."));

type Caller = "agent" | "alice" | { readonly bearer: string };

let aliceCookie: string | undefined;

async function cookieOf(login: string): Promise<string> {
  const response = await fetch(`${base}/auth/_dev/signin?login=${login}`, { redirect: "manual" });
  const cookie = response.headers
    .getSetCookie()
    .find((header) => header.startsWith("dsession="))
    ?.split(";")[0];
  if (response.status !== 302 || cookie === undefined) {
    throw new Error(`dev sign-in as ${login} answered ${response.status}`);
  }
  return cookie;
}

async function call(method: string, path: string, body: object | undefined, as: Caller) {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  let payload = body;
  if (as === "alice") {
    aliceCookie ??= await cookieOf("alice");
    headers.Cookie = aliceCookie;
    headers.Origin = new URL(base).origin;
  } else {
    // Dispatch refuses a bearer's write that names no session (`ACTOR_KIND`), the shared token's
    // and a personal agent token's alike.
    headers.Authorization = `Bearer ${as === "agent" ? agentToken : as.bearer}`;
    payload = body === undefined ? undefined : { ...body, actor: session };
  }
  const response = await fetch(`${base}${path}`, {
    method,
    headers,
    body: payload === undefined ? undefined : JSON.stringify(payload),
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`${method} ${path} answered ${response.status}: ${text}`);
  return text === "" ? null : JSON.parse(text);
}

async function project(key: string): Promise<void> {
  try {
    await call("POST", "/api/v1/projects", { key, name: `Word rewrite ${key}` }, "alice");
  } catch (error) {
    if (!(error instanceof Error && error.message.includes(" 409"))) throw error;
  }
}

async function issue(title: string, spec: string, parent?: string) {
  const body = { project: "SEED", title, spec, force: true, ...(parent ? { parent } : {}) };
  const created = await call("POST", "/api/v1/issues", body, "agent");
  return { key: String(created.key), spec: String(created.primary_artifact_id) };
}

async function upload(owner: string, name: string, content: string, as: Caller = "agent") {
  const path = PROJECT_KEY.test(owner)
    ? `/api/v1/projects/${owner}/artifacts`
    : `/api/v1/issues/${owner}/artifacts`;
  const uploaded = await call("POST", path, { name, content }, as);
  return String(uploaded.artifact.id);
}

/** One edit of a document, with one op, as `as`. */
async function edit(id: string, op: object, as: Caller) {
  await call("POST", `/api/v1/artifacts/${id}/edits`, { ops: [op] }, as);
}

const insert = (id: string, markdown: string, as: Caller = "agent") =>
  edit(id, { op: "insert", markdown, after: "end" }, as);

const replace = (id: string, find: string, replacement: string, as: Caller) =>
  edit(id, { op: "replace", find, with: replacement }, as);

const approve = (id: string) =>
  call("POST", `/api/v1/artifacts/${id}/reviews`, { state: "approved" }, "alice");

/** An open approval request on a document, as an agent asks for one; answers the ask's id. */
async function requestApproval(id: string): Promise<string> {
  const requested = await call(
    "POST",
    `/api/v1/artifacts/${id}/approval-requests`,
    { summary: "Approve the spec." },
    "agent"
  );
  return String(requested.ask.id);
}

const close = (key: string) => call("PATCH", `/api/v1/issues/${key}`, { status: "done" }, "agent");

/** The event log's reads, as the agent. */
const logReads: Pick<DispatchReads, "eventLog"> = {
  eventLog: (after, limit) =>
    call("GET", `/api/v1/events/log?after=${after}&limit=${limit}`, undefined, "agent"),
};

/** The payload of a person's edit notice: the document it names. */
const EditNotice = z.looseObject({ artifact_id: z.string() });

/** The field of Dispatch's priorities answer, under its old name. */
const FIELD_TEXT = "The rail reads priority_work_parked from the priorities answer.\n";

function print(seeded: object): void {
  const printed = JSON.stringify(seeded, null, 2);
  console.log(printed);
  if (values.out !== undefined) writeFileSync(values.out, `${printed}\n`);
}

if (values["field-only"] === true) {
  // What a start of a stamped build needs, since each start empties the database: the tree the
  // run names, the document, and the state a whole read left, with the document waiting for the
  // build to hold the merge and the cursor after its upload, so no event names it.
  await project("SEED");
  const tree = await issue("Tree of the word rewrite", "The subject of this tree.\n");
  const field = await issue("Priorities field", "Specification.\n");
  const fieldDocument = await upload(field.key, "priorities.md", FIELD_TEXT);
  const cursor = await newestEventId(logReads);
  const state = {
    ...emptyState(),
    cursor,
    writeCursor: cursor,
    whole: true,
    waiting: [{ project: "SEED", issue: field.key, id: fieldDocument, gate: "m" as const }],
  };
  await upload("SEED", "rewrite-state.md", serializeState(state));
  print({
    base,
    session: session.id,
    issues: { tree: tree.key, field: field.key },
    documents: { priorities: fieldDocument },
    cursor,
    run_flags: [
      "--tree",
      tree.key,
      "--state-document",
      "dispatch://SEED/artifact/rewrite-state-md",
    ],
  });
  process.exit(0);
}

/** A stub daemon whose state reports `daemon` as its contract, or no `contract` key for null;
 *  `PUT /contract` with `{"daemon": <n>}` sets the contract it reports from then on. */
function stub(daemon: number | null) {
  let reported = daemon;
  return Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const path = new URL(request.url).pathname;
      if (path === "/legion/v1/state") {
        const contract =
          reported === null ? {} : { contract: { daemon: reported, caller: reported } };
        return Response.json({ ...contract, admission: { held: [] } });
      }
      if (path === "/contract" && request.method === "PUT") {
        const body = (await request.json()) as { daemon?: unknown };
        if (typeof body.daemon !== "number" || !Number.isInteger(body.daemon)) {
          return new Response("daemon takes a whole number", { status: 400 });
        }
        reported = body.daemon;
        return Response.json({ daemon: reported });
      }
      if (path === "/healthz") return Response.json({ status: "ok" });
      return new Response("not found", { status: 404 });
    },
  });
}

const stubs = {
  at: stub(contract),
  below: stub(contract - 1),
  above: stub(contract + 1),
  noContract: stub(null),
};
const address = (server: { port?: number | undefined }) => `http://127.0.0.1:${server.port}`;
// An address no daemon answers on: a port the system gave a server that is stopped at once.
const gone = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: () => new Response(null) });
const silent = address(gone);
await gone.stop(true);

for (const key of ["SEED", "BELOW", "ABOVE", "NOKEY", "NONE", "NOADDR", "GIVEN", "SILENT"])
  await project(key);
const registered: Record<string, string> = {
  SEED: address(stubs.at),
  BELOW: address(stubs.below),
  ABOVE: address(stubs.above),
  NOKEY: address(stubs.noContract),
};
for (const [key, stateUrl] of Object.entries(registered)) {
  await call("PUT", `/api/v1/projects/${key}/legion/daemon`, { state_url: stateUrl }, "alice");
}

// The tree the run names (`--tree`), with a child. It stays open: k6 closes its root.
const tree = await issue(
  "Tree of the word rewrite",
  "The word parked is the subject of this tree."
);
const child = await issue(
  "Rename the parked names",
  "Each parked name gets its own words.",
  tree.key
);

// The open issue: an agent's title and spec that use the word for a status, and its documents.
const main = await issue(
  "Parked until the release lands",
  "This issue is parked until the release lands.\n\nThe second paragraph names nothing of it.\n"
);
const mixed = await upload(
  main.key,
  "mixed.md",
  "Café ✓ — the parked tree waits for the release.\n\n" +
    ':::ask{#ask-word urgency="med" multiple="false"}\nShould the parked issue move to backlog?\n\n- Yes\n- No\n:::\n\n' +
    ':::check{#check-word surface="cli" run="go test -run TestParkedDecision ./internal/daemon/"}\nThe parked tree resumes.\n:::\n'
);
await insert(mixed, "Alice added this: the parked tree is hers.", "alice");
const personal = await call(
  "POST",
  "/api/v1/me/agent-tokens",
  { name: "word-rewrite seed" },
  "alice"
);
const personalToken = String(personal.token);
const act = await upload(main.key, "act.md", "Plain text that names nothing.\n");
await insert(act, "An agent for Alice parked this.", { bearer: personalToken });
const quotes = await upload(
  main.key,
  "quotes.md",
  'Sami wrote: "park it until the release".\n\nThe rule says never to write "parked" for a status.\n'
);
const kernel = await upload(
  main.key,
  "kernel.md",
  "The fix touches kthread_park in the scheduler.\n\nA parked kernel thread does not run until it is woken.\n"
);
const priorities = await upload(main.key, "priorities.md", FIELD_TEXT);
const verification = await upload(
  main.key,
  "verification.md",
  ':::check{#k1 surface="cli" run="go test -run TestBootReleasesParkedClaims ./internal/daemon/" when="before_merge"}\nThe parked tree resumes once its root leaves the backlog.\n:::\n'
);
const slugged = await upload(main.key, "parked-notes.md", "Notes on the release.\n");

// A spec and a Verification document a person approved at their latest version.
const approved = await issue(
  "Approved plan",
  "The approved plan keeps the parked tree as it is.\n"
);
const approvedVerification = await upload(
  approved.key,
  "verification.md",
  "The approved checks name the parked tree.\n"
);
await approve(approved.spec);
await approve(approvedVerification);

// A document a person approved at an earlier version, which now holds an open ask block.
const earlier = await issue("Approved earlier", "Specification.\n");
const earlierDocument = await upload(earlier.key, "earlier.md", "The parked plan, as approved.\n");
await approve(earlierDocument);
await insert(
  earlierDocument,
  ':::ask{#may-move urgency="med" multiple="false"}\nMay this plan move to backlog?\n\n- Yes\n- No\n:::\n'
);

// A document an open approval request names: k6's second session withdraws it.
const requested = await issue("Requested", "The requested spec says the issue was parked.\n");
const requestedAsk = await requestApproval(requested.spec);

// A document a design gate names, with no approval: the tool changes it (spec version 18).
const gated = await issue("Gated", "The gated spec was parked by the controller.\n");
await call(
  "PUT",
  `/api/v1/issues/${gated.key}/design-gates`,
  { gates: [{ step: "spec", artifact_id: gated.spec, holds: true }] },
  "agent"
);

// A closed issue whose title and document hold the word.
const closed = await issue("Parked and closed", "The closed issue was parked.\n");
const closedDocument = await upload(closed.key, "notes.md", "The closed notes say parked.\n");

// The items the capture writes to, which hold no trace of the word, and the issue of the last
// seed step. They come right after the issues above, so that each test that reads a fixture
// finds the keys it names (`SEED-3` to `SEED-10`) whatever k6 adds after them.
const capture = await issue("Capture", "Capture.\n");
const captureDocument = await upload(capture.key, "capture.md", "Capture text.\n");
const log = await issue("Log steps", "Log steps.\n");
const logDocument = await upload(log.key, "log-a.md", "First.\n");

// A block a person made, and one a session made that a person changed.
const peopleIssue = await issue("People", "Specification.\n");
const people = await upload(
  peopleIssue.key,
  "people.md",
  "A session wrote this paragraph about the parked tree.\n\nA person will change this paragraph: the parked issue waits.\n"
);
await insert(people, "Alice wrote this paragraph: we parked it for now.", "alice");
await replace(people, "A person will change", "A person changed", "alice");

// The notice kind in SEED, whose daemon reads at the contract.
const noticeIssue = await issue("Notice", "Specification.\n");
const notice = await upload(
  noticeIssue.key,
  "notice.md",
  "The controller reads the parked-decision notice at its next wake.\n"
);

// A second document an open approval request names: k6's person answers it "Request changes".
const answered = await issue("Requested again", "The second requested spec says it was parked.\n");
const answeredAsk = await requestApproval(answered.spec);

// The first twice-failing answer, beside an occurrence the same run changes. k6 moves its issue
// from todo to in_progress, so it starts in todo.
const deploy = await issue("Deploy", "Specification.\n");
await call("PATCH", `/api/v1/issues/${deploy.key}`, { status: "todo" }, "agent");
const deployDocument = await upload(
  deploy.key,
  "deploy.md",
  "The deploy was parked for a week. The checks ran on Tuesday.\n\nThe follow-up issue is parked until the deploy lands.\n"
);

// A twice-failing answer in a paragraph that holds a quote of the person and a status use, and one
// in a paragraph that holds only quotes.
const quoteStatus = await issue("Quote and status", "Specification.\n");
const quoteStatusDocument = await upload(
  quoteStatus.key,
  "quote-status.md",
  'Sami wrote "park the deploy". The deploy was parked on Monday. The checks ran on Tuesday.\n'
);
const quotesOnly = await issue("Quotes only", "Specification.\n");
const quotesOnlyDocument = await upload(
  quotesOnly.key,
  "quotes-only.md",
  'Sami wrote "park it" and Alice wrote "parked for now". The checks ran on Tuesday.\n'
);

// An agent paragraph that holds both a quote of the person and a status use, which the model
// answers.
const quoteUse = await issue("Quote and use", "Specification.\n");
const quoteUseDocument = await upload(
  quoteUse.key,
  "quote-use.md",
  'Sami wrote "park it until Friday", so the release issue was parked.\n'
);

// An agent's typed block, neither an ask nor a check, with the word in an attribute of its
// opening line.
const callout = await issue("Callout", "Specification.\n");
const calloutDocument = await upload(
  callout.key,
  "callout.md",
  ':::callout{#tree-note kind="note" title="Unpark the tree"}\nThe tree resumes.\n:::\n'
);

// A title a session wrote through alice's personal agent token.
const actTitle = await issue("An agent's title for Alice", "Specification.\n");
await call(
  "PATCH",
  `/api/v1/issues/${actTitle.key}`,
  { title: "Parked by an agent for Alice" },
  { bearer: personalToken }
);

// The items k6's second session adds the word to: an issue document the tool does not change,
// and a title, each on an issue of its own.
const untouched = await issue("Untouched", "Specification.\n");
const untouchedDocument = await upload(
  untouched.key,
  "untouched.md",
  "Plain text that names nothing.\n"
);
const plainTitle = await issue("Plain title", "Specification.\n");

// The project documents: SEED's own, and the notice kind in each project of the daemon rows. The
// document k6's second session edits between the tool's read and its write is ABOVE's first, so
// that it is the first item the write mode reaches: ABOVE is the first project Dispatch lists.
const seedNotes = await upload(
  "SEED",
  "notes.md",
  "The release notes say the issue was parked until Monday.\n"
);
const race = await upload("ABOVE", "race.md", "The parked branch waits for its review.\n");
const notices: Record<string, string> = {};
for (const key of ["BELOW", "ABOVE", "NOKEY", "NONE", "NOADDR", "GIVEN", "SILENT"]) {
  notices[key] = await upload(
    key,
    "notice.md",
    "The controller reads the parked-decision notice at its next wake.\n"
  );
}
// In a project with no daemon address that the run does not name: a twice-failing answer in a
// sentence that holds the renamed notice kind and a status use.
const noticeStatus = await upload(
  "NOADDR",
  "notice-status.md",
  "The parked-decision notice says the tree is parked. The checks ran on Tuesday.\n"
);

await close(closed.key);

// Dispatch writes a person's edit as an `artifact.human_edited` event once the document has been
// quiet for 30 seconds (`docs/edit_notices.go`). The last step waits for the notice of alice's
// last edit, people.md's, by which time the sweep has sent every notice due before it, so that
// event-log.json holds that step's three events alone.
for (let waited = 0; ; waited += 1) {
  let noticed = false;
  await readLog(logReads, 0, (page) => {
    noticed ||= page.some(
      (event) =>
        event.type === "artifact.human_edited" &&
        EditNotice.safeParse(event.payload).data?.artifact_id === people
    );
  });
  if (noticed) break;
  if (waited === 120) throw new Error("no edit notice of alice's last edit came in 2 minutes");
  await Bun.sleep(1_000);
}

// The last seed step, which the fixture event-log.json reads: an upload, an edit and a title.
const logAfter = await newestEventId(logReads);
const logUpload = await upload(log.key, "log-b.md", "Uploaded after the cursor.\n");
await insert(logDocument, "Second.");
await call("PATCH", `/api/v1/issues/${log.key}`, { title: "Log steps, renamed" }, "agent");

print({
  base,
  contract,
  session: session.id,
  personal_token: personalToken,
  stubs: {
    at: address(stubs.at),
    below: address(stubs.below),
    above: address(stubs.above),
    no_contract: address(stubs.noContract),
    silent,
  },
  issues: {
    tree: tree.key,
    child: child.key,
    main: main.key,
    approved: approved.key,
    earlier: earlier.key,
    requested: requested.key,
    gated: gated.key,
    closed: closed.key,
    people: peopleIssue.key,
    notice: noticeIssue.key,
    answered: answered.key,
    deploy: deploy.key,
    quote_status: quoteStatus.key,
    quotes_only: quotesOnly.key,
    quote_use: quoteUse.key,
    callout: callout.key,
    act_title: actTitle.key,
    untouched: untouched.key,
    plain_title: plainTitle.key,
    capture: capture.key,
    log: log.key,
  },
  asks: { requested: requestedAsk, answered: answeredAsk },
  documents: {
    main_spec: main.spec,
    mixed,
    act,
    quotes,
    kernel,
    priorities,
    verification,
    slugged,
    approved_spec: approved.spec,
    approved_verification: approvedVerification,
    earlier: earlierDocument,
    requested_spec: requested.spec,
    gated_spec: gated.spec,
    closed: closedDocument,
    people,
    notice,
    answered_spec: answered.spec,
    deploy: deployDocument,
    quote_status: quoteStatusDocument,
    quotes_only: quotesOnlyDocument,
    quote_use: quoteUseDocument,
    callout: calloutDocument,
    untouched: untouchedDocument,
    seed_notes: seedNotes,
    race,
    notices,
    notice_status: noticeStatus,
    capture: captureDocument,
    log_a: logDocument,
    log_b: logUpload,
  },
  log_after: logAfter,
  run_flags: [
    "--tree",
    tree.key,
    "--include-closed",
    "--daemon-state",
    `GIVEN=${address(stubs.at)}`,
    "--daemon-state",
    `SILENT=${silent}`,
    "--no-daemon",
    "NONE",
    "--state-document",
    "dispatch://SEED/artifact/rewrite-state-md",
  ],
});

// The stub daemons serve until the seed is stopped.
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => {
    for (const server of Object.values(stubs)) server.stop(true);
    process.exit(0);
  });
}
await new Promise<never>(() => undefined);
