import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { ApiError, api } from "../../api/client";
import type {
  Agent,
  AskRead,
  IssueCollaborators,
  IssueReferenceSummary,
  MyReplyRow,
} from "../../api/types";
import { DismissControl, RowDismissContext } from "../inbox/DismissControl";
import { ThreadTitle } from "../inbox/ThreadTitle";
import { MarkdownBody } from "./MarkdownBody";
import { useMarkdownHeadline, warmMarkdownRenderer } from "./markdown-engine";
import { partyLink } from "./party-mention";
import { PlainText, usePlainHeadline } from "./plain-text";
import { ReferenceHostIs } from "./reference-host";
import { shortForm } from "./routes";
import type { TextSource } from "./text-references";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The one naming rule and the four host shapes, read on a painted body and on the plain-text
 * projection beside it. Every row here names CORE-1, whose summary the cache already holds, so
 * what is being read is how the rule draws a resolved reference - never whether it resolves.
 */

const summary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "in_progress",
  title: "Fix the thing",
};

/** A query client holding CORE-1's summary, so every reference below resolves at once. */
function clientWithIssue(): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["issue-summary", "CORE-1"], summary);
  return client;
}

function draw(children: ReactNode, client: QueryClient = clientWithIssue()) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

function words(element: HTMLElement): string {
  return (element.textContent ?? "").replace(/\s+/g, " ").trim();
}

// R8's first case: an address standing on its own reads as the target's current name.
test("an address standing on its own reads as the target's current name", async () => {
  const view = draw(<MarkdownBody markdown="Tied to dispatch://CORE-1 here." />);
  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 Fix the thing" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1");
  } finally {
    view.unmount();
  }
});

// R8's second case: a link whose words are its own address - the form a document stores a
// Markdown autolink in - reads as the title too, as the bare address does.
test("a link whose words are its own address reads as the target's current name", async () => {
  const view = draw(<MarkdownBody markdown="See [dispatch://CORE-1](dispatch://CORE-1)." />);
  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 Fix the thing" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1");
  } finally {
    view.unmount();
  }
});

