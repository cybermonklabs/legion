import { beforeAll, beforeEach, expect, jest, test } from "bun:test";
import type { OfferedReference } from "@legion/proof-editor";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, tick } from "../../__tests__/fake-clock";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  documentSurfaceHandlers,
  renderProofDocument,
} from "../../__tests__/proof-document";
import type { Agent, Ask, AskRead, IssueReferenceSummary } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { shortForm } from "../refs/routes";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

answerTextReadsWithTheSeededText();

/**
 * The document surface's own shared reads (`documentSurfaceHandlers`), and the read this file
 * adds to them: the two issues the text names are seeded in the query cache below, so the only
 * key that reaches the summary route is one no issue holds.
 */
beforeEach(() => {
  answerRequests(
    http.get("/api/v1/issues/:key/summary", ({ params }) =>
      Response.json({ code: "NOT_FOUND", error: `no issue ${String(params.key)}` }, { status: 404 })
    ),
    ...documentSurfaceHandlers()
  );
});

/** The text every editor below holds: a bare issue key, a `#id` and a bare `dispatch://`
 *  reference, the three forms a document's own text writes. */
const TEXT = "See CORE-2 and #q2 and dispatch://CORE-1/artifact/design here.";

/** The address of a document, written bare above and, below, as the autolink a document stores
 *  for a link whose text is its own destination. */
const AUTOLINK = "dispatch://CORE-1/artifact/design";

const ask: Ask = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "alice", kind: "user" },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "q2",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Which way round?",
  state: "open",
  urgency: "med",
};

const askRead: AskRead = {
  answers: [],
  ask,
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

const summary: IssueReferenceSummary = {
  documents: [
    {
      id: "artifact-1",
      kind: "doc",
      latest_version: 1,
      name: "spec.md",
      primary: true,
      slug: "spec",
    },
    {
      id: "artifact-2",
      kind: "doc",
      latest_version: 2,
      name: "design.md",
      primary: false,
      slug: "design",
    },
  ],
  key: "CORE-1",
  project: "CORE",
  status: "testing",
  title: "Ship the release",
};

const referenced: IssueReferenceSummary = {
  documents: [],
  key: "CORE-2",
  project: "CORE",
  status: "open",
  title: "Fix the thing",
};

/** A query client holding everything the three references resolve through: the issue a bare key
 *  names, the question map the document's own source answers with and the ask a `#id` declares,
 *  and the issue whose document the bare reference names. */
function seededQueryClient(): QueryClient {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);
  queryClient.setQueryData(["issue-summary", "CORE-2"], referenced);
  // The question map of this document, under the key the route's read is cached by: the
  // document is the source of its own text (`refs.ScopeOf`).
  queryClient.setQueryData(["references", "questions", "artifact", artifact.id], {
    document: artifact.id,
    questions: [{ ask: ask.id, block_id: "q2", reference: `dispatch://CORE-1/ask/${ask.id}` }],
  });
  queryClient.setQueryData(["ask", "ask-1"], askRead);
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return queryClient;
}

/** What the host last told the editor to draw: the words of each candidate to the title drawn
 *  over them, or null while it resolved none - the plain candidates' answers (`targets`), or the
 *  mentions' (`mentions`). */
function drawnTitles(
  references:
    | Record<"mentions" | "targets", ReadonlyMap<string, { title: string } | null>>
    | null
    | undefined,
  answers: "mentions" | "targets" = "targets"
): Record<string, string | null> {
  const drawn: Record<string, string | null> = {};
  for (const [words, target] of references?.[answers] ?? []) {
    drawn[words] = target?.title ?? null;
  }
  return drawn;
}

/** `keys` as the editor offers the candidates its reader found that no mention names. */
function plain(keys: readonly string[]): OfferedReference[] {
  return keys.map((key) => ({ key, mention: false }));
}

