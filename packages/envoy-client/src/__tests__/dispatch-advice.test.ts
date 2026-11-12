import { beforeEach, describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import * as path from "node:path";
import type {
  DocumentWriteAdvice,
  MovedApprovalRequest,
  SearchAsk,
  Suggestions,
  SupersededApprovalGate,
  WriteAdvice,
  WriteSuggestion,
} from "@legion/contracts";
import { bearerCredential } from "../bearer";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool, resetAdviceMemory } from "../dispatch-execute";

/** A write's advice as the route table states it: an issue write answers `WriteAdvice`, a
 *  project document's write the decision-block half alone. */
type AdviceFixture = WriteAdvice | DocumentWriteAdvice;

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};
const issueSuffix =
  "(not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)";

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function repoExec(): ExecFn {
  return async (file, args) => {
    if (file === "jj" && args.join(" ") === "git remote list") {
      return { stdout: "origin https://github.com/owner/repo.git\n" };
    }
    throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
  };
}

function withAdvice<T extends Record<string, unknown>>(
  body: T,
  advice: AdviceFixture | undefined
): T & { advice?: AdviceFixture } {
  return advice === undefined ? body : { ...body, advice };
}

/** A write the tool makes end to end. `requestPaths`, when a row passes one, collects the path
 *  of every request the whole run made, so a row can hold what the run did NOT read. */