// R8's third case, the only one that changes: a link whose words differ from its address keeps
// those words. The link still works and still carries the reference, so its hover card is the
// same card; only the words are the author's.
test("a link whose words its author wrote keeps those words", async () => {
  const view = draw(<MarkdownBody markdown="See [the rollout plan](dispatch://CORE-1) first." />);
  try {
    const link = await within(view.container).findByRole("link", { name: "the rollout plan" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1");
    expect(link).toHaveAttribute("data-dispatch-ref", "dispatch://CORE-1");
    await waitFor(() => expect(words(view.container)).toBe("See the rollout plan first."));
  } finally {
    view.unmount();
  }
});

// R8 for an address that names nobody: a login written in any other form than a stored one is
// no node, so the words the writer wrote stand with no link at all. A `dispatch://` href opens
// no page in any browser, so the worded case is dropped as the bare one already is.
test("a worded link whose address cites nothing reads as its words, with no link", async () => {
  const view = draw(
    <MarkdownBody markdown="Ask [Dana](dispatch://person/Dana) about it." />,
    new QueryClient({ defaultOptions: { queries: { retry: false } } })
  );
  try {
    await waitFor(() => expect(words(view.container)).toBe("Ask Dana about it."));
    expect(within(view.container).queryByRole("link")).toBeNull();
    expect(view.container.querySelector("[data-dispatch-ref]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// The plain-text projection reads the same reference the painted line reads, and names it the
// same way: a label and the line beside it never say two different things.
test("the plain-text projection names a reference by its resolved name", async () => {
  const view = draw(
    <PlainText markdown="Tied to **dispatch://CORE-1** here.">
      {(text) => <span data-testid="line">{text ?? ""}</span>}
    </PlainText>
  );
  try {
    await waitFor(() =>
      expect(view.getByTestId("line").textContent).toBe("Tied to CORE-1 Fix the thing here.")
    );
  } finally {
    view.unmount();
  }
});

const SESSION = "01a1058e-f14f-7684-87eb-3dc885955551";

/** The session a mention below names, as the registry lists it now: renamed since the mention
 *  was written as `@Old name`. */
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

/** A session the registry lists past its live window, holding the watcher role: an unresponsive
 *  session keeps its roles, and the server delivers a role's mention to it. */
const stalled: Agent = {
  ...renamed,
  dir: "/workspaces/watcher",
  roles: ["watcher"],
  session_id: "01a1058e-f14f-7684-87eb-3dc885955553",
  state: "unresponsive",
  title: "Watcher",
};

/** A query client holding the registry's answer - the renamed session, the session that holds
 *  the reviewer role and the unresponsive one that holds the watcher role - and the people
 *  Dispatch knows, of whom dana is one and nobody is not. */
function clientWithParties(): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["agents"], [renamed, reviewing, stalled]);
  client.setQueryData(["users"], [{ login: "dana@example.test" }]);
  return client;
}

// R4: a party's mention reads as the mention sign and the party's current name, a hand-written
// one as much as a picked one, so a session renamed since reads as it is called now. A worded
// link to the same party with no sign keeps its author's words (R8), and an `@` and a word with
// no link around it is no reference at all (R10). A session's live page names no node in the
// shared grammar (`pageRoutes` reads issue, document and agent-document pages alone), so a link
// to it is an ordinary link in its author's words, whatever they start with.
test("a party's mention reads as its current name, its other links keep their words, and a plain @ stays text", async () => {
  const page = `${window.location.origin}/agents/${SESSION}/live`;
  const view = draw(
    <MarkdownBody
      markdown={`Ask [@Old name](dispatch://agent/${SESSION}), [@Old](${page}), [the planner](dispatch://agent/${SESSION}) and @Planner today.`}
    />,
    clientWithParties()
  );
  try {
    const mention = await within(view.container).findByRole("link", { name: "@Planner now" });
    expect(mention).toHaveAttribute("href", `/agents/${SESSION}/live`);
    expect(within(view.container).getByRole("link", { name: "the planner" })).toHaveAttribute(
      "href",
      `/agents/${SESSION}/live`
    );
    expect(within(view.container).getByRole("link", { name: "@Old" })).toHaveAttribute(
      "href",
      page
    );
    expect(within(view.container).getAllByRole("link")).toHaveLength(3);
    expect(words(view.container)).toBe("Ask @Planner now, @Old, the planner and @Planner today.");
  } finally {
    view.unmount();
  }
});

// R5 and R8: a bare person reads as their login, with no link, where Dispatch knows the login;
// a login it does not know is the words written. A person's mention reads as the sign and the
// login. A bare role a session in the registry holds reads as its token, linked to its page.
test("a known person reads as their login with no link, an unknown one as written, and a held role as its token, linked", async () => {
  const view = draw(
    <MarkdownBody markdown="Ask dispatch://person/dana@example.test, [@Dana](dispatch://person/dana@example.test), dispatch://person/nobody@example.test and dispatch://role/reviewer." />,
    clientWithParties()
  );
  try {
    const role = await within(view.container).findByRole("link", { name: "reviewer" });
    expect(role).toHaveAttribute("href", "/agents?role=reviewer");
    await waitFor(() =>
      expect(words(view.container)).toBe(
        "Ask dana@example.test, @dana@example.test, dispatch://person/nobody@example.test and reviewer."
      )
    );
    // A count and an identity check, never a comparison of elements, which on a failure would
    // format the whole document.
    const links = within(view.container).getAllByRole("link");
    expect(links).toHaveLength(1);
    expect(links[0] === role).toBe(true);
  } finally {
    view.unmount();
  }
});

// R5 and R8 for a login GFM would cut: a person's login is an email address, and GFM's transform
// starts an email after a dot or a hyphen in its local part, which drew `dispatch://person/john.`
// and a link to `mailto:doe@example.com`. The engine keeps the address whole
// (`@legion/proof-editor`'s `whole-addresses.ts`), so it names the person and reads as their login,
// with no link and no mail address anywhere in the line.
test("a known person whose login holds a dot or a hyphen reads as their login with no link", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(
    ["users"],
    [{ login: "john.doe@example.com" }, { login: "person-2@example.com" }]
  );
  const view = draw(
    <MarkdownBody markdown="Ask dispatch://person/john.doe@example.com and dispatch://person/person-2@example.com now." />,
    client
  );
  try {
    await waitFor(() =>
      expect(words(view.container)).toBe("Ask john.doe@example.com and person-2@example.com now.")
    );
    expect(view.container.querySelectorAll("a")).toHaveLength(0);
    expect(view.container.innerHTML).not.toContain("mailto:");
  } finally {
    view.unmount();
  }
});

// R5 and R8, by the one rule of a known role (`role-holders.ts`), which the picker's role rows
// follow: a role a session the registry lists holds is named and linked, whether that session is
// live or unresponsive, as the server delivers to either. A text that names no record is on no
// issue, so no collaborators record a role for it, and a role no listed session holds reads as
// the words its writer wrote, with no link - bare or as a mention - as an unknown person does.
test("a role no session holds reads as the words written, and one an unresponsive session holds is linked", async () => {
  const view = draw(
    <MarkdownBody markdown="Ask dispatch://role/release-captain, [@captain](dispatch://role/release-captain), dispatch://role/reviewer and dispatch://role/watcher." />,
    clientWithParties()
  );
  try {
    const role = await within(view.container).findByRole("link", { name: "reviewer" });
    const unresponsive = await within(view.container).findByRole("link", { name: "watcher" });
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-dispatch-missing]")).toHaveLength(2)
    );
    expect(words(view.container)).toBe(
      "Ask dispatch://role/release-captain, @captain, reviewer and watcher."
    );
    // A count and an identity check, as above, so a failure formats no element.
    const links = within(view.container).getAllByRole("link");
    expect(links).toHaveLength(2);
    expect(links[0] === role).toBe(true);
    expect(links[1] === unresponsive).toBe(true);
  } finally {
    view.unmount();
  }
});

// The plain-text projection reads parties as the painted line does: a mention as the sign and
// the current name, an author's own words as they are - a link to a session's live page among
// them, as the painted line keeps it - a role as its token and a known person as their login.
test("the plain-text projection names a mention, a role and a person as the painted line does", async () => {
  const page = `${window.location.origin}/agents/${SESSION}/live`;
  const view = draw(
    <PlainText
      markdown={`Ask [@Old name](dispatch://agent/${SESSION}), [@Old](${page}), [the planner](dispatch://agent/${SESSION}), dispatch://role/reviewer and dispatch://person/dana@example.test.`}
    >
      {(text) => <span data-testid="line">{text ?? ""}</span>}
    </PlainText>,
    clientWithParties()
  );
  try {
    await waitFor(() =>
      expect(view.getByTestId("line").textContent).toBe(
        "Ask @Planner now, @Old, the planner, reviewer and dana@example.test."
      )
    );
  } finally {
    view.unmount();
  }
});

// A mention's `@` is words its writer wrote, never code: a party's link whose words are all code
// is a worded link like any other, and keeps its words in the painted line and in the
// plain-text projection alike, rather than reading as the party's name in one and as code in
// the other.
test("a party's link whose words are code keeps them, painted and projected alike", async () => {
  const markdown = `Ask [\`@x\`](dispatch://agent/${SESSION}) now.`;
  const painted = draw(<MarkdownBody markdown={markdown} />, clientWithParties());
  const projected = draw(
    <PlainText markdown={markdown}>
      {(text) => <span data-testid="code-line">{text ?? ""}</span>}
    </PlainText>,
    clientWithParties()
  );
  try {
    // The session's title is in the cache, so the name is drawn by the time the link is.
    await waitFor(() =>
      expect(painted.container.querySelector(`a[href="/agents/${SESSION}/live"]`)).not.toBeNull()
    );
    expect(words(painted.container)).toBe("Ask @x now.");
    await waitFor(() => expect(projected.getByTestId("code-line").textContent).toBe("Ask @x now."));
  } finally {
    painted.unmount();
    projected.unmount();
  }
});

/** `markdown`'s first line alone, as the projection reads it for a caller that asks for it
 *  (`usePlainHeadline`'s `firstLine`), which a node's line on the new issue page does. */
function FirstLine({
  children,
  markdown,
}: {
  children: (text: string | undefined) => ReactNode;
  markdown: string;
}): ReactNode {
  const { reads, text } = usePlainHeadline(undefined, markdown, { firstLine: true });
  return (
    <>
      {reads}
      {children(text?.text)}
    </>
  );
}

// The projection reads the whole text unless its caller asks for the first line alone, which a
// node's line on the new issue page does: the line is read from the parsed document, so its
// references are still named, and nothing after the first line is drawn or has its references
// named. A heading's line and a list's first line read the same way, and so does a block of
// HTML: its first line reads as its literal characters, which is how the projection draws HTML.
test("the plain-text projection reads the first line alone where it is asked to", async () => {
  const markdown = "Tied to **dispatch://CORE-1**\nhere.\n\n- A list after it";
  const view = draw(
    <>
      <FirstLine markdown={markdown}>
        {(text) => <span data-testid="first">{text ?? ""}</span>}
      </FirstLine>
      <PlainText markdown={markdown}>
        {(text) => <span data-testid="whole">{text ?? ""}</span>}
      </PlainText>
      <FirstLine markdown={"## The plan\n\nIts first step."}>
        {(text) => <span data-testid="heading">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={"\n- First item\n  and its next line\n- Second item"}>
        {(text) => <span data-testid="list">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={"![Diagram](https://example.com/a.png)\nThe priority keeps one build."}>
        {(text) => <span data-testid="image">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={"\\\nAfter a hard break\nand more"}>
        {(text) => <span data-testid="break">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={"| Name | Owner |\n| --- | --- |\n| Fleet | Ops |"}>
        {(text) => <span data-testid="table">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={"```\n\nfirst code\nsecond\n```"}>
        {(text) => <span data-testid="code">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={'<img src="https://example.com/a.png">\nThe priority keeps one build.'}>
        {(text) => <span data-testid="html-image">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine
        markdown={"<details>\n<summary>Why this priority</summary>\nThe fleet first.\n</details>"}
      >
        {(text) => <span data-testid="html-details">{text ?? ""}</span>}
      </FirstLine>
      <FirstLine markdown={"<br>\nThe next line."}>
        {(text) => <span data-testid="html-break">{text ?? ""}</span>}
      </FirstLine>
    </>
  );
  try {
    await waitFor(() =>
      expect(view.getByTestId("first").textContent).toBe("Tied to CORE-1 Fix the thing")
    );
    await waitFor(() => expect(view.getByTestId("heading").textContent).toBe("The plan"));
    await waitFor(() => expect(view.getByTestId("list").textContent).toBe("First item"));
    // A line that holds no words - an image alone, a hard break - is passed over, a table's
    // first row reads whole, and a code block's first line is its first line that holds words.
    await waitFor(() =>
      expect(view.getByTestId("image").textContent).toBe("The priority keeps one build.")
    );
    await waitFor(() => expect(view.getByTestId("break").textContent).toBe("After a hard break"));
    await waitFor(() => expect(view.getByTestId("table").textContent).toBe("Name Owner"));
    await waitFor(() => expect(view.getByTestId("code").textContent).toBe("first code"));
    // A block of HTML is one node holding all of its lines, and its first line is its first
    // line's literal characters, as the projection draws them: never blank, and never the
    // lines after it.
    await waitFor(() =>
      expect(view.getByTestId("html-image").textContent).toBe(
        '<img src="https://example.com/a.png">'
      )
    );
    await waitFor(() => expect(view.getByTestId("html-details").textContent).toBe("<details>"));
    await waitFor(() => expect(view.getByTestId("html-break").textContent).toBe("<br>"));
    await waitFor(() =>
      expect(view.getByTestId("whole").textContent).toBe(
        "Tied to CORE-1 Fix the thing here. A list after it"
      )
    );
  } finally {
    view.unmount();
  }
});

// Shape 2: inside a link or a button there is no link of its own. The words and the reference
// stay - the hover card is still the card - and the host is what a click follows.
test("a reference inside a link or a button is drawn inert", async () => {
  const view = draw(
    <ReferenceHostIs host="control">
      <MarkdownBody markdown="Tied to dispatch://CORE-1 here." />
    </ReferenceHostIs>
  );
  try {
    await waitFor(() => expect(words(view.container)).toBe("Tied to CORE-1 Fix the thing here."));
    expect(view.container.querySelector("a")).toBeNull();
    const inert = view.container.querySelector("[data-markdown-link]");
    expect(inert).toHaveAttribute("data-dispatch-ref", "dispatch://CORE-1");
  } finally {
    view.unmount();
  }
});

// Shape 4: a suggestion's replacement shows the exact characters it swaps, so no title is drawn
// over a reference there - the reader has to see what accepting it would write.
test("a reference in a diff is drawn as its characters", async () => {
  const view = draw(
    <ReferenceHostIs host="diff">
      <MarkdownBody markdown="Tied to **dispatch://CORE-1** here." variant="inline" />
    </ReferenceHostIs>
  );
  try {
    expect(words(view.container)).toBe("Tied to **dispatch://CORE-1** here.");
    expect(view.container.querySelector("a")).toBeNull();
    expect(view.container.querySelector("strong")).toBeNull();
  } finally {
    view.unmount();
  }
});

const threadRow = {
  kind: "comment",
  reply: { author: { id: "planner", kind: "session" }, created_at: "2026-01-01T00:00:00Z" },
  ref: "dispatch://CORE-1/comment/comment-1",
  subject: "## Ship **behind a flag**, or wait?",
  thread_id: "comment-1",
  unread: false,
  unread_count: 0,
} as unknown as MyReplyRow;

// The Inbox's own line is drawn by the renderer, not stripped by a regular expression: the
// heading's words format as the thread formats them, and no syntax is left standing.
test("an Inbox thread title draws its line through the renderer", async () => {
  const view = draw(<ThreadTitle className="text-sm" thread={threadRow} />);
  try {
    await waitFor(() =>
      expect(view.container.querySelector("strong")?.textContent).toBe("behind a flag")
    );
    expect(words(view.container)).toBe("Ship behind a flag, or wait?");
    // One line, never the heading's own block.
    expect(view.container.querySelector("h1, h2, h3, p, ul")).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The spies a row put on the API client, taken off again once it ends. */
const spies: { mockRestore(): void }[] = [];

afterEach(() => {
  for (const spy of spies.splice(0)) {
    spy.mockRestore();
  }
});

const ASK_ID = "2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b";
const ASK = `dispatch://CORE-1/ask/${ASK_ID}`;

/** The comment a row below is the text of: its document declares the ask above as `q2`. */
const COMMENT: TextSource = { id: "comment-1", issue: "CORE-1", kind: "comment" };

/** An ask of CORE-1 whose question is `Ship it?`, as its own read answers it. */
const shipIt = {
  answers: [],
  ask: {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "planner", kind: "session" },
    block_id: "q2",
    created_at: "2026-01-01T00:00:00Z",
    edited_at: null,
    id: ASK_ID,
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Ship it?",
    state: "open",
    urgency: "med",
  },
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
} as AskRead;

/** Answers the three reads the ask is reached through: its own, the graph read that resolves
 *  its id written short, and the question map of the comment above. */
function answerTheAsk(): void {
  spies.push(
    spyOn(api, "getAsk").mockResolvedValue(shipIt),
    spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: graphNode({ id: ASK_ID, kind: "ask", ref: ASK }),
    }),
    spyOn(api, "getReferenceQuestions").mockResolvedValue({
      document: "artifact-1",
      questions: [{ ask: ASK_ID, block_id: "q2", reference: ASK }],
    })
  );
}

/** A text's plain-text projection, drawn where a row can read it. */
function Line({
  id = "line",
  markdown,
  source,
}: {
  id?: string;
  markdown: string;
  source?: TextSource;
}): ReactNode {
  return (
    <PlainText markdown={markdown} source={source}>
      {(text) => <span data-testid={id}>{text ?? ""}</span>}
    </PlainText>
  );
}

/** A text's projection by a reader that holds no names for its references. A title drawn over a
 *  record's text is not this reader: it says it reads none (`ReferenceNaming.readsNone`), and
 *  its `dispatch://` references read as their short forms (`markdown-surfaces.test.tsx`). */
function Headline({ markdown }: { markdown: string }): ReactNode {
  return <span data-testid="headline">{useMarkdownHeadline(undefined, 200, markdown)?.text}</span>;
}

/** CORE-1's implementer role, which no session the registry lists holds. */
const STOPPED_ROLE = "legion-p-core-1-implementer";

/** An issue's collaborators as Dispatch answers them: CORE-1's record its implementer, whose
 *  session has stopped, and any other issue's record no Legion role. */
function collaboratorsOf(key: string): IssueCollaborators {
  return {
    agents:
      key === "CORE-1"
        ? [
            {
              kind: "session",
              last_at: null,
              legion: {
                replaced_at: null,
                replaced_by: null,
                role: "implementer",
                role_token: STOPPED_ROLE,
                started_at: "2026-10-26T00:00:00Z",
              },
              reasons: [],
              session_id: "old-implementer",
              title: "Old implementer",
            },
          ]
        : [],
    issue_key: key,
    legion_registry: "ok",
    people: [],
  };
}

/** Answers each issue's collaborators read with `answer`, and hands back the spy. */
function answerCollaborators(answer: (key: string) => IssueCollaborators = collaboratorsOf) {
  const read = spyOn(api, "getIssueCollaborators").mockImplementation(async (key) => answer(key));
  spies.push(read);
  return read;
}

// A role is known while a session the registry lists holds it, or while the
// collaborators of the issue the record is on record it (`role-holders.ts`): the issue's stopped
// roles, which the picker offers under "Not running". Such a role reads as its token, linked to
// its page, bare and as a mention, and the plain-text projection names it the same way, though
// no session holds it.
test("a stopped role the record's issue's collaborators record reads as its token, linked", async () => {
  const read = answerCollaborators();
  const markdown = `Ask dispatch://role/${STOPPED_ROLE} and [@implementer](dispatch://role/${STOPPED_ROLE}).`;
  const view = draw(
    <>
      <MarkdownBody markdown={markdown} source={COMMENT} />
      <Line markdown={markdown} source={COMMENT} />
    </>,
    clientWithParties()
  );
  try {
    const bare = await within(view.container).findByRole("link", { name: STOPPED_ROLE });
    const mention = within(view.container).getByRole("link", { name: `@${STOPPED_ROLE}` });
    expect(bare).toHaveAttribute("href", `/agents?role=${STOPPED_ROLE}`);
    expect(mention).toHaveAttribute("href", `/agents?role=${STOPPED_ROLE}`);
    await waitFor(() =>
      expect(view.getByTestId("line").textContent).toBe(`Ask ${STOPPED_ROLE} and @${STOPPED_ROLE}.`)
    );
    expect(read).toHaveBeenCalledWith("CORE-1");
  } finally {
    view.unmount();
  }
});

// A role whose recorded session runs on without it: the registry lists that session, live, and it
// claims no role, so nobody holds the role, and the collaborators still record it. It is known all
// the same and reads as its token, linked - the one rule the picker offers it by, under "Not
// running" (`useRefPicker.test.tsx`, "the picker offers a recorded role whose session is live and
// claims no role").
test("a role whose recorded session runs on without it reads as its token, linked", async () => {
  answerCollaborators();
  const client = clientWithParties();
  client.setQueryData(
    ["agents"],
    [
      renamed,
      reviewing,
      stalled,
      { ...renamed, roles: [], session_id: "old-implementer", title: "Old implementer" },
    ]
  );
  const view = draw(
    <MarkdownBody markdown={`Ask dispatch://role/${STOPPED_ROLE}.`} source={COMMENT} />,
    client
  );
  try {
    const role = await within(view.container).findByRole("link", { name: STOPPED_ROLE });
    expect(role).toHaveAttribute("href", `/agents?role=${STOPPED_ROLE}`);
  } finally {
    view.unmount();
  }
});

// The rule's other side: a role neither answer records is no target, and reads as the words its
// writer wrote. That is a role CORE-1's collaborators do not record, on a record of CORE-1. It is
// also CORE-1's stopped role on a record of CORE-2, whose collaborators do not record it, and on
// a record on no issue, which reads no collaborators at all.
test("a role neither the registry nor the record's issue's collaborators record reads as the words written", async () => {
  const read = answerCollaborators();
  const view = draw(
    <>
      <div data-testid="unrecorded">
        <MarkdownBody markdown="Ask dispatch://role/release-captain." source={COMMENT} />
      </div>
      <div data-testid="other-issue">
        <MarkdownBody
          markdown={`Ask dispatch://role/${STOPPED_ROLE}.`}
          source={{ id: "comment-2", issue: "CORE-2", kind: "comment" }}
        />
      </div>
      <div data-testid="no-issue">
        <MarkdownBody
          markdown={`Ask dispatch://role/${STOPPED_ROLE}.`}
          source={{ id: "message-2", issue: undefined, kind: "message" }}
        />
      </div>
    </>,
    clientWithParties()
  );
  try {
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-dispatch-missing]")).toHaveLength(3)
    );
    expect(within(view.container).queryByRole("link")).toBeNull();
    expect(words(view.getByTestId("unrecorded"))).toBe("Ask dispatch://role/release-captain.");
    expect(words(view.getByTestId("other-issue"))).toBe(`Ask dispatch://role/${STOPPED_ROLE}.`);
    expect(words(view.getByTestId("no-issue"))).toBe(`Ask dispatch://role/${STOPPED_ROLE}.`);
    expect(new Set(read.mock.calls.map(([key]) => key))).toEqual(new Set(["CORE-1", "CORE-2"]));
  } finally {
    view.unmount();
  }
});

// A role a listed session holds is known by the registry alone, so a record of an issue reads
// no collaborators for it: they are read only for a role the registry answered no holder for.
test("a held role is linked with no read of the record's issue's collaborators", async () => {
  const read = answerCollaborators();
  const view = draw(
    <MarkdownBody markdown="Ask dispatch://role/reviewer." source={COMMENT} />,
    clientWithParties()
  );
  try {
    const role = await within(view.container).findByRole("link", { name: "reviewer" });
    expect(role).toHaveAttribute("href", "/agents?role=reviewer");
    expect(read).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

// A collaborators answer made while Legion's registry did not answer (`legion_registry:
// "unavailable"`) records no Legion role of anyone, so it says nothing about this one: the role
// keeps its link in the words its writer wrote, as it does while any read is out, rather than
// reading as a target Dispatch answered does not exist.
test("a role keeps its link in its words while Legion's record of the issue cannot be read", async () => {
  const unavailable = (key: string): IssueCollaborators => ({
    ...collaboratorsOf(key),
    agents: [],
    legion_registry: "unavailable",
  });
  answerCollaborators(unavailable);
  const client = clientWithParties();
  // Answered before the body mounts, so its first commit already reads it.
  client.setQueryData(["issue", "CORE-1", "collaborators"], unavailable("CORE-1"));
  const view = draw(
    <MarkdownBody markdown={`Ask dispatch://role/${STOPPED_ROLE}.`} source={COMMENT} />,
    client
  );
  try {
    const role = await within(view.container).findByRole("link", {
      name: `dispatch://role/${STOPPED_ROLE}`,
    });
    expect(role).toHaveAttribute("href", `/agents?role=${STOPPED_ROLE}`);
    expect(view.container.querySelector("[data-dispatch-missing]")).toBeNull();
  } finally {
    view.unmount();
  }
});

// R8 in the projection: an address Dispatch answers does not exist is the words its writer
// wrote, bare or as its own link's words, as the painted line reads it - never its short form,
// which would pass for a name.
test("an address whose target does not exist projects as the words written", async () => {
  spies.push(
    spyOn(api, "getIssueSummary").mockRejectedValue(
      new ApiError(404, { code: "NOT_FOUND", error: "no issue" })
    )
  );
  const markdown =
    "Tied to dispatch://CORE-404 and [dispatch://CORE-404](dispatch://CORE-404) here.";
  const written = "Tied to dispatch://CORE-404 and dispatch://CORE-404 here.";
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>,
    client
  );
  try {
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-dispatch-missing]")).toHaveLength(2)
    );
    expect(words(view.getByTestId("painted"))).toBe(written);
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(written));
  } finally {
    view.unmount();
  }
});

// The projection reads every form the painted line reads, and names each as it does: an issue
// key and a dashboard address as the issue's key and title, and an id written short as the
// record it names.
test("a bare key, a dashboard address and a short id project as the painted line draws them", async () => {
  answerTheAsk();
  const markdown = `See CORE-1, ${window.location.origin}/issues/CORE-1 and dispatch://CORE-1/ask/2a7f1c90 here.`;
  const drawn = "See CORE-1 Fix the thing, CORE-1 Fix the thing and Ship it? here.";
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>
  );
  try {
    await waitFor(() => expect(words(view.getByTestId("painted"))).toBe(drawn));
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(drawn));
  } finally {
    view.unmount();
  }
});

// A reader that holds no facts for a form writes it as its author did, as a rendered body draws
// the words of a link whose title has not resolved: even a `dispatch://` address stays as
// written while nothing has named it, and a line that names no record reads a `#id` as written,
// since only a record's document declares one.
test("a reader with no facts for a form projects it as written", async () => {
  const written = `See CORE-1, ${window.location.origin}/issues/CORE-1 and #q2, not dispatch://CORE-1.`;
  const view = draw(
    <>
      <Headline markdown={written} />
      <Line markdown="Decide #q2 now." />
    </>
  );
  try {
    await waitFor(() => expect(view.getByTestId("headline").textContent).toBe(written));
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe("Decide #q2 now."));
  } finally {
    view.unmount();
  }
});

// A session Dispatch holds a record of that has no title has no name to draw: its bare address
// is the words its writer wrote, linked in the painted line and written out in the projection.
test("a known session with no title, written as its bare address, projects as written", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["agents"], [{ ...renamed, title: "" }]);
  const markdown = `Ask dispatch://agent/${SESSION} now.`;
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>,
    client
  );
  try {
    await within(view.getByTestId("painted")).findByRole("link", {
      name: `dispatch://agent/${SESSION}`,
    });
    expect(words(view.getByTestId("painted"))).toBe(markdown);
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(markdown));
  } finally {
    view.unmount();
  }
});