test("the live editor is told to draw a bare key's, a #id's and a bare reference's title", async () => {
  await warmMarkdownRenderer();
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");

    // The editor hands the host its own text and the host reads what it cites: the one reader
    // decides, so the words it answers are the words the candidates are keyed by.
    expect(references.scan(TEXT)).toEqual([
      { end: 10, key: "CORE-2", start: 4 },
      { end: 18, key: "#q2", start: 15 },
      { end: 56, key: "dispatch://CORE-1/artifact/design", start: 23 },
    ]);
    act(() => {
      references.onKeys(plain(["#q2", "CORE-2", "dispatch://CORE-1/artifact/design"]));
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({
        "#q2": "Which way round?",
        "CORE-2": "CORE-2 Fix the thing",
        "dispatch://CORE-1/artifact/design": "design.md",
      })
    );
    // A bare key reads as its key and its title, as a rendered body's link does, and clicking it
    // opens the issue it names.
    expect(editors[0]?.references?.targets.get("CORE-2")).toEqual({
      href: "dispatch://CORE-2",
      length: undefined,
      title: "CORE-2 Fix the thing",
      titleKey: "CORE-2",
    });
  } finally {
    view.unmount();
  }
});

test("a #id the author left a hyphen on draws its title over the id alone", async () => {
  await warmMarkdownRenderer();
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    act(() => {
      editors[0]?.references?.onKeys(plain(["#q2-"]));
    });

    // The question is q2, so the title stands for `#q2`, the three characters the reference
    // names, and the `-` the author typed stays their own text.
    await waitFor(() =>
      expect(editors[0]?.references?.targets.get("#q2-")).toEqual({
        href: "dispatch://CORE-1/ask/ask-1",
        length: 3,
        title: "Which way round?",
        titleKey: undefined,
      })
    );
  } finally {
    view.unmount();
  }
});

test("a key no issue holds and a #id no question declares are left as the author wrote them", async () => {
  jest.useFakeTimers();
  await warmMarkdownRenderer();
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    act(() => {
      editors[0]?.references?.onKeys(plain(["#q9", "CORE-404"]));
    });

    await tick(20);
    expect(drawnTitles(editors[0]?.references)).toEqual({ "#q9": null, "CORE-404": null });
  } finally {
    realClock();
    view.unmount();
  }
});

test("the live editor is told to draw the title for a link whose text is its own address", async () => {
  const queryClient = seededQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");

    // What the editor asks of a link whose text is its destination: the reader must read that
    // destination as one reference running end to end, which is then the candidate's key.
    expect(references.scan(AUTOLINK)).toEqual([{ end: AUTOLINK.length, key: AUTOLINK, start: 0 }]);
    act(() => {
      references.onKeys(plain([AUTOLINK]));
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({ [AUTOLINK]: "design.md" })
    );
  } finally {
    view.unmount();
  }
});

const SESSION = "01a1058e-f14f-7684-87eb-3dc885955551";

/** The session the mentions below name, as the registry lists it now: renamed since the
 *  mentions were written. */
const renamed: Agent = {
  capabilities: [],
  dir: "/workspaces/planner",
  host: "host-a",
  last_activity: null,
  last_seen: 1_700_000_000_000,
  machine_id: "host-a",
  open_asks: 0,
  roles: [],
  session_id: SESSION,
  state: "live",
  title: "Planner now",
};

/** The session holding the reviewer role, as the registry lists it: a role reference is named
 *  while a session it lists holds the role. */
const reviewing: Agent = {
  ...renamed,
  dir: "/workspaces/reviewer",
  roles: ["reviewer"],
  session_id: "01a1058e-f14f-7684-87eb-3dc885955552",
  title: "Reviewer",
};