async function executeWrite(
  tool: string,
  args: Record<string, unknown>,
  advice?: AdviceFixture,
  requestPaths?: string[]
) {
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const target = new URL(String(url));
    requestPaths?.push(target.pathname);
    const method = init?.method ?? "GET";
    const body = init?.body === undefined ? {} : JSON.parse(String(init.body));

    if (target.pathname === "/api/v1/issues" && method === "POST") {
      return response(
        withAdvice(
          {
            key: "DSP-42",
            title: "Created issue",
            project: "DSP",
            components: {
              mode: "inherit",
              ids: [],
              unknown: [],
              reason: null,
              inherited_from: null,
            },
          },
          advice
        ),
        201
      );
    }
    if (target.pathname === "/api/v1/projects/DSP/architecture-sources" && method === "GET") {
      // No source configured is the list's own answer, not a refusal.
      return new Response("[]", { headers: { "Content-Type": "application/json" } });
    }
    // The two reads an upload makes before it sends: what the issue holds, which holds no
    // document of the uploaded name here, and the markdown the store would hold for the text.
    if (target.pathname === "/api/v1/issues/DSP-42/artifacts" && method === "GET") {
      return response([]);
    }
    if (target.pathname === "/api/v1/markdown/round-trip" && method === "POST") {
      return response({ markdown: body.markdown });
    }
    if (target.pathname === "/api/v1/issues/DSP-42/artifacts" && method === "POST") {
      return response(
        withAdvice(
          {
            artifact: {
              id: "artifact-42",
              issue_key: "DSP-42",
              project: "DSP",
              name: String(body.name),
              slug: "spec",
              primary: body.name === "spec.md",
            },
            version: { number: 1 },
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/issues/DSP-42/messages" && method === "POST") {
      return response(withAdvice({ id: "message-1", issue_key: "DSP-42" }, advice));
    }
    if (target.pathname === "/api/v1/issues/DSP-42/comments" && method === "POST") {
      return response(
        withAdvice(
          {
            id: "comment-1",
            issue_key: "DSP-42",
            ask_id: body.ask_id ?? null,
            reply_to: null,
            turn: body.ask_id === undefined ? null : "human",
            ask_waiting_on: body.ask_id === undefined ? undefined : "human",
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/projects/DSP/artifacts" && method === "POST") {
      return response(
        withAdvice(
          {
            artifact: {
              id: "artifact-42",
              issue_key: null,
              project: "DSP",
              name: String(body.name),
              slug: "spec",
              primary: body.name === "spec.md",
            },
            version: { number: 1 },
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/projects/DSP/artifacts" && method === "GET")
      return response([]);
    if (target.pathname === "/api/v1/projects/DSP/artifacts/spec" && method === "GET") {
      return response({
        id: "artifact-42",
        issue_key: null,
        project: "DSP",
        ref_key: "DSP/spec",
        slug: "spec",
        name: "spec.md",
        kind: "doc",
        primary: true,
        created_by: { kind: "session", id: "session-1" },
        created_at: "2026-09-20T00:00:00Z",
        versions: [],
      });
    }
    if (target.pathname === "/api/v1/artifacts/artifact-42/comments" && method === "POST") {
      return response(
        withAdvice(
          {
            id: "comment-1",
            issue_key: null,
            ask_id: body.ask_id ?? null,
            reply_to: null,
            turn: body.ask_id === undefined ? null : "human",
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/issues/DSP-42/asks" && method === "POST") {
      return response(
        withAdvice(
          {
            id: "ask-new",
            issue_key: "DSP-42",
            question: body.question,
            urgency: body.urgency ?? "med",
          },
          advice
        )
      );
    }
    if (target.pathname === "/api/v1/issues/DSP-42" && method === "GET") {
      return response({
        key: "DSP-42",
        title: "Original title",
        status: "in_progress",
        labels: [],
        route: null,
        parent: null,
        external_links: [],
        primary_artifact_id: "artifact-42",
        artifacts: [
          {
            id: "artifact-42",
            issue_key: "DSP-42",
            project: "DSP",
            name: "spec.md",
            slug: "spec",
            primary: true,
          },
        ],
      });
    }
    if (target.pathname === "/api/v1/artifacts/artifact-42/edits" && method === "POST") {
      return response(withAdvice({ applied: 1, version: null, token: "sha256:edited" }, advice));
    }
    if (target.pathname === "/api/v1/issues/DSP-42" && method === "PATCH") {
      return response(
        withAdvice(
          {
            key: "DSP-42",
            title: body.title ?? "Original title",
            status: body.status ?? "in_progress",
            labels: [],
            route: null,
            parent: null,
            external_links: [],
          },
          advice
        )
      );
    }
    throw new Error(`unexpected request: ${method} ${target.pathname}`);
  };

  return executeDispatchTool({
    tool,
    args,
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-1",
    config,
    env: {},
    exec: repoExec(),
    fetchImpl: fetchImpl as typeof fetch,
    // A session the listener records with no topics: every write names its owner.
    subscriptions: async () => [],
  });
}

function advice(overrides: Partial<WriteAdvice> = {}): WriteAdvice {
  return {
    decision_blocks: 0,
    decision_blocks_added: 0,
    issue_status: "in_progress",
    session_writes_since_human: 0,
    your_open_asks: [],
    ...overrides,
  };
}

beforeEach(() => resetAdviceMemory());

describe("Dispatch write advice", () => {
  const zeroBlocks =
    "This spec holds no ask blocks. A person does not watch your chat. They see that you need them only through an open ask or a spec that waits for their approval. If the design needs a person's choice, add an ask block. If it does not, request approval with `dispatch request-approval`.";

  test.each([
    ["dispatch_issue", { project: "DSP", title: "Created issue", spec: "# Spec\n" }],
    ["dispatch_artifact", { issue: "DSP-42", name: "spec.md", content: "# Spec\n" }],
  ])("%s renders the decision-block pointer for a primary spec", async (tool, args) => {
    const rawAdvice = advice({ decision_blocks: 0 });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(zeroBlocks);
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("renders only decision-block advice for a project document", async () => {
    const rawAdvice: DocumentWriteAdvice = { decision_blocks: 0, decision_blocks_added: 0 };
    const result = await executeWrite(
      "dispatch_artifact",
      { project: "DSP", name: "spec.md", content: "# Spec\n" },
      rawAdvice
    );

    expect(result.text).toEndWith(zeroBlocks);
    expect(result.text).not.toContain("messages on DSP");
    expect(result.text).not.toContain("still in triage");
    expect(result.text).not.toContain("still have an open ask");
    expect(result.details.advice).toEqual(rawAdvice);
  });
  test.each([
    [
      "a populated issue spec",
      "dispatch_issue",
      { project: "DSP", title: "Created issue", spec: "# Spec\n" },
      advice({ decision_blocks: 1 }),
    ],
    [
      "a non-primary artifact",
      "dispatch_artifact",
      { issue: "DSP-42", name: "notes.md", content: "# Notes\n" },
      advice({ decision_blocks: 0 }),
    ],
  ])("does not render decision-block advice for %s", async (_case, tool, args, rawAdvice) => {
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).not.toContain("holds no ask blocks");
    expect(result.details.advice).toEqual(rawAdvice);
  });

  // A spec whose questions are text says so before the count, which alone reads as a spec that
  // needs no decision (LEGION-416); a non-primary document gets the same line, and no count.
  test.each([
    [
      "a primary spec",
      "dispatch_issue",
      { project: "DSP", title: "Created issue", spec: "# Spec\n" },
      true,
    ],
    [
      "a non-primary artifact",
      "dispatch_artifact",
      { issue: "DSP-42", name: "notes.md", content: "# Notes\n" },
      false,
    ],
  ])("names typed-block openings stored as text in %s", async (_case, tool, args, primary) => {
    const rawAdvice = advice({
      decision_blocks: 0,
      unparsed_openers: {
        count: 4,
        examples: ['Intro 27::::ask{urgency="med"}', '…unmeasured. 57::::ask{urgency="low"}'],
      },
    });
    const result = await executeWrite(tool, args, rawAdvice);

    const openers =
      '4 typed-block openings in this document are text, not blocks: "Intro 27::::ask{urgency=\\"med\\"}", "…unmeasured. 57::::ask{urgency=\\"low\\"}". An opening like `:::ask{…}` makes a block only as a line of its own, so as text it asks nobody. Mentioning the syntax on purpose? Put it in code. See the `dispatch-writing-specs` skill, "Questions".';
    expect(result.text).toContain(openers);
    if (primary) {
      expect(result.text).toEndWith(`${openers}\n${zeroBlocks}`);
    } else {
      expect(result.text).toEndWith(openers);
    }
    expect(result.details.advice).toEqual(rawAdvice);
  });
  test("handles decision-only advice on a project-document ask reply", async () => {
    const ask = "01234567-0000-4000-8000-000000000042";
    const rawAdvice: DocumentWriteAdvice = { decision_blocks: 0, decision_blocks_added: 0 };
    const result = await executeWrite(
      "dispatch_comment",
      {
        project: "DSP",
        artifact: "spec",
        body: "Use JSON.",
        reply_to_ask: ask,
      },
      rawAdvice
    );

    expect(result.text).toContain(`Replied on ask ${ask}`);
    expect(result.text).not.toContain("messages on DSP");
    expect(result.text).not.toContain("still in triage");
    expect(result.text).not.toContain("still have an open ask");
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test.each([
    ["dispatch_message", { issue: "DSP-42", body: "Progress" }],
    ["dispatch_comment", { issue: "DSP-42", body: "Progress" }],
    ["dispatch_ask", { issue: "DSP-42", question: "What should we do?" }],
  ])("%s renders the cadence pointer after three session writes", async (tool, args) => {
    const rawAdvice = advice({ session_writes_since_human: 3 });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(
      'Posted. You\'ve sent 3 messages on DSP-42 with no human response. Progress ledger or scratchpad? If so, stop. See the `dispatch` skill, "Structure over stream".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("escalates the cadence wording at six writes", async () => {
    const rawAdvice = advice({ session_writes_since_human: 6 });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Progress" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'Posted. You\'ve sent 6 messages on DSP-42 with no human response. Stop posting here until a human replies. Their reply ends this: post then what you held back. See the `dispatch` skill, "Structure over stream".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("renders the triage pointer once per issue per process", async () => {
    const rawAdvice = advice({ issue_status: "triage" });
    const first = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "First" },
      rawAdvice
    );
    const second = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Second" },
      rawAdvice
    );
    const line =
      'DSP-42 is still in triage — nobody can see its development status. See the `dispatch` skill, "Issue status is yours to move".';

    expect(first.text).toEndWith(line);
    expect(second.text).toBe(
      `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`
    );
    expect(first.details.advice).toEqual(rawAdvice);
    expect(second.details.advice).toEqual(rawAdvice);
  });

  test("drops the triage pointer when Legion owns the issue's status", async () => {
    const rawAdvice = advice({ issue_status: "triage", legion_managed_status: true });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "First" },
      rawAdvice
    );

    expect(result.text).toBe(
      `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`
    );
    expect(result.details.advice).toEqual(rawAdvice);
    // The memo stays unmarked, so the line is still available if the issue leaves Legion's hands.
    const later = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Second" },
      advice({ issue_status: "triage" })
    );
    expect(later.text).toContain("is still in triage");
  });

  // The absent case is the rest of this file's fixtures, which carry no field and print the line.
  test("renders the triage pointer when legion_managed_status is false", async () => {
    const rawAdvice = advice({ issue_status: "triage", legion_managed_status: false });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "First" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'DSP-42 is still in triage — nobody can see its development status. See the `dispatch` skill, "Issue status is yours to move".'
    );
  });

  test("caps open-ask pointers at two and truncates questions to 80 characters", async () => {
    const rawAdvice = advice({
      your_open_asks: [
        { id: "ask-1", question: "A".repeat(81) },
        { id: "ask-2", question: "Second question" },
        { id: "ask-3", question: "Third question" },
      ],
    });
    const result = await executeWrite(
      "dispatch_message",
      { issue: "DSP-42", body: "Update" },
      rawAdvice
    );

    expect(result.text).toBe(
      [
        `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`,
        `You still have an open ask on DSP-42: "${"A".repeat(80)}" (ask-1). Still needed? See the \`dispatch\` skill, "Close what you opened".`,
        'You still have an open ask on DSP-42: "Second question" (ask-2). Still needed? See the `dispatch` skill, "Close what you opened".',
      ].join("\n")
    );
    expect(result.text).not.toContain("ask-3");
    expect(result.details.advice).toEqual(rawAdvice);
  });
  test.each([
    ["dispatch_comment", { issue: "DSP-42", body: "Update" }],
    [
      "dispatch_doc_edit",
      {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
    ],
    ["dispatch_issue_update", { issue: "DSP-42", title: "Renamed" }],
  ])("%s renders an open-ask pointer", async (tool, args) => {
    const rawAdvice = advice({
      your_open_asks: [{ id: "ask-1", question: "Choose the format?" }],
    });
    const result = await executeWrite(tool, args, rawAdvice);

    expect(result.text).toEndWith(
      'You still have an open ask on DSP-42: "Choose the format?" (ask-1). Still needed? See the `dispatch` skill, "Close what you opened".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("a reply to another ask still renders the caller's open ask", async () => {
    const replyTarget = "01234567-0000-4000-8000-000000000042";
    const rawAdvice = advice({
      your_open_asks: [
        { id: "aaaaaaaa-0000-4000-8000-000000000042", question: "Choose the format?" },
      ],
    });
    const result = await executeWrite(
      "dispatch_comment",
      { issue: "DSP-42", body: "JSON.", reply_to_ask: replyTarget },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'You still have an open ask on DSP-42: "Choose the format?" (aaaaaaaa-0000-4000-8000-000000000042). Still needed? See the `dispatch` skill, "Close what you opened".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("an ask reply is exempt from cadence and its own open-ask pointer", async () => {
    const ownAsk = "01234567-0000-4000-8000-000000000042";
    const rawAdvice = advice({
      session_writes_since_human: 3,
      your_open_asks: [{ id: ownAsk, question: "Choose the format?" }],
    });
    const result = await executeWrite(
      "dispatch_comment",
      { issue: "DSP-42", body: "JSON.", reply_to_ask: ownAsk },
      rawAdvice
    );

    expect(result.text).toBe(
      `Replied on ask ${ownAsk} (comment comment-1; ask now waiting on human). ` +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>"
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("a status-setting issue update is exempt from the triage pointer", async () => {
    const rawAdvice = advice({ issue_status: "triage" });
    const result = await executeWrite(
      "dispatch_issue_update",
      { issue: "DSP-42", status: "testing" },
      rawAdvice
    );

    expect(result.text).toBe(`DSP-42: status in_progress -> testing ${issueSuffix}`);
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test("a non-status issue update renders the triage pointer", async () => {
    const rawAdvice = advice({ issue_status: "triage" });
    const result = await executeWrite(
      "dispatch_issue_update",
      { issue: "DSP-42", title: "Renamed" },
      rawAdvice
    );

    expect(result.text).toEndWith(
      'DSP-42 is still in triage — nobody can see its development status. See the `dispatch` skill, "Issue status is yours to move".'
    );
    expect(result.details.advice).toEqual(rawAdvice);
  });

  test.each([
    // An issue or an ask from a server that sends no advice gets the not-made form: that server
    // ran no related-item check the reader can rely on.
    [
      "dispatch_issue",
      { project: "DSP", title: "Created issue" },
      `Created DSP-42: Created issue → http://dispatch.test/issues/DSP-42 ${issueSuffix}\n` +
        "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
      { issue: "DSP-42" },
    ],
    [
      "dispatch_artifact",
      { issue: "DSP-42", name: "spec.md", content: "# Spec\n" },
      `Uploaded spec.md as version 1 (artifact slug spec; dispatch://DSP-42/artifact/spec) ${issueSuffix}`,
      { issue: "DSP-42", artifact: "artifact-42", version: 1 },
    ],
    [
      "dispatch_message",
      { issue: "DSP-42", body: "Progress" },
      `Posted message message-1 (dispatch://DSP-42/message/message-1) ${issueSuffix}`,
      { issue: "DSP-42", message: "message-1" },
    ],

    [
      "dispatch_comment",
      { issue: "DSP-42", body: "Progress" },
      `Posted comment comment-1 ${issueSuffix}`,
      { issue: "DSP-42", comment: "comment-1" },
    ],
    [
      "dispatch_ask",
      { issue: "DSP-42", question: "What should we do?" },
      "Asked ask-new on DSP-42 (urgency med): What should we do?\n" +
        "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'What should'\n" +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>",
      { issue: "DSP-42", ask: "ask-new", follows: { ask: "ask-new" } },
    ],
    [
      "dispatch_doc_edit",
      {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
      `Applied 1 ops (no new version) ${issueSuffix}\nDocument token: sha256:edited`,
      { issue: "DSP-42", applied: 1, token: "sha256:edited" },
    ],
    [
      "dispatch_issue_update",
      { issue: "DSP-42", title: "Renamed" },
      `DSP-42: title "Renamed" ${issueSuffix}`,
      { issue: "DSP-42", status: "in_progress", external_links: [] },
    ],
  ])("%s renders its result when advice is absent", async (tool, args, text, details) => {
    const result = await executeWrite(tool, args);

    expect(result.text).toBe(text);
    expect(result.details).toEqual(details);
    expect(result.details).not.toHaveProperty("advice");
  });
});

const createdLine = `Created DSP-42: Created issue → http://dispatch.test/issues/DSP-42 ${issueSuffix}`;
const askedLine = "Asked ask-new on DSP-42 (urgency med): Should the billing service retry?";
const followsLine =
  "You follow this ask: its answer and replies reach you directly. " +
  "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>";

function issueItem(key: string, title: string, status = "backlog"): WriteSuggestion {
  return {
    kind: "issue",
    owner: { kind: "issue", key, title, status },
    id: key,
    snippet: title,
    href: `/issues/${key}`,
  };
}

function askItem(key: string, id: string, ask?: SearchAsk): WriteSuggestion {
  return {
    kind: "ask",
    owner: { kind: "issue", key, title: "Billing retries", status: "in_progress" },
    id,
    snippet: "Should billing retry?",
    href: `/issues/${key}#ask-${id}`,
    ...(ask === undefined ? {} : { ask }),
  };
}

/** An answered question of another project, as the server offers it: an ask item whose owner
 *  names the project the item is in. */
function otherProjectAsk(
  key: string,
  id: string,
  project: string,
  answer: string
): WriteSuggestion {
  return {
    kind: "ask",
    owner: { kind: "issue", key, title: "Webhook retries", status: "done", project },
    id,
    snippet: "Should webhook deliveries back off?",
    href: `/issues/${key}#ask-${id}`,
    ask: {
      state: "answered",
      answer: { user: "sami", selected: [], text: answer, at: "2026-09-12T13:20:00Z" },
    },
  };
}

async function createIssue(suggestions: Suggestions | undefined, extra: Partial<WriteAdvice> = {}) {
  return executeWrite(
    "dispatch_issue",
    { project: "DSP", title: "Created issue" },
    advice({ ...(suggestions === undefined ? {} : { suggestions }), ...extra })
  );
}

describe("the related-item check", () => {
  test("dispatch_issue lists what it found, and how it searched, right under the created line", async () => {
    const suggestions: Suggestions = {
      outcome: "found",
      search: "meaning_and_keywords",
      related: [issueItem("DSP-7", "Existing issue", "triage")],
    };
    const result = await createIssue(suggestions);

    expect(result.text).toBe(
      [
        createdLine,
        "Related items (searched by meaning and keywords):",
        "- DSP-7 [triage] Existing issue → http://dispatch.test/issues/DSP-7",
      ].join("\n")
    );
    expect(result.details.advice).toEqual(advice({ suggestions }));
  });

  test("a keywords-only search names why in its header", async () => {
    const result = await createIssue({
      outcome: "found",
      search: "keywords_only",
      search_note: "meaning search is off on this server",
      related: [issueItem("DSP-7", "Existing issue")],
    });

    expect(result.text.split("\n")[1]).toBe(
      "Related items (searched by keywords only: meaning search is off on this server):"
    );
  });

  test.each([
    [
      { search: "meaning_and_keywords" } as const,
      "Related-item check searched (meaning and keywords) and found nothing.",
    ],
    [
      { search: "keywords_only", search_note: "meaning search is off on this server" } as const,
      "Related-item check searched (keywords only: meaning search is off on this server) and found nothing.",
    ],
  ])("a search that found nothing says so: %p", async (mode, line) => {
    const result = await createIssue({ outcome: "none", related: [], ...mode });

    expect(result.text).toBe([createdLine, line].join("\n"));
  });

  test("a check not made says why and names the search to run, quoting the server's words for a shell", async () => {
    const result = await createIssue({
      outcome: "not_made",
      related: [],
      missing: "the search behind it did not answer within 5s",
      search_query: "carrier's webhook retries",
    });

    expect(result.text).toBe(
      [
        createdLine,
        "No related-item check was made: the search behind it did not answer within 5s. Search before you rely on this being new: dispatch search --project DSP --query 'carrier'\\''s webhook retries'",
      ].join("\n")
    );
  });

  test.each([
    [
      "no check result",
      undefined,
      "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
    ],
    [
      "a missing reason",
      { related: [], missing: "the search behind it did not answer within 300ms" },
      "No related-item check was made: the search behind it did not answer within 300ms. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
    ],
    [
      "items and no outcome",
      { related: [issueItem("DSP-7", "Existing issue")] },
      "Related items:\n- DSP-7 [backlog] Existing issue → http://dispatch.test/issues/DSP-7",
    ],
    [
      "no items and no outcome",
      { related: [] },
      "No related-item check was made: the server did not say whether it searched. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
    ],
  ] as const)("an older server's %s reads as one of the forms", async (_shape, suggestions, form) => {
    const result = await createIssue(suggestions as Suggestions | undefined);

    expect(result.text).toBe([createdLine, form].join("\n"));
  });

  test("dispatch_issue naming an external issue Dispatch already holds does not say it created one", async () => {
    // Dispatch answers an external reference it already holds with 200 (a new issue is 201), the
    // existing issue, and a check not made because nothing was created: the head server's answer.
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      const method = init?.method ?? "GET";
      if (target.pathname === "/api/v1/issues" && method === "POST") {
        return new Response(
          JSON.stringify({
            key: "DSP-42",
            title: "External issue",
            project: "DSP",
            status: "triage",
            components: {
              mode: "inherit",
              ids: [],
              unknown: [],
              reason: null,
              inherited_from: null,
            },
            advice: {
              suggestions: {
                outcome: "not_made",
                related: [],
                missing: "Dispatch already holds this issue; nothing was created",
              },
            },
          }),
          { status: 200, headers: { "Content-Type": "application/json" } }
        );
      }
      if (target.pathname === "/api/v1/projects/DSP/architecture-sources" && method === "GET") {
        return new Response("[]", { headers: { "Content-Type": "application/json" } });
      }
      throw new Error(`unexpected request: ${method} ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "DSP", title: "External issue", external: "owner/repo#7" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec(),
      fetchImpl: fetchImpl as typeof fetch,
      subscriptions: async () => [],
    });

    expect(result.text).toContain("DSP-42");
    expect(result.text).not.toMatch(/^Created DSP-42/m);
  });

  test.each([
    ["ada@example.com", "; assigned to ada@example.com"],
    [null, "; unassigned"],
  ] as const)("an external issue Dispatch already holds (assignee %p) says nothing was created and prints no related-item check", async (assignee, said) => {
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      const method = init?.method ?? "GET";
      if (target.pathname === "/api/v1/issues" && method === "POST") {
        return response({
          key: "DSP-42",
          title: "External issue",
          project: "DSP",
          assignee,
          components: {
            mode: "inherit",
            ids: [],
            unknown: [],
            reason: null,
            inherited_from: null,
          },
          advice: advice({
            suggestions: {
              outcome: "not_made",
              related: [],
              missing: "Dispatch already holds this issue; nothing was created",
            },
          }),
        });
      }
      if (target.pathname === "/api/v1/projects/DSP/architecture-sources" && method === "GET") {
        return response([]);
      }
      throw new Error(`unexpected request: ${method} ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "DSP", title: "External issue", external: "owner/repo#7" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec(),
      fetchImpl: fetchImpl as typeof fetch,
      subscriptions: async () => [],
    });

    expect(result.text).toBe(
      `Dispatch already holds DSP-42: External issue → http://dispatch.test/issues/DSP-42; nothing was created${said} ${issueSuffix}`
    );
  });

  test("an ask item says where the ask stands", async () => {
    const answer = (selected: string[], text: string | null) => ({
      user: "alice",
      selected,
      text,
      at: "2026-09-12T13:20:00Z",
    });
    const resolution = {
      kind: "retracted" as const,
      reason: "Superseded.",
      actor: { kind: "user" as const, id: "bob" },
      at: "2026-09-13T08:00:00Z",
    };
    const result = await createIssue({
      outcome: "found",
      search: "meaning_and_keywords",
      related: [
        askItem("DSP-1", "a1", { state: "open" }),
        askItem("DSP-2", "a2", {
          state: "answered",
          answer: answer(["Exponential", "Capped"], "Back off.\nCap at an hour."),
        }),
        askItem("DSP-3", "a3", { state: "answered", answer: answer([], "Never retry.") }),
        askItem("DSP-4", "a4", { state: "resolved", resolution }),
        askItem("DSP-5", "a5", {
          state: "resolved",
          resolution: { ...resolution, kind: "resolved" },
          answer: answer(["Linear"], null),
        }),
        askItem("DSP-6", "a6"),
      ],
    });

    expect(result.text).toBe(
      [
        createdLine,
        "Related items (searched by meaning and keywords):",
        "- DSP-1 [in_progress] Billing retries → http://dispatch.test/issues/DSP-1#ask-a1",
        "  Open: no answer yet.",
        "- DSP-2 [in_progress] Billing retries → http://dispatch.test/issues/DSP-2#ask-a2",
        "  Answered by alice at 2026-09-12T13:20:00Z · options chosen: Exponential, Capped",
        "  Answer text: Back off.",
        "    Cap at an hour.",
        "- DSP-3 [in_progress] Billing retries → http://dispatch.test/issues/DSP-3#ask-a3",
        "  Answered by alice at 2026-09-12T13:20:00Z · options chosen: none",
        "  Answer text: Never retry.",
        "- DSP-4 [in_progress] Billing retries → http://dispatch.test/issues/DSP-4#ask-a4",
        "  Closed with no answer: retracted by user bob at 2026-09-13T08:00:00Z: Superseded.",
        "- DSP-5 [in_progress] Billing retries → http://dispatch.test/issues/DSP-5#ask-a5",
        "  Closed after an answer: resolved by user bob at 2026-09-13T08:00:00Z: Superseded.",
        "  Last answer by alice at 2026-09-12T13:20:00Z · options chosen: Linear",
        "  Answer text: none",
        // An older server sends no state, and the item says so, as a search hit does.
        "- DSP-6 [in_progress] Billing retries → http://dispatch.test/issues/DSP-6#ask-a6",
        "  Answer state not reported by this Dispatch; open the link to read the ask.",
      ].join("\n")
    );
  });

  test("a forced create names the titles it went past after the form", async () => {
    const result = await createIssue({
      outcome: "not_made",
      related: [],
      missing: "the search behind it failed",
      title_matches: [
        {
          key: "DSP-3",
          title: "Created issue",
          status: "todo",
          snippet: "Created issue",
          shared_terms: 2,
          href: "/issues/DSP-3",
        },
      ],
    });

    expect(result.text).toBe(
      [
        createdLine,
        "No related-item check was made: the search behind it failed. Search before you rely on this being new: dispatch search --project DSP --query 'Created issue'",
        "Near copy you forced past: DSP-3 [todo] Created issue → http://dispatch.test/issues/DSP-3",
      ].join("\n")
    );
  });

  test("a link that does not parse prints as the server sent it", async () => {
    const result = await createIssue({
      outcome: "found",
      search: "meaning_and_keywords",
      related: [{ ...issueItem("DSP-7", "Existing issue"), href: "http://[" }],
    });

    expect(result.text.split("\n")[2]).toBe("- DSP-7 [backlog] Existing issue → http://[");
  });

  test("advice that cannot be shown leaves the created line and the form, and says so", async () => {
    const result = await createIssue(
      { outcome: "none", search: "meaning_and_keywords", related: [] },
      // A malformed field makes the advice renderer throw. The route table states `examples` as
      // a list or null, so neither is malformed now; a string in its place is.
      { unparsed_openers: { count: 2, examples: "two" as unknown as string[] } }
    );
    const lines = result.text.split("\n");

    expect(lines.slice(0, 2)).toEqual([
      createdLine,
      "Related-item check searched (meaning and keywords) and found nothing.",
    ]);
    expect(lines[2]).toStartWith("Other advice could not be shown: ");
    expect(lines).toHaveLength(3);
  });

  test("dispatch_ask prints the form between the asked line and the follow line", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      { issue: "DSP-42", question: "Should the billing service retry?" },
      advice({
        suggestions: {
          outcome: "found",
          search: "meaning_and_keywords",
          related: [issueItem("DSP-9", "Billing retries", "in_progress")],
        },
      })
    );

    expect(result.text).toBe(
      [
        askedLine,
        "Related items (searched by meaning and keywords):",
        "- DSP-9 [in_progress] Billing retries → http://dispatch.test/issues/DSP-9",
        followsLine,
      ].join("\n")
    );
  });

  test("dispatch_ask still ends with the cadence line", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      { issue: "DSP-42", question: "Should the billing service retry?" },
      advice({
        session_writes_since_human: 3,
        suggestions: {
          outcome: "not_made",
          related: [],
          missing: "the search behind it did not answer within 300ms",
        },
      })
    );
    const lines = result.text.split("\n");

    expect(lines.slice(0, 3)).toEqual([
      askedLine,
      "No related-item check was made: the search behind it did not answer within 300ms. Search before you rely on this being new: dispatch search --project DSP --query 'Should billing service'",
      followsLine,
    ]);
    expect(lines.at(-1)).toStartWith("Posted. You've sent 3 messages on DSP-42");
  });
});

// The review an issue's specification already carries when a question is opened on it
// (WriteAdvice.standing_review). The server decides whether a review stands; the command decides
// what it reads like and where it sits.
describe("dispatch_ask's standing review", () => {
  const changedSince = "Say what changed since that review in a reply on this ask.";
  const askArgs = { issue: "DSP-42", question: "Should the billing service retry?" };
  // Every question runs the related-item check too, so its line sits between the asked line and
  // the review in each case below; here it finds nothing.
  const noRelatedLine = "Related-item check searched (meaning and keywords) and found nothing.";
  const emptyCheck: Suggestions = { outcome: "none", search: "meaning_and_keywords", related: [] };

  async function askWithReview(review: NonNullable<WriteAdvice["standing_review"]>) {
    return await executeWrite(
      "dispatch_ask",
      askArgs,
      advice({ suggestions: emptyCheck, standing_review: review })
    );
  }

  test("an approval names the person, the version, the time and the reason, between the asked line and the follow line", async () => {
    const result = await askWithReview({
      by: "alice",
      at: "2026-09-12T13:20:00Z",
      state: "approved",
      version: 4,
      reason: "The rollback path is named.",
    });

    expect(result.text).toBe(
      [
        askedLine,
        noRelatedLine,
        'alice approved this issue\'s spec at version 4 on 2026-09-12T13:20:00Z: "The rollback path is named."',
        changedSince,
        followsLine,
      ].join("\n")
    );
  });

  test("a request for changes says so", async () => {
    const result = await askWithReview({
      by: "alice",
      at: "2026-09-12T13:20:00Z",
      state: "changes_requested",
      version: 4,
      reason: "The rollback path is missing.",
    });

    expect(result.text.split("\n")[2]).toBe(
      'alice asked for changes on this issue\'s spec at version 4 on 2026-09-12T13:20:00Z: "The rollback path is missing."'
    );
  });

  test("a review with no reason says so rather than quoting nothing", async () => {
    const result = await askWithReview({
      by: "alice",
      at: "2026-09-12T13:20:00Z",
      state: "approved",
      version: 4,
    });
    const lines = result.text.split("\n");

    expect(lines[2]).toBe(
      "alice approved this issue's spec at version 4 on 2026-09-12T13:20:00Z, and gave no reason."
    );
    expect(lines[2]).not.toContain('""');
  });

  test("the reason is printed as the server cut it, with no second cut here", async () => {
    // The server cuts the reason by characters (firstCharacters in write_advice.go), which is
    // where the 500 is decided and proven. Cutting again here would count UTF-16 code units and
    // could split a character the server kept whole, so the line prints what it was sent.
    const reason = "é".repeat(500);
    const result = await askWithReview({
      by: "alice",
      at: "2026-09-12T13:20:00Z",
      state: "approved",
      version: 4,
      reason,
    });

    expect(result.text.split("\n")[2]).toBe(
      `alice approved this issue's spec at version 4 on 2026-09-12T13:20:00Z: "${reason}"`
    );
  });

  test("a carried review names the version its carry reason gives, the carry's own time, and no reason", async () => {
    const result = await askWithReview({
      by: "alice",
      at: "2026-09-14T09:05:00Z",
      state: "approved",
      version: 7,
      // The stored reason is Dispatch's carry sentence, which the line never prints.
      reason:
        "Carried forward from the approval of version 5: the answer to a decision block was the whole change since it.",
      // The version the carry names is what says the review is one, with no second field to
      // disagree with it.
      carried_from_version: 5,
    });
    const lines = result.text.split("\n");

    expect(lines.slice(2, 4)).toEqual([
      "alice's approval of this issue's spec was carried forward from the approval of version 5, and Dispatch recorded that carry on 2026-09-14T09:05:00Z.",
      changedSince,
    ]);
    expect(result.text).not.toContain("version 7");
    expect(result.text).not.toContain("the answer to a decision block");
  });

  test("the cadence line stays last", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      askArgs,
      advice({
        session_writes_since_human: 3,
        suggestions: emptyCheck,
        standing_review: {
          by: "alice",
          at: "2026-09-12T13:20:00Z",
          state: "approved",
          version: 4,
          reason: "The rollback path is named.",
        },
      })
    );
    const lines = result.text.split("\n");

    expect(lines.slice(0, 5)).toEqual([
      askedLine,
      noRelatedLine,
      'alice approved this issue\'s spec at version 4 on 2026-09-12T13:20:00Z: "The rollback path is named."',
      changedSince,
      followsLine,
    ]);
    expect(lines.at(-1)).toStartWith("Posted. You've sent 3 messages on DSP-42");
  });

  test("advice carrying no review prints nothing new", async () => {
    const result = await executeWrite("dispatch_ask", askArgs, advice({ suggestions: emptyCheck }));

    expect(result.text).toBe([askedLine, noRelatedLine, followsLine].join("\n"));
  });
});

describe("dispatch_doc_edit's decision block checks", () => {
  const askBlock =
    ':::ask{urgency="med"}\nShould webhook deliveries back off exponentially?\n:::\n';
  const editArgs = {
    issue: "DSP-42",
    artifact: "spec",
    ops: [{ op: "insert", after: "end", markdown: askBlock }],
  };
  const appliedLine = `Applied 1 ops (no new version) ${issueSuffix}`;

  test("prints each new block's check under its id, after the applied line", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({
        decision_blocks_added: 2,
        decision_block_checks: [
          {
            block_id: "ask-block-1",
            question: "Should webhook deliveries back off exponentially?",
            suggestions: {
              outcome: "found",
              search: "keywords_only",
              search_note: "meaning search is off on this server",
              related: [issueItem("DSP-7", "Webhook retries")],
            },
          },
          {
            block_id: "ask-block-2",
            question: "Which queue holds them?",
            suggestions: {
              outcome: "not_made",
              related: [],
              missing: "more than five new decision blocks in one edit",
            },
          },
        ],
      })
    );

    expect(result.text).toBe(
      [
        appliedLine,
        "Decision block ask-block-1:",
        "Related items (searched by keywords only: meaning search is off on this server):",
        "- DSP-7 [backlog] Webhook retries → http://dispatch.test/issues/DSP-7",
        "Decision block ask-block-2:",
        "No related-item check was made: more than five new decision blocks in one edit. Search before you rely on this being new: dispatch search --project DSP --query 'Which queue holds'",
        "Document token: sha256:edited",
      ].join("\n")
    );
  });

  test("an older server that only counts the new blocks gets one not-made line, searched by the block's question", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({ decision_blocks_added: 1 })
    );

    expect(result.text).toBe(
      [
        appliedLine,
        "1 new decision block:",
        "No related-item check was made: the server sent no check result. Search before you rely on this being new: dispatch search --project DSP --query 'Should webhook deliveries'",
        "Document token: sha256:edited",
      ].join("\n")
    );
  });

  test("an older server's count with no block text in the edit searches by the issue's title", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      { issue: "DSP-42", artifact: "spec", ops: [{ op: "replace", find: "draft", with: "final" }] },
      advice({ decision_blocks_added: 1 })
    );

    expect(result.text.split("\n")[2]).toEndWith("--query 'Original title'");
  });

  test("an edit that added no block prints no check", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({ decision_blocks_added: 0 })
    );

    expect(result.text).toBe([appliedLine, "Document token: sha256:edited"].join("\n"));
  });
});

// The server sends `superseded_approval` only on the write that replaced a person's approved
// version, so the line is about that one write: what it stored, whose approval it staled, how that
// approval comes back, and what the reader does next. The negative is the ordinary case - a write
// on a document nobody approved, one whose approval was already stale, and one whose version
// carried that approval forward (an answer to a decision block, or text the approved version held
// exactly, an undo among them) all arrive here with the field absent, and print nothing.
//
// The gate clause between the two is printed from `design_gates` alone, the issues whose design
// gate records this document: no field at all (what an older Dispatch answers), an empty list and
// a list that did not read as one all print the line without it.
describe("a document write that superseded a person's approval", () => {
  const stored =
    "Stored version 15, superseding version 14, which alice approved. That approval is stale until someone approves version 15 or a later write stores the text of version 14 again exactly, with no decision block in it decided since.";
  const nextStep =
    "Meant to change the approved design? Request approval with `dispatch request-approval`. Meant a correction beside it? Tell the tree's architect, and keep the next one in a comment or in the plan.";
  const supersededLine = `${stored} ${nextStep}`;
  const superseded = { approved_version: 14, version: 15, approved_by: "alice" };
  const editArgs = {
    issue: "DSP-42",
    artifact: "spec",
    ops: [{ op: "replace", find: "a stale sentence", with: "" }],
  };

  async function editWithGates(design_gates: SupersededApprovalGate[]) {
    return executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({ superseded_approval: { ...superseded, design_gates } })
    );
  }

  test.each([
    ["dispatch_doc_edit", editArgs],
    ["dispatch_artifact", { issue: "DSP-42", name: "spec.md", content: "# Spec\n" }],
  ])("%s names the approval its version superseded, last", async (tool, args) => {
    const result = await executeWrite(tool, args, advice({ superseded_approval: superseded }));

    expect(result.text).toEndWith(supersededLine);
    expect(result.text).not.toContain("design gate");
  });

  test("a record that names no gate prints the line without the clause", async () => {
    const result = await editWithGates([]);

    expect(result.text).toEndWith(supersededLine);
    expect(result.text).not.toContain("design gate");
  });

  test("one gate names its issue and step", async () => {
    const result = await editWithGates([{ issue: "INFRA-1503", step: "spec" }]);

    expect(result.text).toEndWith(
      `${stored} A Legion design gate records this document, so every READY in its tree is refused until that approval comes back: INFRA-1503, step spec. ${nextStep}`
    );
  });

  test("two gates name both trees, in the order the server sent them", async () => {
    const result = await editWithGates([
      { issue: "INFRA-1503", step: "spec" },
      { issue: "INFRA-2200", step: "criteria" },
    ]);

    expect(result.text).toEndWith(
      `${stored} A Legion design gate records this document, so every READY in each tree is refused until that approval comes back: INFRA-1503, step spec; INFRA-2200, step criteria. ${nextStep}`
    );
  });

  test("a write that superseded no approval says nothing about one", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      { issue: "DSP-42", artifact: "spec", ops: [{ op: "replace", find: "draft", with: "final" }] },
      advice({ decision_blocks_added: 0 })
    );

    expect(result.text).not.toContain("design gate");
    expect(result.text).not.toContain("superseding version");
  });
});