// The painted line links no reference a link's words hold (`RefLink.tsx`'s `linkPassReads`), so
// the projection names none there: the words are the author's, and the same key after the link
// is named.
test("a reference inside a link's own words is not named", async () => {
  const markdown = "[see dispatch://CORE-1 and CORE-1](https://example.test/x) for CORE-1.";
  const drawn = "see dispatch://CORE-1 and CORE-1 for CORE-1 Fix the thing.";
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>
  );
  try {
    await waitFor(() => expect(words(view.getByTestId("painted"))).toBe(drawn));
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(drawn));
  } finally {
    view.unmount();
  }
});

// A `#id` is declared by the document of the record a text is, so the Inbox's label names one
// because it says which record its subject is, as the row's own line does (`ThreadTitle`); the
// same subject projected with no record named keeps the `#id`.
test("a #id in an Inbox thread subject projects as its question where the row names its record", async () => {
  answerTheAsk();
  const thread = { ...threadRow, subject: "Decide #q2 now" } as MyReplyRow;
  const view = draw(
    <RowDismissContext.Provider value={{ dismiss: () => {}, pending: new Set(), show: "inbox" }}>
      <DismissControl shown thread={thread} />
      <Line markdown={thread.subject} />
    </RowDismissContext.Provider>
  );
  try {
    await waitFor(() =>
      expect(view.getByRole("button").getAttribute("aria-label")).toBe(
        "Dismiss: Decide Ship it? now"
      )
    );
    expect(view.getByTestId("line").textContent).toBe("Decide #q2 now");
  } finally {
    view.unmount();
  }
});