// R4 and R5 in the editor, as a rendered body reads them: a party's mention - the editor offers
// its address as a mention - is drawn as `@` and the current name, a link with `@` words to
// anything but a party is left as its words, a known person is drawn as their login and no link,
// and a role a session holds as its token, linked. A session's live page is no reference in the
// shared grammar, so a link to it is never offered at all, and keeps its words as a rendered body
// does.
test("the live editor draws a mention as @ and the party's current name, a person as their login and a role as its token", async () => {
  const queryClient = seededQueryClient();
  queryClient.setQueryData(["agents"], [renamed, reviewing]);
  queryClient.setQueryData(["users"], [{ login: "dana@example.test" }]);
  const { editors, sync, view } = renderProofDocument({ queryClient });
  const address = `dispatch://agent/${SESSION}`;

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");
    expect(references.scan(`${window.location.origin}/agents/${SESSION}/live`)).toEqual([]);
    act(() => {
      references.onKeys([
        { key: address, mention: true },
        { key: "dispatch://CORE-2", mention: true },
        ...plain(["dispatch://person/dana@example.test", "dispatch://role/reviewer"]),
      ]);
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references, "mentions")).toEqual({
        [address]: "@Planner now",
        "dispatch://CORE-2": null,
      })
    );
    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({
        "dispatch://person/dana@example.test": "dana@example.test",
        "dispatch://role/reviewer": "reviewer",
      })
    );
    const targets = editors[0]?.references?.targets;
    expect(editors[0]?.references?.mentions.get(address)?.href).toBe(address);
    expect(targets?.get("dispatch://person/dana@example.test")?.href).toBeUndefined();
    expect(targets?.get("dispatch://role/reviewer")?.href).toBe("dispatch://role/reviewer");
  } finally {
    view.unmount();
  }
});

// The editor and the host agree on a mention through the typed field alone
// (`OfferedReference.mention`): the editor offers a party's address once as its own link's words
// and once as a mention's, and draws each from its own map (`@legion/proof-editor`'s
// `reference-decorations.test.ts`), so the host answers the first in `targets` as the party's
// name and the second in `mentions` as the mention sign and that name - never one for the other.
test("the host answers a mention the editor offers in the map the editor draws mentions from", async () => {
  const queryClient = seededQueryClient();
  queryClient.setQueryData(["agents"], [renamed]);
  const { editors, sync, view } = renderProofDocument({ queryClient });
  const address = `dispatch://agent/${SESSION}`;

  try {
    await sync();
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    act(() => {
      editors[0]?.references?.onKeys([
        { key: address, mention: false },
        { key: address, mention: true },
      ]);
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references, "mentions")).toEqual({ [address]: "@Planner now" })
    );
    expect(drawnTitles(editors[0]?.references)).toEqual({ [address]: "Planner now" });
  } finally {
    view.unmount();
  }
});

// A session Dispatch holds a record of that has no title has no name for a mention to read as:
// its mention is drawn as the mention sign and the session's short form, as a rendered body
// draws it, never left as the words its writer chose, which would pass for a current name. The
// same address written as its own link's words is still a link in those words.
test("the live editor draws a mention of a known session with no title as @ and its short form", async () => {
  const queryClient = seededQueryClient();
  queryClient.setQueryData(["agents"], [{ ...renamed, title: "" }]);
  const { editors, sync, view } = renderProofDocument({ queryClient });
  const address = `dispatch://agent/${SESSION}`;

  try {
    await sync();
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    act(() => {
      editors[0]?.references?.onKeys([
        { key: address, mention: false },
        { key: address, mention: true },
      ]);
    });

    const short = shortForm({ kind: "agent-session", session: SESSION });
    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references, "mentions")).toEqual({ [address]: `@${short}` })
    );
    expect(drawnTitles(editors[0]?.references)).toEqual({ [address]: address });
  } finally {
    view.unmount();
  }
});

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return (
    <>
      <div data-testid="current-route">{location.pathname}</div>
      {/* The whole of where the reader is: a click that navigates to the empty path keeps none
          of the version and the block they were reading. */}
      <div data-testid="current-location">
        {`${location.pathname}${location.search}${location.hash}`}
      </div>
    </>
  );
}

/** The past version of the document, rendered as the document page renders it, with the reader
 *  standing at `at`. */