// The server sends `moved_approval_request` only on a write whose version took an open approval
// request back from the person it was waiting on: the ask, the issue or project it stands on, and
// every document the request names with the version it asks about beside the version that stands
// now. The line is printed from that report alone, which is why a write that prints it reads no
// ask, and it comes before the superseded-approval line, which stays a write's last advice line.
describe("a document write that moved an open approval request", () => {
  const ask = "01234567-0000-4000-8000-000000000045";
  const specDocument = {
    artifact_id: "artifact-42",
    name: "spec.md",
    primary: true,
    requested_version: 9,
    version: 9,
  };
  const verificationDocument = {
    artifact_id: "artifact-77",
    name: "verification.md",
    primary: false,
    role: "verification",
    requested_version: 16,
    version: 17,
  };
  const notesDocument = {
    artifact_id: "artifact-91",
    name: "notes.md",
    primary: false,
    requested_version: 3,
    version: 4,
  };
  const editArgs = {
    issue: "DSP-42",
    artifact: "spec",
    ops: [{ op: "replace", find: "draft", with: "final" }],
  };
  const uploadArgs = { issue: "DSP-42", name: "spec.md", content: "# Spec\n" };

  function request(overrides: Partial<MovedApprovalRequest> = {}): MovedApprovalRequest {
    return {
      ask_id: ask,
      issue: "DSP-42",
      documents: [specDocument, verificationDocument],
      ...overrides,
    };
  }

  /** The advice of a write that moved `moved`. The spec holds a block of its own, because the
   *  zero-block pointer is another line's state, not this one's. */
  function movedAdvice(moved: MovedApprovalRequest): WriteAdvice {
    return advice({ decision_blocks: 1, moved_approval_request: moved });
  }

  function movedLine(clauses: string, command: string): string {
    return (
      `This write moved the open approval request (ask ${ask}): it waits on an agent now, not ` +
      `on the person, and nothing tells the person. ${clauses}. Once the human has agreed to ` +
      `every point in it, hand the same request back: ${command}. Not yours to hand back? Tell ` +
      "the issue's architect."
    );
  }

  /** The lines `renderAdvice` printed: a write states its own result first - an edit its applied
   *  line and the document token, an upload its stored line - and the advice lines follow. */
  function adviceLines(text: string): string[] {
    return text
      .split("\n")
      .filter(
        (line) =>
          !line.startsWith("Applied ") &&
          !line.startsWith("Uploaded ") &&
          !line.startsWith("Document token: ")
      );
  }

  const verificationMoved = movedLine(
    "verification.md asked about version 16 and is now version 17",
    "dispatch request-approval --issue DSP-42 --include-verification " +
      "--summary '<what changed since version 16>'"
  );

  // The two tools the server sends the report on: an edit of a document and an upload that
  // replaces one. The request names the issue's spec and its Verification document, and the
  // write stored the second: the clause names that document and both its versions, and the
  // document the write left alone is named nowhere.
  test.each([
    ["dispatch_doc_edit", editArgs],
    ["dispatch_artifact", uploadArgs],
  ])("%s names the document that moved, with both its versions", async (tool, args) => {
    const result = await executeWrite(tool, args, movedAdvice(request()));

    expect(adviceLines(result.text)).toEqual([verificationMoved]);
    expect(adviceLines(result.text)[0]).not.toContain("spec.md");
  });

  test("two documents that moved are named in the report's order, in one sentence", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      movedAdvice(request({ documents: [{ ...specDocument, version: 10 }, verificationDocument] }))
    );

    expect(adviceLines(result.text)).toEqual([
      movedLine(
        "spec.md asked about version 9 and is now version 10; " +
          "verification.md asked about version 16 and is now version 17",
        "dispatch request-approval --issue DSP-42 --include-verification " +
          "--summary '<what changed since version 9>'"
      ),
    ]);
  });

  // The call reopens the request's first document, so the owner part of it is that document's
  // place: an issue's primary document is named by its issue alone, every other document by the
  // artifact beside its owner.
  test.each([
    [
      "the issue's primary document",
      "dispatch_doc_edit",
      editArgs,
      movedAdvice(request({ documents: [{ ...specDocument, version: 10 }] })),
      "dispatch request-approval --issue DSP-42 --summary '<what changed since version 9>'",
    ],
    [
      "another document of the issue",
      "dispatch_doc_edit",
      editArgs,
      movedAdvice(request({ documents: [notesDocument] })),
      "dispatch request-approval --issue DSP-42 --artifact artifact-91 " +
        "--summary '<what changed since version 3>'",
    ],
    [
      "a project document",
      "dispatch_artifact",
      { project: "DSP", name: "notes.md", content: "# Notes\n" },
      {
        decision_blocks: 1,
        decision_blocks_added: 0,
        moved_approval_request: {
          ask_id: ask,
          project: "DSP",
          documents: [notesDocument],
        },
      } satisfies DocumentWriteAdvice,
      "dispatch request-approval --project DSP --artifact artifact-91 " +
        "--summary '<what changed since version 3>'",
    ],
  ])("a request whose first document is %s names it that way in the call", async (_case, tool, args, rawAdvice, command) => {
    const result = await executeWrite(tool, args, rawAdvice);

    expect(adviceLines(result.text)[0]).toEndWith(
      `hand the same request back: ${command}. Not yours to hand back? Tell the issue's architect.`
    );
  });

  // `--include-verification` asks for an issue's spec and that issue's Verification document in
  // one card, and the approval route refuses it on every other document. A request over the
  // Verification document alone is one of those, so the call that hands it back names the
  // document and not the flag.
  test("a request over the Verification document alone carries no --include-verification", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      movedAdvice(request({ documents: [verificationDocument] }))
    );

    expect(adviceLines(result.text)).toEqual([
      movedLine(
        "verification.md asked about version 16 and is now version 17",
        "dispatch request-approval --issue DSP-42 --artifact artifact-77 " +
          "--summary '<what changed since version 16>'"
      ),
    ]);
  });

  // The placeholder names the floor of what the summary has to cover: the version the moved
  // document was handed over at, which on this request is the Verification document's 16 and not
  // the spec's 9.
  test("the --summary floor is the version the document that moved was handed over at", async () => {
    const result = await executeWrite("dispatch_doc_edit", editArgs, movedAdvice(request()));

    expect(adviceLines(result.text)[0]).toEndWith(
      "--summary '<what changed since version 16>'. Not yours to hand back? Tell the issue's architect."
    );
    expect(result.text).not.toContain("version 9");
  });

  test("the whole write that prints the line reads no ask", async () => {
    const requestPaths: string[] = [];
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      movedAdvice(request()),
      requestPaths
    );

    expect(adviceLines(result.text)).toEqual([verificationMoved]);
    expect(requestPaths.length).toBeGreaterThan(0);
    expect(requestPaths.filter((path) => path.startsWith("/api/v1/asks/"))).toEqual([]);
  });

  test("the line names who to tell when the request is not this agent's to hand back", async () => {
    const result = await executeWrite("dispatch_doc_edit", editArgs, movedAdvice(request()));

    expect(adviceLines(result.text)[0]).toEndWith(
      "Not yours to hand back? Tell the issue's architect."
    );
  });

  test("an open ask of this session is named before the moved request", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({
        decision_blocks: 1,
        your_open_asks: [{ id: "ask-1", question: "Choose the format?" }],
        moved_approval_request: request(),
      })
    );

    expect(adviceLines(result.text)).toEqual([
      'You still have an open ask on DSP-42: "Choose the format?" (ask-1). Still needed? See the `dispatch` skill, "Close what you opened".',
      verificationMoved,
    ]);
  });

  test("the superseded-approval line stays the last advice line, after this one", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      editArgs,
      advice({
        decision_blocks: 1,
        moved_approval_request: request(),
        superseded_approval: { approved_version: 14, version: 15, approved_by: "alice" },
      })
    );
    const lines = adviceLines(result.text);

    expect(lines).toHaveLength(2);
    expect(lines[0]).toBe(verificationMoved);
    expect(lines.at(-1)).toStartWith(
      "Stored version 15, superseding version 14, which alice approved."
    );
  });

  // Four states the report leaves nothing to say in: no report at all (every write that moved
  // nothing), a request this client cannot describe, a request no document of which stands above
  // the version it asks about, and a report whose owner is missing, where the call could name no
  // issue and no project and an invalid call is worse than no line.
  test.each([
    ["a write that moved no request", advice({ decision_blocks: 1 })],
    ["a report that names no document", movedAdvice(request({ documents: null }))],
    [
      "a report whose documents all stand at the version asked about",
      movedAdvice(request({ documents: [specDocument, { ...verificationDocument, version: 16 }] })),
    ],
    [
      "a report that names neither an issue nor a project",
      movedAdvice({ ask_id: ask, documents: [{ ...specDocument, version: 10 }] }),
    ],
  ])("prints no line for %s", async (_case, rawAdvice) => {
    const result = await executeWrite("dispatch_doc_edit", editArgs, rawAdvice);

    expect(result.text).not.toContain("moved the open approval request");
    expect(result.text).not.toContain("request-approval");
  });
});