// Markdown drops the backslash of an escaped `\#q2`, so the words hold a `#q2` the source never
// wrote. The painted line links only what the source cites, and the projection is held to the
// same: the escaped one stays its words in both, beside the one written plain, which is named.
test("an escaped #id stays its words, painted and projected alike", async () => {
  answerTheAsk();
  const escaped = "Decide \\#q2 now.";
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={escaped} source={COMMENT} />
      </div>
      <Line markdown={escaped} source={COMMENT} />
      <Line id="plain" markdown="Decide #q2 now." source={COMMENT} />
    </>
  );
  try {
    await waitFor(() => expect(view.getByTestId("plain").textContent).toBe("Decide Ship it? now."));
    expect(view.getByTestId("line").textContent).toBe("Decide #q2 now.");
    expect(words(view.getByTestId("painted"))).toBe("Decide #q2 now.");
    expect(within(view.getByTestId("painted")).queryByRole("link")).toBeNull();
  } finally {
    view.unmount();
  }
});

// A mention's sign may be written escaped: Markdown reads `\@` as `@`, so the link is the
// mention it reads as, in the painted line and the projection - and in a draft's chips, which
// read the same words through the parse (`parsed-references.ts`, `party-references.test.ts`).
test("a mention whose sign is written escaped reads as the party's current name", async () => {
  const markdown = `Ask [\\@Old name](dispatch://agent/${SESSION}) now.`;
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>,
    clientWithParties()
  );
  try {
    await within(view.getByTestId("painted")).findByRole("link", { name: "@Planner now" });
    expect(words(view.getByTestId("painted"))).toBe("Ask @Planner now now.");
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe("Ask @Planner now now."));
  } finally {
    view.unmount();
  }
});