function renderVersion(markdown: string = TEXT, at = "/") {
  const fake = fakeDocumentRuntime();
  const queryClient = seededQueryClient();
  queryClient.setQueryData(["artifact", "artifact-1", "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown,
    // The version page draws the server's reading of the stored text and parses nothing itself,
    // so a version a test invents carries the reading the page's own engine makes of that text
    // (`__tests__/display-trees`). `warmMarkdownRenderer` must have resolved first.
    markdown_tree: displayTree(markdown),
    named: false,
    number: 1,
    summary: null,
  });
  const view = render(
    <MemoryRouter initialEntries={[at]}>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <CurrentRoute />
          <VersionView
            artifactId="artifact-1"
            asks={[]}
            createdAt="2026-09-09T00:00:00Z"
            highlight={undefined}
            issueKey="CORE-1"
            owner={undefined}
            version={1}
          />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { ...fake, view };
}

test("a past version is told to draw the same three titles over its own text", async () => {
  await warmMarkdownRenderer();
  const { editors, view } = renderVersion();

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");
    act(() => {
      references.onKeys(plain(references.scan(TEXT).map((candidate) => candidate.key)));
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({
        "#q2": "Which way round?",
        "CORE-2": "CORE-2 Fix the thing",
        "dispatch://CORE-1/artifact/design": "design.md",
      })
    );
  } finally {
    view.unmount();
  }
});

test("a past version is told to draw the title for a link whose text is its own address", async () => {
  await warmMarkdownRenderer();
  const { editors, view } = renderVersion(`See [${AUTOLINK}](${AUTOLINK}) here.`);

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() => expect(editors[0]?.references).toBeDefined());
    const references = editors[0]?.references;
    if (references === undefined || references === null) throw new Error("references missing");
    // The editor reads a link whose text is its destination as one candidate of that address.
    expect(references.scan(AUTOLINK)).toEqual([{ end: AUTOLINK.length, key: AUTOLINK, start: 0 }]);
    act(() => {
      references.onKeys(plain([AUTOLINK]));
    });

    await waitFor(() =>
      expect(drawnTitles(editors[0]?.references)).toEqual({ [AUTOLINK]: "design.md" })
    );
  } finally {
    view.unmount();
  }
});

test("a click on a reference in a past version opens what it names", async () => {
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  const { editors, view } = renderVersion();

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // What the editor draws over a resolved reference: a link carrying its target, which is
    // also what its Markdown serializer leaves on a `dispatch://` link mark.
    act(() => {
      root.innerHTML =
        '<p><a class="dispatch-reference" role="link" data-dispatch-href="dispatch://CORE-2">CORE-2 Fix the thing</a></p>';
    });
    const anchor = root.querySelector("a");
    if (anchor === null) throw new Error("anchor missing");

    await user.click(anchor);
    expect(within(view.container).getByTestId("current-route").textContent).toBe("/issues/CORE-2");
  } finally {
    view.unmount();
  }
});

// Nothing shows a person (R5), so a click on the mention of one is the link's own and opens
// nothing: the reader stays on the version and the block they were reading, where the empty
// path this used to navigate to took them off both.
test("a click on a person in a document opens nothing and keeps the reader's place", async () => {
  const user = userEvent.setup();
  const at = "/projects/CORE/documents/design?v=2#b-one";
  const { editors, view } = renderVersion(TEXT, at);

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // A person's mention as the document stores and draws it: a link whose words are their
    // name, its `dispatch://` target sanitized out of the href and carried beside it.
    act(() => {
      root.innerHTML =
        '<p><a data-dispatch-href="dispatch://person/alice@example.com" href="">alice@example.com</a></p>';
    });
    const anchor = root.querySelector("a");
    if (anchor === null) throw new Error("anchor missing");

    await user.click(anchor);
    expect(within(view.container).getByTestId("current-location").textContent).toBe(at);
  } finally {
    view.unmount();
  }
});