describe("dispatch_related", () => {
  async function related(args: Record<string, unknown>, answer: Suggestions) {
    const requests: Array<{ method: string; path: string; body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push({
        method: init?.method ?? "GET",
        path: target.pathname,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      return response(answer);
    };
    const result = await executeDispatchTool({
      tool: "dispatch_related",
      args,
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec(),
      fetchImpl: fetchImpl as typeof fetch,
    });
    return { result, requests };
  }

  test("posts the title, text and switches to the project's related route and prints the form", async () => {
    const answer: Suggestions = {
      outcome: "found",
      search: "keywords_only",
      search_note: "the caller asked for keywords only",
      related: [issueItem("DSP-7", "Webhook retries")],
    };
    const { result, requests } = await related(
      {
        project: "DSP",
        title: "Retry failed webhook deliveries",
        text: "Back off exponentially.",
        exclude_issues: ["DSP-9"],
        keywords_only: true,
      },
      answer
    );

    expect(requests).toEqual([
      {
        method: "POST",
        path: "/api/v1/projects/DSP/related",
        body: {
          title: "Retry failed webhook deliveries",
          text: "Back off exponentially.",
          exclude_issues: ["DSP-9"],
          keywords_only: true,
        },
      },
    ]);
    expect(result.text).toBe(
      [
        "Related items (searched by keywords only: the caller asked for keywords only):",
        "- DSP-7 [backlog] Webhook retries → http://dispatch.test/issues/DSP-7",
      ].join("\n")
    );
    expect(result.details).toEqual({ project: "DSP", suggestions: answer });
  });

  test("a check not made names the project and the text's words to search by", async () => {
    const { result, requests } = await related(
      { project: "DSP", text: "the a of" },
      { outcome: "not_made", related: [], missing: "the text holds no word to search for" }
    );

    expect(requests.map(({ body }) => body)).toEqual([{ text: "the a of" }]);
    expect(result.text).toBe(
      "No related-item check was made: the text holds no word to search for. Search before you rely on this being new: dispatch search --project DSP with two or three of its words as --query"
    );
  });

  test("prints that it found nothing", async () => {
    const { result } = await related(
      { project: "DSP", title: "Quantum teleportation" },
      { outcome: "none", search: "meaning_and_keywords", related: [] }
    );

    expect(result.text).toBe(
      "Related-item check searched (meaning and keywords) and found nothing."
    );
  });

  test("prints the answered questions of other projects after its own project's items", async () => {
    const { result } = await related(
      { project: "DSP", title: "Retry failed webhook deliveries" },
      {
        outcome: "found",
        search: "meaning_and_keywords",
        related: [issueItem("DSP-7", "Webhook retries")],
        other_projects: [otherProjectAsk("OPS-9", "a9", "OPS", "Back off exponentially.")],
      }
    );

    expect(result.text).toBe(
      [
        "Related items (searched by meaning and keywords):",
        "- DSP-7 [backlog] Webhook retries → http://dispatch.test/issues/DSP-7",
        "Answered questions in other projects:",
        "- OPS: OPS-9 [done] Webhook retries → http://dispatch.test/issues/OPS-9#ask-a9",
        "  Answered by sami at 2026-09-12T13:20:00Z · options chosen: none",
        "  Answer text: Back off exponentially.",
      ].join("\n")
    );
  });
});

// The check of a write that is a question offers the answered questions of other projects. They
// print under a heading of their own, after every item of the asking project, so the reader sees
// which project each one is in and what was answered there.
describe("the answered questions of other projects", () => {
  const question = { issue: "DSP-42", question: "Should the billing service retry?" };
  const ownItem = issueItem("DSP-9", "Billing retries", "in_progress");
  const ownLines = [
    "Related items (searched by meaning and keywords):",
    "- DSP-9 [in_progress] Billing retries → http://dispatch.test/issues/DSP-9",
  ];
  const opsLines = [
    "- OPS: OPS-9 [done] Webhook retries → http://dispatch.test/issues/OPS-9#ask-a9",
    "  Answered by sami at 2026-09-12T13:20:00Z · options chosen: none",
    "  Answer text: Back off exponentially.",
  ];
  const opsItem = otherProjectAsk("OPS-9", "a9", "OPS", "Back off exponentially.");

  test("dispatch_ask prints them under their heading, after its own project's items", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      question,
      advice({
        suggestions: {
          outcome: "found",
          search: "meaning_and_keywords",
          related: [ownItem],
          other_projects: [
            opsItem,
            otherProjectAsk("WEB-3", "a3", "WEB", "Retry twice, then stop."),
          ],
        },
      })
    );

    expect(result.text).toBe(
      [
        askedLine,
        ...ownLines,
        "Answered questions in other projects:",
        ...opsLines,
        "- WEB: WEB-3 [done] Webhook retries → http://dispatch.test/issues/WEB-3#ask-a3",
        "  Answered by sami at 2026-09-12T13:20:00Z · options chosen: none",
        "  Answer text: Retry twice, then stop.",
        followsLine,
      ].join("\n")
    );
  });

  // A document owner's label already spells its project, so the row must not spell it twice.
  test("an ask owned by another project's document names that project once", async () => {
    const runbookAsk: WriteSuggestion = {
      kind: "ask",
      owner: {
        artifact_id: "document-7",
        kind: "document",
        name: "Delivery runbook",
        project: "OPS",
        slug: "delivery-runbook",
      },
      id: "a7",
      snippet: "Should webhook deliveries back off?",
      href: "/projects/OPS/documents/delivery-runbook?ask=a7",
      ask: {
        state: "answered",
        answer: {
          user: "sami",
          selected: [],
          text: "Back off exponentially.",
          at: "2026-09-12T13:20:00Z",
        },
      },
    };
    const result = await executeWrite(
      "dispatch_ask",
      question,
      advice({
        suggestions: {
          outcome: "found",
          search: "meaning_and_keywords",
          related: [ownItem],
          other_projects: [runbookAsk],
        },
      })
    );

    expect(result.text).toBe(
      [
        askedLine,
        ...ownLines,
        "Answered questions in other projects:",
        "- Delivery runbook (OPS/delivery-runbook) → http://dispatch.test/projects/OPS/documents/delivery-runbook?ask=a7",
        "  Answered by sami at 2026-09-12T13:20:00Z · options chosen: none",
        "  Answer text: Back off exponentially.",
        followsLine,
      ].join("\n")
    );
  });

  test("dispatch_doc_edit prints them under the new block's own items", async () => {
    const result = await executeWrite(
      "dispatch_doc_edit",
      {
        issue: "DSP-42",
        artifact: "spec",
        ops: [
          {
            op: "insert",
            after: "end",
            markdown: ':::ask{urgency="med"}\nShould webhook deliveries back off?\n:::\n',
          },
        ],
      },
      advice({
        decision_blocks_added: 1,
        decision_block_checks: [
          {
            block_id: "ask-block-1",
            question: "Should webhook deliveries back off?",
            suggestions: {
              outcome: "found",
              search: "meaning_and_keywords",
              related: [issueItem("DSP-7", "Webhook retries")],
              other_projects: [opsItem],
            },
          },
        ],
      })
    );

    expect(result.text).toBe(
      [
        `Applied 1 ops (no new version) ${issueSuffix}`,
        "Decision block ask-block-1:",
        "Related items (searched by meaning and keywords):",
        "- DSP-7 [backlog] Webhook retries → http://dispatch.test/issues/DSP-7",
        "Answered questions in other projects:",
        ...opsLines,
        "Document token: sha256:edited",
      ].join("\n")
    );
  });

  test.each([
    ["absent", {}],
    ["null", { other_projects: null }],
    ["an empty list", { other_projects: [] }],
  ] as const)("a check that offered none (%s) prints no heading", async (_shape, offered) => {
    const result = await executeWrite(
      "dispatch_ask",
      question,
      advice({
        suggestions: {
          outcome: "found",
          search: "meaning_and_keywords",
          related: [ownItem],
          ...offered,
        } as unknown as Suggestions,
      })
    );

    expect(result.text).toBe([askedLine, ...ownLines, followsLine].join("\n"));
  });

  test("an older server's past-decision field is ignored, and costs no line", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      question,
      advice({
        suggestions: {
          outcome: "found",
          search: "meaning_and_keywords",
          related: [ownItem],
          other_projects: [opsItem],
          // The field this client no longer reads. A deployment older than its deletion sends
          // it, and the lines hold nothing of it.
          decision: {
            ...otherProjectAsk("OPS-4", "a4", "OPS", "Retry once."),
            answered_by: "sami",
            answered_at: "2026-09-01T09:00:00Z",
          },
        } as unknown as Suggestions,
      })
    );

    expect(result.text).toBe(
      [
        askedLine,
        ...ownLines,
        "Answered questions in other projects:",
        ...opsLines,
        followsLine,
      ].join("\n")
    );
    expect(result.text).not.toContain("OPS-4");
  });

  test("a check whose own project found nothing says so, then prints them", async () => {
    const result = await executeWrite(
      "dispatch_ask",
      question,
      advice({
        suggestions: {
          outcome: "none",
          search: "meaning_and_keywords",
          related: [],
          other_projects: [opsItem],
        },
      })
    );

    expect(result.text).toBe(
      [
        askedLine,
        "Related-item check searched (meaning and keywords) and found nothing.",
        "Answered questions in other projects:",
        ...opsLines,
        followsLine,
      ].join("\n")
    );
  });
});