// A session Dispatch holds a record of that has no title has no name for a mention to read as.
// Its mention reads as the mention sign and the session's short form - never as the words its
// writer chose, which would pass for a current name - in the painted line, where it is still
// the link to the session, and in the projection alike.
test("a mention of a known session with no title reads as @ and its short form", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["agents"], [{ ...renamed, title: "" }]);
  const markdown = `Ask [@Alice](dispatch://agent/${SESSION}) now.`;
  const short = shortForm({ kind: "agent-session", session: SESSION });
  const view = draw(
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>,
    client
  );
  try {
    const mention = await within(view.getByTestId("painted")).findByRole("link", {
      name: `@${short}`,
    });
    expect(mention).toHaveAttribute("href", `/agents/${SESSION}/live`);
    expect(words(view.getByTestId("painted"))).toBe(`Ask @${short} now.`);
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(`Ask @${short} now.`));
  } finally {
    view.unmount();
  }
});

/** A text drawn as a painted body and as its plain-text projection, side by side. */
function PaintedAndProjected({ markdown }: { markdown: string }): ReactNode {
  return (
    <>
      <div data-testid="painted">
        <MarkdownBody markdown={markdown} />
      </div>
      <Line markdown={markdown} />
    </>
  );
}

// The formatting rule every reader of a record applies (`@legion/contracts`'s
// `dispatch-mentions.ts`), in its three cases, painted and projected alike. First: code anywhere
// in a link's words keeps the words - all of them code, code that opens them, or code after the
// sign.
test("a party's link with code anywhere in its words keeps them, painted and projected alike", async () => {
  const agent = `dispatch://agent/${SESSION}`;
  const markdown = `Ask [\`@x\`](${agent}), [\`@y\` too](${agent}) and [@z \`now\`](${agent}) here.`;
  const written = "Ask @x, @y too and @z now here.";
  const view = draw(<PaintedAndProjected markdown={markdown} />, clientWithParties());
  try {
    await waitFor(() => expect(words(view.getByTestId("painted"))).toBe(written));
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(written));
    // The session's title is in the cache: no link draws it. A count, so a failure formats no
    // element.
    expect(within(view.getByTestId("painted")).queryAllByText(/Planner now/)).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// Second: emphasis, strong or strikethrough over all of a link's words is the same record as
// that formatting around the link, so the link is still a mention, drawn in that formatting -
// written inside the brackets, around the link, or around the sentence.
test("a party's link whose words are all under one formatting reads as its current name", async () => {
  const agent = `dispatch://agent/${SESSION}`;
  const markdown = `Ask [**@Old**](${agent}), *[@Old](${agent})* and ~~cc [@Old](${agent}) now~~ here.`;
  const drawn = "Ask @Planner now, @Planner now and cc @Planner now now here.";
  const view = draw(<PaintedAndProjected markdown={markdown} />, clientWithParties());
  try {
    await waitFor(() => expect(words(view.getByTestId("painted"))).toBe(drawn));
    const painted = view.getByTestId("painted");
    expect(painted.querySelector("strong a")?.textContent).toBe("@Planner now");
    expect(painted.querySelector("em a")?.textContent).toBe("@Planner now");
    expect(within(painted).getAllByRole("link", { name: "@Planner now" })).toHaveLength(3);
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(drawn));
  } finally {
    view.unmount();
  }
});

// Third: formatting over some of a link's words makes them the author's words. Proof's schema
// draws strong and emphasis outside a link, so such a link is painted as more than one anchor,
// and the painted line reads them as the one link they are.
test("a party's link whose words are formatted in part keeps them, painted and projected alike", async () => {
  const agent = `dispatch://agent/${SESSION}`;
  const markdown = `Ask [@Old **name**](${agent}), [*@Old* name](${agent}) and [@Old ~~name~~](${agent}) here.`;
  const written = "Ask @Old name, @Old name and @Old name here.";
  const view = draw(<PaintedAndProjected markdown={markdown} />, clientWithParties());
  try {
    await waitFor(() => expect(words(view.getByTestId("painted"))).toBe(written));
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(written));
    expect(within(view.getByTestId("painted")).queryAllByText(/Planner now/)).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// A hard break among a link's words cuts them as formatting over a part of them does, so they
// are the author's words. The break holds no link, so Proof paints such a link as an anchor on
// each side of it, and the projection reads a run of text on each side; each reads the two as
// the one link they are. The plain mention beside it shows the party's name has been read.
test("a party's link whose words a hard break cuts keeps them, painted and projected alike", async () => {
  const agent = `dispatch://agent/${SESSION}`;
  const markdown = `Ask [@Old\\\nname](${agent}) and [@Old](${agent}) here.`;
  const view = draw(<PaintedAndProjected markdown={markdown} />, clientWithParties());
  try {
    const painted = view.getByTestId("painted");
    await waitFor(() => expect(words(painted)).toBe("Ask @Oldname and @Planner now here."));
    expect(
      within(painted)
        .getAllByRole("link")
        .map((link) => link.textContent)
    ).toEqual(["@Old", "name", "@Planner now"]);
    await waitFor(() =>
      expect(view.getByTestId("line").textContent).toBe("Ask @Old name and @Planner now here.")
    );
  } finally {
    view.unmount();
  }
});

// A session picked while its title held a formatting character, then renamed: the pick wrote the
// title as literal words (`partyLink`), so the formatting covers no part of them, and the link is
// the mention it was picked as, which reads as the session's current name, painted and projected
// alike.
test("a party picked by a name holding formatting characters reads as its current name after a rename", async () => {
  const markdown = `Ask ${partyLink("fix *the* cache", `dispatch://agent/${SESSION}`)} now.`;
  const view = draw(<PaintedAndProjected markdown={markdown} />, clientWithParties());
  try {
    const painted = view.getByTestId("painted");
    await within(painted).findByRole("link", { name: "@Planner now" });
    expect(words(painted)).toBe("Ask @Planner now now.");
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe("Ask @Planner now now."));
  } finally {
    view.unmount();
  }
});