// A notice that cites a skill section sends the agent to read it, so each cited section must be a
// `## ` heading of that skill's SKILL.md, read as dispatch-tools.test.ts reads the Dispatch skill's.
test("every skill section a notice cites is a heading of that skill", () => {
  const repoRoot = path.resolve(import.meta.dir, "../../../..");
  // The dispatcher's entry and every module of its commands: a notice's text sits with the
  // handler that prints it, and a handler moves between modules, so all of them are read.
  const commands = path.resolve(import.meta.dir, "../dispatch-commands");
  const source = [
    readFileSync(path.resolve(import.meta.dir, "../dispatch-execute.ts"), "utf8"),
    ...readdirSync(commands)
      .filter((file) => file.endsWith(".ts"))
      .map((file) => readFileSync(path.join(commands, file), "utf8")),
  ].join("\n");
  const citations = [...source.matchAll(/the \\?`([a-z0-9-]+)\\?` skill, "([^"]+)"/gu)].map(
    ([, skill, section]) => ({ skill, section })
  );
  expect(citations.length).toBeGreaterThan(0);
  for (const { skill, section } of citations) {
    const headings = readFileSync(path.join(repoRoot, "skills", skill, "SKILL.md"), "utf8")
      .split("\n")
      .flatMap((line) => {
        const match = line.match(/^## (.+)$/u);
        return match === null ? [] : [match[1]];
      });
    expect(headings, `${skill} "${section}"`).toContain(section);
  }
});
