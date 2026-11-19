import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { type RenderResult, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import * as ts from "typescript";

import { graphNode } from "../../__tests__/graph-fixture";
import { artifact as specDocument } from "../../__tests__/proof-document";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import type {
  Actor,
  Artifact,
  ArtifactApproval,
  ArtifactReview,
  Ask,
  Priorities,
  Subscriber,
} from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { MultiSelect } from "../../components/MultiSelect";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import { PinButton } from "../../components/PinButton";
import { RefusableButton } from "../../components/RefusableButton";
import { withoutEllipsis } from "../../components/tooltips";
import * as theme from "../../theme/classes";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  PAGE_COLORS,
  pageInkText,
  pageStateText,
  pageWaitingChip,
  secondaryButtonCompact,
  textMutedOnSurface,
} from "../../theme/classes";
import { ComponentPicker } from "../architecture/ComponentPicker";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ConnectionDot } from "../doc/ConnectionDot";
import { AskCard } from "../inbox/AskCard";
import { ApprovalSteps } from "../issue/ApprovalSteps";
import { AssigneeControl } from "../issue/AssigneeControl";
import { ClaimChip } from "../issue/ClaimChip";
import { GitHubLink } from "../issue/GitHubLink";
import type { StepEntry } from "../issue/gated-steps";
import { componentsControlName, IssueComponentsLine } from "../issue/IssueComponentsLine";
import { IssueLabels } from "../issue/IssueLabels";
import { PriorityControl, PriorityEditor } from "../issue/PriorityControl";
import { RepoControl } from "../issue/RepoControl";
import {
  RouteReachUnknownMarker,
  UnreachableRouteMarker,
  UnresponsiveRouteMarker,
} from "../issue/RouteReach";
import { SubscribedAgents } from "../issue/SubscribedAgents";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { AgentName, AgentUnresponsiveNote } from "../refs/AgentName";
import { relativeTimeLabel, Timestamp } from "../refs/Timestamp";
import { AUTHOR_TEXT_ATTRIBUTE, PAGE_CHROME_ATTRIBUTE, PROPERTY_LABELS } from "./page-structure";
import { ANSWER_LINES, AUTHOR_KIND_WORDS, CONTROL_LABELS, STATE_LINES } from "./page-words";
import {
  architecture,
  CLAIMED_AT,
  classOf,
  colourOf,
  DECISIONS_ID,
  doc,
  draw,
  FOCUS_VARIANTS,
  GONE_SESSION,
  holdsItsLine,
  idlePriorityWrite,
  LIVE_SESSION,
  listed,
  minutesFromNow,
  offPageColours,
  openAsk,
  priorities,
  proofDocument,
  RAW_COLOR_PATTERN,
  SPEC_ID,
  STALLED_SESSION,
  type Surface,
  settleReads,
  specRequest,
  splitToken,
  threadOf,
  tokens,
  undraw,
} from "./shared-controls-fixture";

/**
 * The shared controls on the new issue page's surface: inside `NoTooltips` each shared control
 * sets no `title`, draws what a `title` held, says the page's words with their marks, draws every
 * colour in the page's palette, and lets every value it draws wrap; outside it, each draws what
 * `main` draws. Each case mounts the controls through `draw` (`shared-controls-fixture.tsx`),
 * from fixtures that the controls' own test files on `main` build the same way. One case reads
 * source rather than a drawing: every page colour pair of an `ON_PAGE` table is registered in
 * `CONTRAST_CHECKS`.
 */

// ---------------------------------------------------------------------------------------------
// Drawing a control on either side of `NoTooltips`
// ---------------------------------------------------------------------------------------------

/** One shared control, or a few of one file, in the state a case reads it in. */
interface Mount {
  /** The shared control that draws it, and how it is drawn. */
  readonly name: string;
  /** What to draw: called once for each draw, so each draw holds fresh fixtures. */
  readonly node: () => ReactNode;
  /** Brings the drawn mount to the state it is read in: its reads answered, a popover open. */
  readonly settle?: (view: RenderResult, user: UserEvent) => Promise<void>;
  /** The roots of what a file outside the shared controls draws inside the mount, read by
   *  selector and by role (the rule that no shared control sets a `title` binds each one's own
   *  elements alone, so `AskCard`'s children of other files, the editor `ProofDocument` hosts and
   *  a Markdown body are out). Each root and what it holds is skipped; an element of a shared
   *  control that a child of another file holds, such as `CopyRefButton`'s `CopyButton`, is
   *  read. */
  readonly outside?: (root: HTMLElement) => readonly Element[];
  /** Whether the mount draws no colour at all (a `Timestamp` alone takes its host's). */
  readonly colourless?: boolean;
  /** The values the mount draws that no `data-author-text` mark finds, each found by its text
   *  (a `MultiSelect`'s option, a route, a login inside the words around it): the wrap case
   *  reads these as values too. */
  readonly values?: readonly string[];
}

/** A drawn mount, settled, with the elements its shared controls draw. */
interface Drawn {
  readonly elements: readonly Element[];
  readonly view: RenderResult;
}

/** Draws `mount` on `surface`, settles it, and reads every element its shared controls draw,
 *  those a portal puts under `document.body` (an open `MultiSelect`'s popover) among them. */
async function drawMount(mount: Mount, surface: Surface): Promise<Drawn> {
  answerDefaults();
  const user = userEvent.setup();
  const view = draw(mount.node(), surface);
  await (mount.settle ?? settleReads)(view, user);
  const skipped = mount.outside?.(view.baseElement) ?? [];
  const elements = [...view.baseElement.querySelectorAll("*")].filter(
    (element) => !skipped.some((root) => root.contains(element))
  );
  return { elements, view };
}

// ---------------------------------------------------------------------------------------------
// What the controls read, each answered by a stub on `api`
// ---------------------------------------------------------------------------------------------

/** The reviews the approval chip's dialog lists (`ApprovalChip.test.tsx`'s, on `notes`). */
const reviews: ArtifactReview[] = [
  {
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-notes",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-10T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: null,
    state: "approved",
    version: 2,
  },
  {
    actor: { id: "bob", kind: "user" },
    artifact_id: "artifact-notes",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-2",
    kind: "person",
    reason: "Please tighten the intro.",
    state: "changes_requested",
    version: 3,
  },
];

/** A GitHub read's answer, as `api.githubRest` hands it. */
function githubAnswer(body: unknown): Response {
  return new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });
}

/** The `api` methods the mounts read, each answered for the whole file and restored after each
 *  test: a read no stub answers reaches the network, which fails the test (`setup.ts`). */
interface Stubs {
  readonly getArchitecture: Mock<typeof api.getArchitecture>;
  readonly getArtifact: Mock<typeof api.getArtifact>;
  readonly getArtifactBlocks: Mock<typeof api.getArtifactBlocks>;
  readonly getArtifactText: Mock<typeof api.getArtifactText>;
  readonly getDocumentRead: Mock<typeof api.getDocumentRead>;
  readonly getIssueDecisions: Mock<typeof api.getIssueDecisions>;
  readonly getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;
  readonly getReferences: Mock<typeof api.getReferences>;
  readonly githubRest: Mock<typeof api.githubRest>;
  readonly listAgents: Mock<typeof api.listAgents>;
  readonly listArtifactReviews: Mock<typeof api.listArtifactReviews>;
  readonly listIssueAsks: Mock<typeof api.listIssueAsks>;
  readonly listIssues: Mock<typeof api.listIssues>;
  readonly listPriorities: Mock<typeof api.listPriorities>;
  readonly listProjectArchitectureSources: Mock<typeof api.listProjectArchitectureSources>;
  readonly listRepoProjects: Mock<typeof api.listRepoProjects>;
  readonly listUsers: Mock<typeof api.listUsers>;
  readonly putDocumentRead: Mock<typeof api.putDocumentRead>;
  readonly whoAmI: Mock<typeof api.whoAmI>;
}

let stubs: Stubs;

beforeEach(() => {
  stubs = {
    getArchitecture: spyOn(api, "getArchitecture"),
    getArtifact: spyOn(api, "getArtifact"),
    getArtifactBlocks: spyOn(api, "getArtifactBlocks"),
    getArtifactText: spyOn(api, "getArtifactText"),
    getDocumentRead: spyOn(api, "getDocumentRead"),
    getIssueDecisions: spyOn(api, "getIssueDecisions"),
    getIssueSubscribers: spyOn(api, "getIssueSubscribers"),
    getReferences: spyOn(api, "getReferences"),
    githubRest: spyOn(api, "githubRest"),
    listAgents: spyOn(api, "listAgents"),
    listArtifactReviews: spyOn(api, "listArtifactReviews"),
    listIssueAsks: spyOn(api, "listIssueAsks"),
    listIssues: spyOn(api, "listIssues"),
    listPriorities: spyOn(api, "listPriorities"),
    listProjectArchitectureSources: spyOn(api, "listProjectArchitectureSources"),
    listRepoProjects: spyOn(api, "listRepoProjects"),
    listUsers: spyOn(api, "listUsers"),
    putDocumentRead: spyOn(api, "putDocumentRead"),
    whoAmI: spyOn(api, "whoAmI"),
  };
  answerDefaults();
});

afterEach(() => {
  for (const stub of Object.values(stubs)) stub.mockRestore();
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
});

const originalClipboard = navigator.clipboard;

/** Each stub's answer for a mount that asks nothing else of it; a mount that needs another
 *  answer sets it in its own `node` or `settle`, and the next mount starts from these again. */
function answerDefaults(): void {
  stubs.getArchitecture.mockResolvedValue(architecture);
  stubs.getArtifact.mockImplementation(async (id) =>
    id === specDocument.id ? specDocument : notes
  );
  stubs.getArtifactBlocks.mockResolvedValue([
    {
      from: 0,
      id: "block-1",
      references: { asks: 1, comments: 1 },
      to: 17,
      token: "sha256:block-1",
      type: "paragraph",
    },
  ]);
  stubs.getArtifactText.mockResolvedValue({
    markdown: "The live document",
    token: "sha256:spec-text",
    version: null,
  });
  stubs.getDocumentRead.mockImplementation(async (id) => ({
    artifact_id: id,
    read_at: null,
    version: null,
  }));
  stubs.getIssueDecisions.mockResolvedValue({ decisions: [], document: null, open_questions: [] });
  stubs.getIssueSubscribers.mockResolvedValue([]);
  stubs.getReferences.mockResolvedValue({
    edges: [],
    node: graphNode({ id: "ask-1", kind: "ask" }),
  });
  stubs.githubRest.mockImplementation(async (path) => {
    if (path === "repos/owner/repository/pulls/7") {
      return githubAnswer({
        head: { sha: "abcdef" },
        merged: false,
        state: "open",
        title: "Ship it",
      });
    }
    if (path.startsWith("repos/owner/repository/commits/abcdef/check-runs")) {
      return githubAnswer({
        check_runs: [
          { check_suite: { id: 1 }, conclusion: "success", id: 1, name: "ci", status: "completed" },
        ],
      });
    }
    if (path.startsWith("repos/owner/repository/actions/runs")) {
      return githubAnswer({ workflow_runs: [{ check_suite_id: 1, event: "pull_request", id: 1 }] });
    }
    throw new Error(`no GitHub answer for ${path}`);
  });
  stubs.listAgents.mockResolvedValue([
    listed(LIVE_SESSION, "Implementer"),
    listed(STALLED_SESSION, "Reviewer", "unresponsive"),
  ]);
  stubs.listArtifactReviews.mockResolvedValue(reviews);
  stubs.listIssueAsks.mockResolvedValue([]);
  stubs.listIssues.mockResolvedValue([]);
  stubs.listPriorities.mockResolvedValue(priorities);
  stubs.listProjectArchitectureSources.mockResolvedValue([]);
  stubs.listRepoProjects.mockResolvedValue([]);
  stubs.listUsers.mockResolvedValue([]);
  stubs.putDocumentRead.mockImplementation(async (id, version) => ({
    artifact_id: id,
    read_at: "2026-09-09T00:00:00Z",
    version,
  }));
  stubs.whoAmI.mockResolvedValue({ event_head: "0", kind: "user", login: "alice" });
}

// ---------------------------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------------------------

/** The document the approval chips speak for. */
const notes = doc("artifact-notes", "notes.md");

function notesWith(approval: ArtifactApproval): Artifact {
  return { ...notes, approval };
}

/** The gate's steps as Details draws them: the spec waits on a request for its version 1, the
 *  decisions were sent back with a reason, and the verification has no document yet. `values`
 *  names the two documents and gives the reason, which the wrap case makes long. */
function stepEntries(
  values = {
    decisions: "decisions.md",
    reason: "The second decision cites no evidence.",
    spec: "spec.md",
  }
): StepEntry[] {
  return [
    {
      document: doc(SPEC_ID, values.spec, {
        latest_version: 2,
        state: "awaiting",
        waiting_on: "human",
      }),
      label: "Spec",
      step: "spec",
    },
    {
      document: doc(DECISIONS_ID, values.decisions, {
        by: { id: "bob", kind: "user" },
        latest_version: 3,
        reason: values.reason,
        state: "changes_requested",
        version: 3,
      }),
      label: "Decisions",
      step: "decisions",
    },
    { document: undefined, label: "Verification", step: "criteria" },
  ];
}

/** An open approval request (`AskCard.test.tsx`'s `approvalAsk`, as `ask-2` at high urgency). */
function openApprovalAsk(): Ask {
  return openAsk({
    approval: {
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 12,
      version: 12,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec-md" },
    id: "ask-2",
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 12)?",
    urgency: "high",
  });
}

/** Two open decisions in the spec, each its own block, which the page's navigator counts. */
function blockAsk(id: string, blockId: string, question: string): Ask {
  return openAsk({
    author: { id: LIVE_SESSION, kind: "session", origin: { session_title: "Architect" } },
    block_artifact: { id: specDocument.id, primary: true, slug: "spec" },
    block_id: blockId,
    id,
    options: [{ label: "Ship" }, { label: "Hold" }],
    question,
    urgency: "high",
  });
}

function subscriber(sessionId: string, title: string, removable: boolean): Subscriber {
  return {
    last_seen: Date.now(),
    live: true,
    removable,
    session_id: sessionId,
    title,
    topics: [removable ? "notifications.dispatch.issue.CORE-1.>" : "notifications.dispatch.>"],
    via: removable ? "notifications.dispatch.issue.CORE-1.>" : "notifications.dispatch.>",
  };
}

/** Opens a popover or a dialog by the control a reader presses. */
function pressing(name: string | RegExp) {
  return async (view: RenderResult, user: UserEvent) => {
    await settleReads();
    await user.click(await within(view.container).findByRole("button", { name }));
    await settleReads();
  };
}

// ---------------------------------------------------------------------------------------------
// Every shared control's code file, and `MultiSelect`, in the states that draw its colours
// ---------------------------------------------------------------------------------------------

/** The roots of what `AskCard`'s children of other files draw (the rule that no shared control
 *  sets a `title` leaves them out, since the new page draws no `AskCard`): each option row
 *  (`AskChoiceRow`, the label inside the options' fieldset), the decline control
 *  (`DeclineControl`, around its button), the recipients (`AskRecipients`), the replies
 *  (`AskThread`) and the question's Markdown body. */
function askCardChildren(root: HTMLElement): Element[] {
  const declines = within(root)
    .queryAllByRole("button", { name: "Decline to answer" })
    .flatMap((button) => (button.parentElement === null ? [] : [button.parentElement]));
  return [
    ...root.querySelectorAll(
      "form fieldset > label, section[aria-label='Recipients'], section[aria-label='Replies'], .dispatch-markdown"
    ),
    ...declines,
  ];
}

/** The editor `ProofDocument` hosts, which the editor fills and the decision blocks portal into:
 *  the element its `root` ref holds, the first under the document's article. */
function documentEditor(root: HTMLElement): Element[] {
  return [...root.querySelectorAll("article[aria-label='Document'] > div:first-child")];
}

/** A Markdown body (`MarkdownBody`), which draws a reviewer's reason. */
function markdownBodies(root: HTMLElement): Element[] {
  return [...root.querySelectorAll(".dispatch-markdown")];
}

const MOUNTS: readonly Mount[] = [
  {
    name: "CopyButton, copied and refused",
    node: () => (
      <>
        <CopyButton value="session-1" what="session ID">
          ID
        </CopyButton>
        <CopyButton value="CORE-1" what="issue key" />
      </>
    ),
    settle: async (view, user) => {
      Object.defineProperty(navigator, "clipboard", {
        configurable: true,
        value: { writeText: async () => undefined },
      });
      await user.click(
        within(view.container).getByRole("button", { name: "Copy session ID session-1" })
      );
      Object.defineProperty(navigator, "clipboard", {
        configurable: true,
        value: { writeText: async () => Promise.reject(new Error("denied")) },
      });
      await user.click(
        within(view.container).getByRole("button", { name: "Copy issue key CORE-1" })
      );
      await within(view.container).findByText("Copied");
      await within(view.container).findByText(/Copy failed/);
    },
  },
  {
    name: "MultiSelect, open with a pick, details and a refusal",
    node: () => (
      <MultiSelect
        emptyMessage="No labels yet."
        error="Labels must be 1–40 characters."
        label="Labels"
        onChange={() => undefined}
        onOpenChange={() => undefined}
        open
        optionDetail={(value) => (value === "api" ? "3" : undefined)}
        options={["api", "docs"]}
        searchLabel="Search labels"
        selected={["docs"]}
      />
    ),
  },
  {
    name: "MultiSelect, open with nothing to pick",
    node: () => (
      <MultiSelect
        emptyMessage="No labels yet."
        label="Labels"
        onChange={() => undefined}
        onOpenChange={() => undefined}
        open
        options={[]}
        searchLabel="Search labels"
        selected={[]}
      />
    ),
  },
  {
    name: "Pill, LabelPill and StatusPill",
    node: () => (
      <>
        <Pill>3</Pill>
        <LabelPill>api</LabelPill>
        <LabelPill selected>2 unread replies</LabelPill>
        <StatusPill>In progress</StatusPill>
      </>
    ),
  },
  {
    name: "PinButton, pinned, unpinned and quiet",
    node: () => (
      <>
        <PinButton label="Unpin CORE-1" onClick={() => undefined} pinned />
        <PinButton label="Pin CORE-1" onClick={() => undefined} pinned={false} />
        <PinButton label="Pin this turn" onClick={() => undefined} pinned={false} quiet />
      </>
    ),
  },
  {
    name: "RefusableButton, primary refused and compact secondary",
    node: () => (
      <>
        <RefusableButton refusal="Write a reason first.">Send</RefusableButton>
        <RefusableButton look="secondary-compact" refusal={null}>
          Attach
        </RefusableButton>
      </>
    ),
  },
  {
    name: "ApprovalChip, each state, and the Request changes dialog",
    node: () => (
      <>
        <ApprovalChip
          artifact={notesWith({ latest_version: 3, state: "awaiting" })}
          variant="header"
        />
        <ApprovalChip
          artifact={notesWith({ latest_version: 5, state: "stale", version: 3 })}
          size="compact"
          variant="header"
        />
        <ApprovalChip artifact={notesWith({ latest_version: 3, state: "approved", version: 3 })} />
      </>
    ),
    settle: async (view, user) => {
      const [requestChanges] = within(view.container).getAllByRole("button", {
        name: "Request changes",
      });
      if (requestChanges === undefined) throw new Error("no Request changes");
      await user.click(requestChanges);
      await within(view.container).findByRole("dialog", { name: "Request changes" });
    },
  },
  {
    name: "ApprovalChip, the Reviews dialog",
    node: () => (
      <ApprovalChip
        artifact={notesWith({ latest_version: 3, state: "changes_requested", version: 3 })}
      />
    ),
    outside: markdownBodies,
    settle: async (view, user) => {
      await user.click(within(view.container).getByRole("button", { name: "Changes requested" }));
      const dialog = await within(view.container).findByRole("dialog", { name: "Reviews" });
      await within(dialog).findByText("Please tighten the intro.");
      await settleReads();
    },
  },
  {
    name: "ConnectionDot, each state and a pending notice",
    node: () => (
      <>
        <ConnectionDot connection="connected" pending={undefined} />
        <ConnectionDot connection="connecting" pending={undefined} />
        <ConnectionDot connection="failed" pending={undefined} />
        <ConnectionDot
          connection="offline"
          pending={{ count: 2, readOnly: false, rebuiltAt: undefined, stored: true }}
        />
      </>
    ),
  },
  {
    name: "ProofDocument, two open decisions and a block's references",
    node: () => {
      stubs.listIssueAsks.mockResolvedValue([
        { ...blockAsk("ask-b1", "b-1", "Should we ship?"), last_reply: null },
        { ...blockAsk("ask-b2", "b-2", "Which region?"), last_reply: null },
      ]);
      return proofDocument();
    },
    outside: documentEditor,
    settle: async (view) => {
      await within(view.container).findByRole("navigation", { name: "Open decisions" });
      await within(view.container).findByRole("button", { name: "2 references on block" });
    },
  },
  {
    name: "ProofDocument, a history that cannot load",
    node: () => {
      stubs.getArtifactText.mockRejectedValue(
        new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: "The stored history cannot load." })
      );
      return proofDocument();
    },
    outside: documentEditor,
    settle: async (view) => {
      await within(view.container).findByRole("button", {
        name: "Rebuild from the latest version",
      });
    },
  },
  {
    name: "AskCard, an open blocking question with its session's handles open",
    node: () => {
      const ask = openAsk();
      return (
        <AskCard ask={ask} getAskThread={async () => threadOf(ask)} initialThread={threadOf(ask)} />
      );
    },
    outside: askCardChildren,
    settle: async (view, user) => {
      await settleReads();
      await user.click(
        within(view.container).getByRole("button", { expanded: false, name: /Implementer/ })
      );
      await within(view.container).findByRole("button", { name: /Copy session ID/ });
    },
  },
  {
    name: "AskCard, an open approval request, compact",
    node: () => {
      const ask = openApprovalAsk();
      return (
        <AskCard
          ask={ask}
          getAskThread={async () => threadOf(ask)}
          initialThread={threadOf(ask)}
          variant="compact"
        />
      );
    },
    outside: askCardChildren,
  },
  {
    name: "IssuePriorityLine, its own priority and an inherited one",
    node: () => (
      <IssuePriorityLine
        disabled={false}
        issue={{
          advances: [
            { id: "rocq-lean", inherited: false },
            { id: "evals-now", inherited: true },
          ],
          advances_revision: 1,
          key: "CORE-1",
        }}
      />
    ),
    settle: async (view) => {
      await within(view.container).findByRole("link", { name: /Rocq and Lean/ });
    },
  },
  {
    name: "IssuePriorityLine, adding a priority",
    node: () => (
      <IssuePriorityLine
        disabled={false}
        issue={{ advances: [], advances_revision: 1, key: "CORE-1" }}
      />
    ),
    settle: async (view, user) => {
      await user.click(
        within(view.container).getByRole("button", { name: "Add a priority to CORE-1" })
      );
      await within(view.container).findByRole("combobox", { name: "Priority to advance" });
      await settleReads();
    },
  },
  {
    name: "IssuePriorityLine, in a row of Details",
    node: () => (
      <>
        <IssuePriorityLine
          disabled={false}
          inRow
          issue={{
            advances: [{ id: "rocq-lean", inherited: false }],
            advances_revision: 1,
            key: "CORE-1",
          }}
        />
        <IssuePriorityLine
          disabled={false}
          inRow
          issue={{ advances: [], advances_revision: 1, key: "CORE-2" }}
        />
      </>
    ),
  },
  {
    name: "AgentName, each state the registry reads and each shape",
    node: () => (
      <>
        <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} />
        <AgentName actor={{ id: STALLED_SESSION, kind: "session" }} inline />
        <AgentName sessionId={GONE_SESSION} storedTitle="Old planner" />
        <AgentName
          actor={{ id: LIVE_SESSION, kind: "session" }}
          control={(name) => <button type="button">{name}</button>}
          kind="control"
        />
        <AgentName actor={{ id: "alice", kind: "user" }} />
        <AgentUnresponsiveNote lastSeen={Date.now() - 10 * 60_000} state="unresponsive" />
      </>
    ),
    settle: async (view) => {
      await waitFor(() =>
        expect(view.container.querySelector(`[data-agent-state="unresponsive"]`)).not.toBeNull()
      );
    },
  },
  {
    name: "AgentName and ClaimChip, with no registry read",
    node: () => {
      stubs.listAgents.mockRejectedValue(new Error("Envoy listener unreachable"));
      return (
        <>
          <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} />
          <ClaimChip claim={{ actor: { id: LIVE_SESSION, kind: "session" }, at: CLAIMED_AT }} />
        </>
      );
    },
    settle: async (view) => {
      await waitFor(() =>
        expect(view.container.querySelector(`[data-agent-state="unknown"]`)).not.toBeNull()
      );
      await settleReads();
    },
  },
  {
    name: "Timestamp, exact",
    node: () => <Timestamp at={CLAIMED_AT} exact />,
    colourless: true,
  },
  {
    name: "ApprovalSteps, Details' size with versions and reasons",
    node: () => (
      <ApprovalSteps
        entries={stepEntries()}
        primaryArtifactId={SPEC_ID}
        versions={{ asks: [specRequest], issueKey: "CORE-1" }}
      />
    ),
  },
  {
    name: "ApprovalSteps, the header's compact size",
    node: () => (
      <ApprovalSteps
        entries={stepEntries()}
        primaryArtifactId={SPEC_ID}
        reasons={false}
        size="compact"
      />
    ),
  },
  {
    name: "AssigneeControl, both variants, assigned and not",
    node: () => (
      <>
        <AssigneeControl assignee="alice@example.com" issueKey="CORE-1" />
        <AssigneeControl assignee={null} issueKey="CORE-2" />
        <AssigneeControl assignee="alice@example.com" issueKey="CORE-3" variant="header" />
        <AssigneeControl assignee={null} issueKey="CORE-4" variant="header" />
      </>
    ),
  },
  {
    name: "PriorityControl and PriorityEditor, both variants, set and not",
    node: () => (
      <>
        <PriorityControl issueKey="CORE-1" priority={0} />
        <PriorityControl issueKey="CORE-2" priority={null} />
        <PriorityEditor
          issueKey="CORE-3"
          variant="header"
          write={{ ...idlePriorityWrite, shown: 1 }}
        />
        <PriorityEditor issueKey="CORE-4" variant="header" write={idlePriorityWrite} />
      </>
    ),
  },
  {
    name: "ClaimChip, held, lapsed, unresponsive and in a row of Details",
    node: () => (
      <>
        <ClaimChip claim={{ actor: { id: LIVE_SESSION, kind: "session" }, at: CLAIMED_AT }} />
        <ClaimChip claim={{ actor: { id: GONE_SESSION, kind: "session" }, at: CLAIMED_AT }} />
        <ClaimChip
          claim={{ actor: { id: STALLED_SESSION, kind: "session" }, at: CLAIMED_AT }}
          liveness={{
            holder_state: "unresponsive",
            last_seen: minutesFromNow(-10),
            takeable_at: minutesFromNow(20),
          }}
        />
        <ClaimChip
          claim={{ actor: { id: STALLED_SESSION, kind: "session" }, at: CLAIMED_AT }}
          liveness={{
            holder_state: "unresponsive",
            last_seen: minutesFromNow(-90),
            takeable_at: minutesFromNow(-30),
          }}
        />
        <ClaimChip claim={{ actor: { id: LIVE_SESSION, kind: "session" }, at: CLAIMED_AT }} inRow />
      </>
    ),
    settle: async (view) => {
      await within(view.container).findByTestId("issue-claim-lapsed");
      await settleReads();
    },
  },
  {
    name: "GitHubLink, a pull request, a plain link and an unsafe one",
    node: () => (
      <>
        <GitHubLink link={{ url: "https://github.com/owner/repository/pull/7" }} />
        <GitHubLink link={{ url: "https://example.com/notes" }} />
        <GitHubLink link={{ url: "javascript:alert(1)" }} />
      </>
    ),
    settle: async (view) => {
      await within(view.container).findByText(/checks/);
    },
  },
  {
    name: "IssueComponentsLine, an explicit set, inherited, none and not attached",
    node: () => (
      <>
        <IssueComponentsLine
          issue={{
            components: {
              ids: ["dispatch-server"],
              inherited_from: "CORE-9",
              mode: "explicit",
              reason: null,
              unknown: ["old-queue"],
            },
            key: "CORE-1",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
        <IssueComponentsLine
          issue={{
            components: {
              ids: [],
              inherited_from: null,
              mode: "none",
              reason: "A process change",
              unknown: [],
            },
            key: "CORE-2",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
        <IssueComponentsLine
          issue={{
            components: {
              ids: [],
              inherited_from: null,
              mode: "inherit",
              reason: null,
              unknown: [],
            },
            key: "CORE-3",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
        <IssueComponentsLine
          inRow
          issue={{
            components: {
              ids: ["dispatch-server"],
              inherited_from: null,
              mode: "explicit",
              reason: null,
              unknown: [],
            },
            key: "CORE-4",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
      </>
    ),
  },
  {
    name: "IssueLabels, its chips and its picker open",
    node: () => (
      <IssueLabels
        disabled={false}
        issueKey="CORE-1"
        labels={["api", "frontend"]}
        onSave={async () => undefined}
        project="CORE"
        saving={false}
      />
    ),
    // With a label the control draws "+", and its name is `main`'s on both surfaces, key given
    // or not; only an issue with no label, and inside `NoTooltips` a control that saves, is named
    // by the words it draws (`labelsEmptyAndSaving`).
    settle: pressing("Edit labels"),
  },
  {
    name: "RepoControl, its own repository and an inherited one, out of a row of Details and in one, and none in one",
    node: () => (
      <>
        <RepoControl issueKey="CORE-1" project="CORE" repo="acme/api" resolvedRepo="acme/api" />
        <RepoControl issueKey="CORE-2" project="CORE" repo={null} resolvedRepo="acme/web" />
        <RepoControl inRow issueKey="CORE-3" project="CORE" repo={null} resolvedRepo={null} />
        <RepoControl
          inRow
          issueKey="CORE-4"
          project="CORE"
          repo="acme/app"
          resolvedRepo="acme/app"
        />
        <RepoControl inRow issueKey="CORE-5" project="CORE" repo={null} resolvedRepo="acme/site" />
      </>
    ),
  },
  {
    name: "RouteReach, each marker",
    node: () => routeMarkers(),
  },
  {
    name: "SubscribedAgents, one removable and one subscribed more widely",
    node: () => (
      <SubscribedAgents
        onUnsubscribe={() => undefined}
        ownerLabel="CORE-1"
        subscribers={[
          subscriber(LIVE_SESSION, "Implementer", true),
          subscriber(STALLED_SESSION, "Reviewer", false),
        ]}
      />
    ),
  },
];

/** Every marker `RouteReach.tsx` draws, each by its test id: the role nobody holds is `nobody`,
 *  and the role an unresponsive session holds `unresponsive`, which the wrap case makes long. */
function routeMarkers(nobody = "role:architect", unresponsive = "role:reviewer"): ReactNode {
  return (
    <>
      <RouteReachUnknownMarker testId="route-unknown" />
      <UnreachableRouteMarker
        issue={{
          route: nobody,
          route_holder: null,
          route_status: "no_holder",
          route_status_reason: null,
        }}
        showRoute
        testId="route-nobody"
      />
      <UnreachableRouteMarker
        issue={{
          route: `session:${GONE_SESSION}`,
          route_holder: null,
          route_status: "no_holder",
          route_status_reason: null,
        }}
        showRoute
        testId="route-not-running"
      />
      <UnresponsiveRouteMarker
        issue={{
          route: unresponsive,
          route_holder: STALLED_SESSION,
          route_status: "holder_unresponsive",
          route_status_reason: null,
        }}
        showRoute
      />
    </>
  );
}

// ---------------------------------------------------------------------------------------------
// The states that set the rest of `main`'s titles and draw the rest of the page's words
// ---------------------------------------------------------------------------------------------

/** Fixed instants for the mounts whose `title` or drawn text says one, so each reads the same on
 *  every run: beside the claim's `CLAIMED_AT`, the heartbeat its holder was last seen at, and the
 *  moment the claim comes free, already past (`FREED_AT`) or still ahead (`FREE_FROM`). */
const HEARD_AT = "2026-09-24T05:50:00Z";
const FREED_AT = "2026-09-24T06:20:00Z";
const FREE_FROM = "2099-09-24T06:00:00Z";

/** An instant as `main`'s `title`s say it: `Timestamp`'s `date.toLocaleString()`. */
function localTime(iso: string): string {
  return new Date(iso).toLocaleString();
}

/** An instant as a control draws it inside `NoTooltips`, where no `title` says it: the date and
 *  the time, each in its short form (`Timestamp`'s `exact`, `ClaimChip`'s free moment). */
function shortTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" });
}

/** How each session of this file reads where nothing names it: `session:` and the first eight
 *  characters of its id (`shortSessionId`), which all three share. */
const UNNAMED_SESSION = "session:session-…";

/** A person's act made through an agent (`shared-controls.options.test.tsx`'s `throughAgent`). */
const THROUGH_AGENT: Actor = { act: { session: LIVE_SESSION }, id: "alice", kind: "user" };

const pinWithTitle: Mount = {
  name: "PinButton, with a tooltip shorter than its name",
  node: () => (
    <PinButton
      label="Pin CORE-1, Ship the new issue page"
      onClick={() => undefined}
      pinned={false}
      title="Pin CORE-1"
    />
  ),
};

const personAsksForApproval: Mount = {
  name: "AskCard, an approval a person asked for, with Request changes picked",
  node: () => {
    const ask: Ask = { ...openApprovalAsk(), author: { id: "alice", kind: "user" } };
    return (
      <AskCard ask={ask} getAskThread={async () => threadOf(ask)} initialThread={threadOf(ask)} />
    );
  },
  outside: askCardChildren,
  settle: async (view, user) => {
    await settleReads();
    await user.click(within(view.container).getByRole("radio", { name: "Request changes" }));
    await settleReads();
  },
};

const agentsAtHeartbeat: Mount = {
  name: "AgentName, each state, a session nothing names, and an unresponsive note at a fixed heartbeat",
  node: () => (
    <>
      <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} />
      <AgentName actor={{ id: STALLED_SESSION, kind: "session" }} inline />
      <AgentName sessionId={GONE_SESSION} storedTitle="Old planner" />
      <AgentName sessionId={GONE_SESSION} storedTitle={undefined} />
      <AgentName
        actor={{ id: LIVE_SESSION, kind: "session" }}
        control={(name) => <button type="button">{name}</button>}
        kind="control"
      />
      <AgentUnresponsiveNote lastSeen={Date.parse(HEARD_AT)} state="unresponsive" />
    </>
  ),
  settle: async (view) => {
    await waitFor(() =>
      expect(view.container.querySelector(`[data-agent-state="unresponsive"]`)).not.toBeNull()
    );
  },
};

const agentsAsAuthors: Mount = {
  name: "AgentName as an author: a person, a person acting through an agent, an agent nothing names",
  node: () => (
    <>
      <AgentName actor={{ id: "alice", kind: "user" }} />
      <AgentName actor={THROUGH_AGENT} author />
      <AgentName actor={{ id: GONE_SESSION, kind: "session" }} author />
    </>
  ),
  settle: async (view) => {
    await waitFor(() =>
      expect(view.container.querySelector(`[data-agent-state="not-running"]`)).not.toBeNull()
    );
  },
};

const claimsAtFixedInstants: Mount = {
  name: "ClaimChip, held, lapsed, unresponsive and free to take, at fixed instants",
  node: () => (
    <>
      <ClaimChip claim={{ actor: { id: LIVE_SESSION, kind: "session" }, at: CLAIMED_AT }} />
      <ClaimChip claim={{ actor: { id: GONE_SESSION, kind: "session" }, at: CLAIMED_AT }} />
      <ClaimChip
        claim={{ actor: { id: STALLED_SESSION, kind: "session" }, at: CLAIMED_AT }}
        liveness={{ holder_state: "unresponsive", last_seen: HEARD_AT, takeable_at: FREE_FROM }}
      />
      <ClaimChip
        claim={{ actor: { id: STALLED_SESSION, kind: "session" }, at: CLAIMED_AT }}
        liveness={{ holder_state: "unresponsive", last_seen: HEARD_AT, takeable_at: FREED_AT }}
      />
    </>
  ),
  settle: async (view) => {
    await within(view.container).findByTestId("issue-claim-lapsed");
    await settleReads();
  },
};

const githubUnread: Mount = {
  name: "GitHubLink, an issue whose read is still out and a pull request Dispatch holds no token for",
  node: () => {
    stubs.githubRest.mockImplementation((path) => {
      if (path === "repos/owner/repository/issues/8") {
        return new Promise<Response>(() => undefined);
      }
      if (path === "repos/acme/private/pulls/9") {
        return Promise.reject(
          new ApiError(503, { code: "GITHUB_TOKEN_UNAVAILABLE", error: "no installation" })
        );
      }
      return Promise.reject(new Error(`no GitHub answer for ${path}`));
    });
    return (
      <>
        <GitHubLink link={{ url: "https://github.com/owner/repository/issues/8" }} />
        <GitHubLink link={{ url: "https://github.com/acme/private/pull/9" }} />
      </>
    );
  },
};

/** The priorities an issue at the cap advances, each its own, none of them named by a read. */
const PRIORITIES_AT_CAP = Array.from(
  { length: MAX_ISSUE_ADVANCES },
  (_, index) => `priority-${index + 1}`
);

const priorityLineAtCap: Mount = {
  name: "IssuePriorityLine, at the cap, with its priorities unread",
  node: () => {
    stubs.listPriorities.mockRejectedValue(new Error("the priorities read failed"));
    return (
      <IssuePriorityLine
        disabled={false}
        issue={{
          advances: PRIORITIES_AT_CAP.map((id) => ({ id, inherited: false })),
          advances_revision: 1,
          key: "CORE-1",
        }}
      />
    );
  },
  settle: async (view) => {
    await within(view.container).findByRole("alert");
  },
};

const priorityLineLoading: Mount = {
  name: "IssuePriorityLine, adding while its priorities are still read",
  node: () => {
    stubs.listPriorities.mockReturnValue(new Promise<Priorities>(() => undefined));
    return (
      <IssuePriorityLine
        disabled={false}
        issue={{ advances: [], advances_revision: 1, key: "CORE-1" }}
      />
    );
  },
  settle: async (view, user) => {
    await user.click(
      within(view.container).getByRole("button", { name: "Add a priority to CORE-1" })
    );
    await within(view.container).findByRole("combobox", { name: "Priority to advance" });
  },
};

const repoNothingResolved: Mount = {
  name: "RepoControl, nothing resolved in a project that maps a repository",
  node: () => {
    stubs.listRepoProjects.mockResolvedValue([
      {
        created_at: "2026-10-01T00:00:00Z",
        created_by: { id: "alice", kind: "user" },
        default: true,
        project: "CORE",
        renamed_to: null,
        repo: "acme/api",
      },
    ]);
    return <RepoControl issueKey="CORE-5" project="CORE" repo={null} resolvedRepo={null} />;
  },
  settle: async (view) => {
    await within(view.container).findByRole("combobox", { name: "Repository of CORE-5" });
  },
};

const labelsEmptyAndSaving: Mount = {
  name: "IssueLabels, with no labels, and saving",
  node: () => (
    <>
      <IssueLabels
        disabled={false}
        issueKey="CORE-1"
        labels={[]}
        onSave={async () => undefined}
        project="CORE"
        saving={false}
      />
      <IssueLabels
        disabled={false}
        labels={["api"]}
        onSave={async () => undefined}
        project="CORE"
        saving
      />
    </>
  ),
};

/** The components line's picker is drawn for a project with an architecture source alone. */
const componentsWithPicker: Mount = {
  name: "IssueComponentsLine, with its picker, on an explicit set and on none attached",
  node: () => {
    stubs.listProjectArchitectureSources.mockResolvedValue([
      {
        branch: "main",
        components: 1,
        created_at: "2026-09-10T00:00:00Z",
        created_by: { id: "alice", kind: "user" },
        enabled: true,
        last_commit: "3be61df5be90",
        last_error: null,
        last_sync_at: "2026-09-17T00:00:00Z",
        project: "CORE",
        repo: "legion/legion",
      },
    ]);
    return (
      <>
        <IssueComponentsLine
          issue={{
            components: {
              ids: ["dispatch-server"],
              inherited_from: null,
              mode: "explicit",
              reason: null,
              unknown: [],
            },
            key: "CORE-1",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
        <IssueComponentsLine
          issue={{
            components: {
              ids: [],
              inherited_from: null,
              mode: "inherit",
              reason: null,
              unknown: [],
            },
            key: "CORE-2",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
      </>
    );
  },
  // The picker's name is `main`'s outside `NoTooltips` and begins with the words its trigger
  // draws inside, then the word list's joining word (`componentsControlName`), and either ends
  // with the issue's key.
  settle: async (view) => {
    const names = [
      "Edit components of CORE-2",
      `${CONTROL_LABELS.components_set} ${CONTROL_LABELS.control_name_of} CORE-2`,
    ];
    await within(view.container).findByRole("button", { name: (name) => names.includes(name) });
  },
};

/** The components picker while the set its popover closed with is saving, as the components line
 *  draws it with its write out: in place of the host's words the trigger says that it saves. */
const componentPickerSaving: Mount = {
  name: "ComponentPicker, saving",
  node: () => {
    const components = architecture.components;
    if (components === null) throw new Error("the fixture has no components to offer");
    return (
      <ComponentPicker
        components={components}
        onSave={() => undefined}
        saving
        selected={[]}
        triggerAriaLabel={componentsControlName("CORE-1", {
          components: { inherited_from: null, mode: "inherit" },
          saving: true,
          words: { ANSWER_LINES, CONTROL_LABELS },
        })}
      >
        Set components
      </ComponentPicker>
    );
  },
};

const subscribersAtHeartbeat: Mount = {
  name: "SubscribedAgents, one removable and one subscribed more widely, seen at a fixed heartbeat",
  node: () => (
    <SubscribedAgents
      onUnsubscribe={() => undefined}
      ownerLabel="CORE-1"
      subscribers={[
        { ...subscriber(LIVE_SESSION, "Implementer", true), last_seen: Date.parse(HEARD_AT) },
        { ...subscriber(STALLED_SESSION, "Reviewer", false), last_seen: Date.parse(HEARD_AT) },
      ]}
    />
  ),
};

/** Every mount of this file: `MOUNTS`, which the colour case reads, and the states above. */
const EVERY_MOUNT: readonly Mount[] = [
  ...MOUNTS,
  pinWithTitle,
  personAsksForApproval,
  agentsAtHeartbeat,
  agentsAsAuthors,
  claimsAtFixedInstants,
  githubUnread,
  priorityLineAtCap,
  priorityLineLoading,
  repoNothingResolved,
  componentsWithPicker,
  componentPickerSaving,
  labelsEmptyAndSaving,
  subscribersAtHeartbeat,
];

/** The mount of `MOUNTS` named `name`. */
function mountNamed(name: string): Mount {
  const mount = MOUNTS.find((candidate) => candidate.name === name);
  if (mount === undefined) throw new Error(`no mount named ${name}`);
  return mount;
}

/** Each `title` the elements carry, in document order. */
function titlesOf(elements: readonly Element[]): string[] {
  return elements.flatMap((element) => {
    const title = element.getAttribute("title");
    return title === null ? [] : [title];
  });
}

/** What `main`'s `title`s say again and again: a running and an unresponsive session's state
 *  mark three minutes after its heartbeat (`listed`), what a claim is, and why a route's
 *  vacancy may pass. */
const RUNNING_READING = "Running · seen 3 minutes ago";
const UNRESPONSIVE_READING =
  "Unresponsive · seen 3 minutes ago · it has missed its heartbeats without ending: it keeps its roles and claims, and messages are stored for it";
const CLAIM_EXPLAINS =
  "A claim marks who is implementing this issue, so two agents never take the same work.";
const VACANCY =
  ", so this issue's messages reach nobody at the moment. A session that is restarting or moving between boxes drops out of the listener for minutes and comes back under the same id, so this is a vacancy only when it is still true on a read ten minutes later; then staff the role, re-route it to a live holder, or clear the route and assign it.";

/**
 * Every `title` each mount carries on `main`, in document order, as `main`'s files write them
 * (read at the merge-base), and none for a mount whose files set none. The three mounts of
 * `MOUNTS` whose instants move with the clock are read here through their twins at fixed
 * instants.
 */
const MAIN_TITLES: ReadonlyMap<Mount, readonly string[]> = new Map([
  [
    mountNamed("CopyButton, copied and refused"),
    ["Copy session ID session-1", "Copy issue key CORE-1"],
  ],
  [mountNamed("MultiSelect, open with a pick, details and a refusal"), ["api", "docs"]],
  [mountNamed("MultiSelect, open with nothing to pick"), []],
  [mountNamed("Pill, LabelPill and StatusPill"), []],
  [
    mountNamed("PinButton, pinned, unpinned and quiet"),
    ["Unpin CORE-1", "Pin CORE-1", "Pin this turn"],
  ],
  [pinWithTitle, ["Pin CORE-1"]],
  [mountNamed("RefusableButton, primary refused and compact secondary"), ["Write a reason first."]],
  [
    mountNamed("ApprovalChip, each state, and the Request changes dialog"),
    ["Approved at version 3; the latest version is 5"],
  ],
  [
    mountNamed("ApprovalChip, the Reviews dialog"),
    [localTime("2026-09-10T00:00:00Z"), localTime("2026-09-11T00:00:00Z")],
  ],
  [
    mountNamed("ConnectionDot, each state and a pending notice"),
    [
      "connected",
      "Connecting to the document…",
      "The document could not load",
      "offline · 2 edits saved in this browser, not sent yet.",
    ],
  ],
  [mountNamed("ProofDocument, two open decisions and a block's references"), ["Should we ship?"]],
  [mountNamed("ProofDocument, a history that cannot load"), []],
  [
    mountNamed("AskCard, an open blocking question with its session's handles open"),
    [
      RUNNING_READING,
      "Implementer · session-live-0001",
      "Open live conversation with Implementer",
      localTime("2026-10-08T00:00:00Z"),
      "Copy reference dispatch://CORE-1/ask/ask-1",
      "Copy session ID session-live-0001",
      "Copy session title Implementer",
      "Copy tmux target legion:3",
    ],
  ],
  [
    mountNamed("AskCard, an open approval request, compact"),
    [
      RUNNING_READING,
      "Implementer · session-live-0001",
      "Open live conversation with Implementer",
      localTime("2026-10-08T00:00:00Z"),
      "Copy reference dispatch://CORE-1/ask/ask-2",
    ],
  ],
  [
    personAsksForApproval,
    [
      "alice",
      localTime("2026-10-08T00:00:00Z"),
      "Copy reference dispatch://CORE-1/ask/ask-2",
      "Add a reason to send Request changes",
    ],
  ],
  [
    mountNamed("IssuePriorityLine, its own priority and an inherited one"),
    [
      "P2 Rocq and Lean speed work restarted",
      "P1 Evals collecting data now",
      "Add a priority to CORE-1",
    ],
  ],
  [mountNamed("IssuePriorityLine, adding a priority"), []],
  [
    mountNamed("IssuePriorityLine, in a row of Details"),
    [
      "P2 Rocq and Lean speed work restarted",
      "Add a priority to CORE-1",
      "Add a priority to CORE-2",
    ],
  ],
  [priorityLineAtCap, PRIORITIES_AT_CAP],
  [priorityLineLoading, []],
  [
    agentsAtHeartbeat,
    [
      RUNNING_READING,
      UNRESPONSIVE_READING,
      "Not running",
      GONE_SESSION,
      "Not running",
      RUNNING_READING,
      "Open live conversation with Implementer",
      `This session has missed its heartbeats since ${localTime(HEARD_AT)}. It has not ended: it keeps its roles and claims, and messages sent to it are stored until it answers.`,
      localTime(HEARD_AT),
    ],
  ],
  [agentsAsAuthors, [GONE_SESSION, "Not running"]],
  [
    mountNamed("AgentName and ClaimChip, with no registry read"),
    [
      LIVE_SESSION,
      "Running state unknown",
      `Claimed by ${UNNAMED_SESSION} · since ${localTime(CLAIMED_AT)}. ${CLAIM_EXPLAINS} Couldn't check whether that session is still running: Envoy listener unreachable.`,
      LIVE_SESSION,
      "Running state unknown",
      localTime(CLAIMED_AT),
    ],
  ],
  [mountNamed("Timestamp, exact"), [localTime(CLAIMED_AT)]],
  [mountNamed("ApprovalSteps, Details' size with versions and reasons"), []],
  [mountNamed("ApprovalSteps, the header's compact size"), []],
  [mountNamed("AssigneeControl, both variants, assigned and not"), []],
  [mountNamed("PriorityControl and PriorityEditor, both variants, set and not"), []],
  [
    claimsAtFixedInstants,
    [
      `Claimed by Implementer · since ${localTime(CLAIMED_AT)}. ${CLAIM_EXPLAINS}`,
      RUNNING_READING,
      localTime(CLAIMED_AT),
      `Claimed by ${UNNAMED_SESSION} · since ${localTime(CLAIMED_AT)}. ${CLAIM_EXPLAINS} That session is no longer running, so anyone may claim it.`,
      GONE_SESSION,
      "Not running",
      localTime(CLAIMED_AT),
      `Claimed by Reviewer · since ${localTime(CLAIMED_AT)}. ${CLAIM_EXPLAINS} That session has missed its heartbeats without ending, last seen ${localTime(HEARD_AT)}. It keeps this issue until ${localTime(FREE_FROM)}, when another agent may take it.`,
      UNRESPONSIVE_READING,
      localTime(CLAIMED_AT),
      localTime(HEARD_AT),
      localTime(FREE_FROM),
      `Claimed by Reviewer · since ${localTime(CLAIMED_AT)}. ${CLAIM_EXPLAINS} That session has missed its heartbeats without ending, last seen ${localTime(HEARD_AT)}. It kept this issue until ${localTime(FREED_AT)}, so another agent may take it now. It has not ended, and may yet answer.`,
      UNRESPONSIVE_READING,
      localTime(CLAIMED_AT),
      localTime(HEARD_AT),
    ],
  ],
  [
    mountNamed("GitHubLink, a pull request, a plain link and an unsafe one"),
    ["owner/repository#7: Ship it", "https://example.com/notes", "Unsafe external link"],
  ],
  [
    githubUnread,
    [
      "https://github.com/owner/repository/issues/8",
      "GitHub details are unavailable for this repository.",
    ],
  ],
  [
    mountNamed("IssueComponentsLine, an explicit set, inherited, none and not attached"),
    ["old-queue is no longer in the project's architecture model"],
  ],
  [mountNamed("IssueLabels, its chips and its picker open"), ["api", "frontend"]],
  [labelsEmptyAndSaving, []],
  [
    mountNamed(
      "RepoControl, its own repository and an inherited one, out of a row of Details and in one, and none in one"
    ),
    // Outside `NoTooltips` a row of Details draws `main`'s box, its clear control with it.
    [
      "Clear it, and the issue inherits a repository again",
      "Clear it, and the issue inherits a repository again",
    ],
  ],
  [repoNothingResolved, []],
  [componentsWithPicker, []],
  [componentPickerSaving, []],
  [
    mountNamed("RouteReach, each marker"),
    [
      "The Envoy listener did not answer, so Dispatch cannot tell whether anyone receives this route.",
      `No running session holds role:architect right now${VACANCY}`,
      `${UNNAMED_SESSION} is not running right now${VACANCY}`,
      GONE_SESSION,
      "Not running",
      "Reviewer has missed its heartbeats without ending, so it still holds this route: what is sent to this issue is stored for it and waits until it answers. Nothing has to be done about it unless it stays this way - a session past the listener's gone horizon leaves the role unheld, and the route then reads as reaching nobody.",
    ],
  ],
  [
    subscribersAtHeartbeat,
    [
      LIVE_SESSION,
      RUNNING_READING,
      localTime(HEARD_AT),
      STALLED_SESSION,
      UNRESPONSIVE_READING,
      localTime(HEARD_AT),
      "Subscribed only via notifications.dispatch.>, which also covers other issues and documents — cannot unsubscribe just this one",
    ],
  ],
]);

/** The mounts of `MOUNTS` whose instants move with the clock, which `MAIN_TITLES` reads through
 *  their twins above (`agentsAtHeartbeat`, `claimsAtFixedInstants`, `subscribersAtHeartbeat`). */
const MOVING_INSTANTS: readonly Mount[] = [
  mountNamed("AgentName, each state the registry reads and each shape"),
  mountNamed("ClaimChip, held, lapsed, unresponsive and in a row of Details"),
  mountNamed("SubscribedAgents, one removable and one subscribed more widely"),
];

/** Draws `mount` on `surface`, settled, hands its view and the elements its shared controls
 *  draw to `read`, and takes it off the page. */
async function readDrawn(
  mount: Mount,
  surface: Surface,
  read: (view: RenderResult, elements: readonly Element[]) => void
): Promise<void> {
  const { elements, view } = await drawMount(mount, surface);
  try {
    read(view, elements);
  } finally {
    undraw(view);
  }
}

/** The link to `href` that a mount draws. */
function linkTo(root: HTMLElement, href: string): HTMLAnchorElement {
  const link = root.querySelector<HTMLAnchorElement>(`a[href="${href}"]`);
  if (link === null) throw new Error(`no link to ${href}`);
  return link;
}

/** The text of each element `element` holds directly, in order. */
function childTexts(element: Element): (string | null)[] {
  return [...element.children].map((child) => child.textContent);
}

/** How long ago `iso` was, as `Timestamp` says it. */
function ageOf(iso: string): string | undefined {
  return relativeTimeLabel(new Date(iso), Date.now());
}

/** Which of the page's two marks stands over an element's text: its own, else its nearest
 *  marked ancestor's, as the page's reader takes a `[data-page-chrome]` element's words with each
 *  `[data-author-text]` descendant's taken out (`page-words.ts`). `both` is an element that
 *  carries the two at once. */
type Mark = "author-text" | "both" | "none" | "page-chrome";

function markOf(element: Element): Mark {
  const marked = element.closest(`[${PAGE_CHROME_ATTRIBUTE}], [${AUTHOR_TEXT_ATTRIBUTE}]`);
  if (marked === null) return "none";
  const chrome = marked.hasAttribute(PAGE_CHROME_ATTRIBUTE);
  const author = marked.hasAttribute(AUTHOR_TEXT_ATTRIBUTE);
  return chrome && author ? "both" : chrome ? "page-chrome" : "author-text";
}

/** For each word, the marks over every element whose own text it is, each once, beside the one
 *  mark it must carry: an empty list says no element of `root` draws the word. A `<select>`'s
 *  option is skipped, since it can hold no mark; a case reads an option by its role. */
function expectMarks(root: HTMLElement, words: readonly (readonly [string, Mark])[]): void {
  const marksOf = (text: string) => [
    ...new Set(within(root).queryAllByText(text, { ignore: "script, style, option" }).map(markOf)),
  ];
  expect(words.map(([text]) => [text, marksOf(text)])).toEqual(
    words.map(([text, mark]) => [text, [mark]])
  );
}

// ---------------------------------------------------------------------------------------------
// Every mount with long values, for the case that every value wraps
// ---------------------------------------------------------------------------------------------

/** Values as long as a reader's can be, each with no space to break at, so that only a class
 *  that breaks inside a word keeps one inside its row: a tester of the new page found a
 *  44-character repository name running past Details' row. A session's title begins with the
 *  word the mounts find its name by. */
const LONG = {
  component: "a-component-whose-identifier-runs-on-past-its-row",
  componentTitle: "A-component-whose-title-runs-on-past-the-row-of-Details",
  document: "a-specification-whose-file-name-runs-on.md",
  githubTitle: "Ship-the-new-issue-page-with-a-pull-request-title-that-runs-on",
  handedOnBy: "a-person-who-handed-the-question-on-with-a-long-login@example.com",
  inheritedRepository: "acme/its-parents-repository-with-a-long-name",
  label: "a-label-with-forty-characters-and-no-gap",
  liveTitle: "Implementer-of-the-new-issue-page-whose-title-runs-on-past-its-row",
  login: "a-person-whose-login-runs-on-past-the-row@example.com",
  question: "Should-we-ship-the-new-issue-page-before-its-header-is-done?",
  reason: "The-second-decision-cites-no-evidence-for-its-claim-at-all.",
  repository: "acme/the-repository-with-a-44-character-name",
  retired: "a-retired-component-whose-identifier-runs-on",
  route: "role:an-architect-role-whose-address-runs-on-past-its-row",
  stalledTitle: "Reviewer-of-the-new-issue-page-whose-title-runs-on-past-its-row",
  tmux: "the-tmux-session-of-a-long-running-agent:3",
  url: "https://example.com/a-path-of-notes-that-runs-on-past-the-row-of-details",
} as const;

/** `mount` with each read it makes answered in long values: each session's title, each
 *  priority's, the component's id and title, and each reviewer's login. A mount's own `node`
 *  still sets the answers it needs after these, as it does after `answerDefaults`. */
function withLongReads(mount: Mount): Mount {
  return {
    ...mount,
    node: () => {
      stubs.listAgents.mockResolvedValue([
        listed(LIVE_SESSION, LONG.liveTitle),
        listed(STALLED_SESSION, LONG.stalledTitle, "unresponsive"),
      ]);
      const rows = priorities.priorities;
      if (rows === null) throw new Error("the fixture lists no priorities to lengthen");
      stubs.listPriorities.mockResolvedValue({
        ...priorities,
        priorities: rows.map((row) => ({
          ...row,
          title: `${row.title}-under-a-title-that-runs-on-past-its-row`,
        })),
      });
      const components = architecture.components;
      if (components === null) throw new Error("the fixture has no components to lengthen");
      stubs.getArchitecture.mockResolvedValue({
        ...architecture,
        components: components.map((component) => ({
          ...component,
          id: LONG.component,
          title: LONG.componentTitle,
        })),
      });
      stubs.listArtifactReviews.mockResolvedValue(
        reviews.map((review) => ({
          ...review,
          actor: { id: `${review.actor.id}-whose-login-runs-on-past-the-row`, kind: "user" },
        }))
      );
      return mount.node();
    },
  };
}

/** `mount`, and its twin that draws long values in place of its props' (`long.node`) or names
 *  the values it draws that no mark finds (`long.values`). */
function withLongValues(
  mount: Mount,
  long: Pick<Mount, "values"> & Partial<Pick<Mount, "node">>
): readonly [Mount, Mount] {
  return [mount, { ...mount, ...long }];
}

/** An issue whose components line draws `components`, for the long-valued twins. */
function componentsIssue(
  key: string,
  components: Partial<Parameters<typeof IssueComponentsLine>[0]["issue"]["components"]>
): Parameters<typeof IssueComponentsLine>[0]["issue"] {
  return {
    components: {
      ids: [],
      inherited_from: null,
      mode: "explicit",
      reason: null,
      unknown: [],
      ...components,
    },
    key,
    project: "CORE",
    status: "todo",
    title: "Ship",
  };
}

/** The twin of each mount of `EVERY_MOUNT` whose values are its props, drawing long ones in
 *  their place, and of each that draws a value no `data-author-text` mark finds, naming it. */
const LONG_VALUES: ReadonlyMap<Mount, Mount> = new Map([
  // The refused copy prints the key it could not copy.
  withLongValues(mountNamed("CopyButton, copied and refused"), { values: ["CORE-1"] }),
  withLongValues(mountNamed("MultiSelect, open with a pick, details and a refusal"), {
    node: () => (
      <MultiSelect
        emptyMessage="No labels yet."
        error="Labels must be 1–40 characters."
        label="Labels"
        onChange={() => undefined}
        onOpenChange={() => undefined}
        open
        optionDetail={(value) => (value === LONG.label ? "3" : undefined)}
        options={[LONG.label, "docs"]}
        searchLabel="Search labels"
        selected={["docs"]}
      />
    ),
    values: [LONG.label],
  }),
  withLongValues(mountNamed("Pill, LabelPill and StatusPill"), {
    node: () => (
      <>
        <Pill>3</Pill>
        <LabelPill>{LONG.label}</LabelPill>
        <LabelPill selected>2 unread replies</LabelPill>
        <StatusPill>In progress</StatusPill>
      </>
    ),
    values: [LONG.label],
  }),
  withLongValues(mountNamed("ProofDocument, two open decisions and a block's references"), {
    node: () => {
      stubs.listIssueAsks.mockResolvedValue([
        { ...blockAsk("ask-b1", "b-1", LONG.question), last_reply: null },
        { ...blockAsk("ask-b2", "b-2", "Which region?"), last_reply: null },
      ]);
      return proofDocument();
    },
    values: [LONG.question],
  }),
  withLongValues(mountNamed("AskCard, an open blocking question with its session's handles open"), {
    node: () => {
      const ask = openAsk({
        asked_of_at: "2026-10-09T00:00:00Z",
        asked_of_by: LONG.handedOnBy,
        author: {
          id: LIVE_SESSION,
          kind: "session",
          origin: { session_title: LONG.liveTitle, tmux: LONG.tmux },
        },
        waiting_on_person: LONG.login,
      });
      return (
        <AskCard ask={ask} getAskThread={async () => threadOf(ask)} initialThread={threadOf(ask)} />
      );
    },
    values: [LONG.login, LONG.handedOnBy, LONG.tmux],
  }),
  withLongValues(mountNamed("ApprovalSteps, Details' size with versions and reasons"), {
    node: () => (
      <ApprovalSteps
        entries={stepEntries({
          decisions: `decisions-${LONG.document}`,
          reason: LONG.reason,
          spec: LONG.document,
        })}
        primaryArtifactId={SPEC_ID}
        versions={{ asks: [specRequest], issueKey: "CORE-1" }}
      />
    ),
    values: [LONG.document, LONG.reason],
  }),
  withLongValues(mountNamed("AssigneeControl, both variants, assigned and not"), {
    node: () => (
      <>
        <AssigneeControl assignee={LONG.login} issueKey="CORE-1" />
        <AssigneeControl assignee={null} issueKey="CORE-2" />
        <AssigneeControl assignee={LONG.login} issueKey="CORE-3" variant="header" />
        <AssigneeControl assignee={null} issueKey="CORE-4" variant="header" />
      </>
    ),
    values: [LONG.login],
  }),
  withLongValues(mountNamed("GitHubLink, a pull request, a plain link and an unsafe one"), {
    node: () => {
      const answered = stubs.githubRest.getMockImplementation();
      if (answered === undefined) throw new Error("no GitHub answer to draw a long title over");
      stubs.githubRest.mockImplementation(async (...read) =>
        read[0] === "repos/owner/repository/pulls/7"
          ? githubAnswer({
              head: { sha: "abcdef" },
              merged: false,
              state: "open",
              title: LONG.githubTitle,
            })
          : answered(...read)
      );
      return (
        <>
          <GitHubLink link={{ url: "https://github.com/owner/repository/pull/7" }} />
          <GitHubLink link={{ url: LONG.url }} />
          <GitHubLink link={{ url: `javascript:${LONG.url}` }} />
        </>
      );
    },
    values: [LONG.githubTitle, LONG.url],
  }),
  withLongValues(
    mountNamed("IssueComponentsLine, an explicit set, inherited, none and not attached"),
    {
      node: () => (
        <>
          <IssueComponentsLine
            issue={componentsIssue("CORE-1", {
              ids: [LONG.component],
              inherited_from: "CORE-9",
              unknown: [LONG.retired],
            })}
          />
          <IssueComponentsLine
            issue={componentsIssue("CORE-2", { mode: "none", reason: LONG.reason })}
          />
          <IssueComponentsLine inRow issue={componentsIssue("CORE-4", { ids: [LONG.component] })} />
        </>
      ),
      values: [LONG.component, LONG.componentTitle, LONG.retired, LONG.reason],
    }
  ),
  withLongValues(mountNamed("IssueLabels, its chips and its picker open"), {
    node: () => (
      <IssueLabels
        disabled={false}
        issueKey="CORE-1"
        labels={[LONG.label, "api"]}
        onSave={async () => undefined}
        project="CORE"
        saving={false}
      />
    ),
    values: [LONG.label],
  }),
  withLongValues(labelsEmptyAndSaving, {
    node: () => (
      <>
        <IssueLabels
          disabled={false}
          issueKey="CORE-1"
          labels={[]}
          onSave={async () => undefined}
          project="CORE"
          saving={false}
        />
        <IssueLabels
          disabled={false}
          labels={[LONG.label]}
          onSave={async () => undefined}
          project="CORE"
          saving
        />
        <IssueLabels
          disabled={false}
          labels={[LONG.label]}
          onSave={async () => undefined}
          project="CORE"
          saving={false}
          variant="rail"
        />
      </>
    ),
    values: [LONG.label],
  }),
  withLongValues(
    mountNamed(
      "RepoControl, its own repository and an inherited one, out of a row of Details and in one, and none in one"
    ),
    {
      node: () => (
        <>
          <RepoControl
            issueKey="CORE-1"
            project="CORE"
            repo={LONG.repository}
            resolvedRepo={LONG.repository}
          />
          <RepoControl
            issueKey="CORE-2"
            project="CORE"
            repo={null}
            resolvedRepo={LONG.inheritedRepository}
          />
          <RepoControl inRow issueKey="CORE-3" project="CORE" repo={null} resolvedRepo={null} />
          <RepoControl
            inRow
            issueKey="CORE-4"
            project="CORE"
            repo={LONG.repository}
            resolvedRepo={LONG.repository}
          />
          <RepoControl
            inRow
            issueKey="CORE-5"
            project="CORE"
            repo={null}
            resolvedRepo={LONG.inheritedRepository}
          />
        </>
      ),
      values: [LONG.repository, LONG.inheritedRepository],
    }
  ),
  withLongValues(mountNamed("RouteReach, each marker"), {
    node: () => routeMarkers(LONG.route, LONG.route),
    values: [LONG.route],
  }),
]);

/** What lets a value break inside a word: `[overflow-wrap:anywhere]`, the one spelling the shared
 *  controls use for it inside `NoTooltips`. Unlike `break-words` (`overflow-wrap: break-word`),
 *  it also lets the value's min-content width shrink below its longest word. */
const BREAKS_ANYWHERE = "[overflow-wrap:anywhere]";

/** What keeps a value within its line's width: `max-w-full`, or `min-w-0`, which a flex item
 *  needs to shrink below its text. */
const STAYS_IN_ITS_LINE = ["max-w-full", "min-w-0"];

/**
 * The elements inside a value that are no part of its text, skipped by name. A count (an
 * option's detail, the open decisions' count) is never read as a value at all: it carries no
 * author-text mark and no mount names it among its values.
 */
const NOT_VALUES: readonly (readonly [what: string, selector: string])[] = [
  // An icon draws no text.
  ["an icon", "svg, svg *"],
  // `AgentName`'s state mark is the dot before a session's name, named by its reading.
  ["AgentName's state mark", "[data-agent-name] > [role='img']"],
  // `AssigneeControl`'s initial is drawn from `data-initial` and adds no letter to the name.
  ["AssigneeControl's initial", "[data-initial]"],
];

/** The text an element holds itself, not inside a child element. */
function ownText(element: Element): string {
  return [...element.childNodes]
    .filter((node) => node.nodeType === node.TEXT_NODE)
    .map((node) => node.textContent)
    .join("");
}

/** Whether `element` is a text box whose value or placeholder holds `value`: a box draws its
 *  words from those attributes, not from any text of its own. */
function boxHolds(element: Element, value: string): boolean {
  return (
    element instanceof HTMLInputElement &&
    (element.value.includes(value) || element.placeholder.includes(value))
  );
}

/**
 * Each value `mount` draws among `drawn`: every element with the author-text mark, and each
 * element holding one of `mount.values` in its own text, or a text box holding it as its value or
 * its placeholder. A value inside `sr-only` is read aloud and drawn in no row, and a native
 * `<option>` is drawn by the browser's own list, which no class wraps, so neither is read.
 * `missing` takes each value of `mount.values` that nothing draws.
 */
function drawnValues(mount: Mount, drawn: readonly Element[], missing: string[]): Element[] {
  const shown = drawn.filter((element) => element.closest(".sr-only, select") === null);
  const values = new Set(shown.filter((element) => element.hasAttribute(AUTHOR_TEXT_ATTRIBUTE)));
  for (const value of mount.values ?? []) {
    const holders = shown.filter(
      (element) => ownText(element).includes(value) || boxHolds(element, value)
    );
    if (holders.length === 0) missing.push(`${mount.name}: draws no ${value}`);
    for (const holder of holders) values.add(holder);
  }
  return [...values];
}

/** An element as the wrap case names it: its tag and class, and the start of its text. */
function described(element: Element): string {
  return `<${element.tagName.toLowerCase()} class="${element.getAttribute("class") ?? ""}">${(element.textContent ?? "").slice(0, 32)}`;
}

/** Why `value` cannot wrap inside its row: it does not break anywhere (`BREAKS_ANYWHERE`), it
 *  has no bound on its width (`STAYS_IN_ITS_LINE`), or it, an element of its text inside it, or
 *  an element around it up to `root` holds its line or its width (`HOLDS_ITS_LINE`). A text box
 *  is exempt from the first alone: it draws its value on one line, which it scrolls inside its
 *  own width and no class breaks, so it narrows to its line instead, which the rest is read for. */
function wrapOffences(mountName: string, value: Element, root: Element): string[] {
  const named = `${mountName}: ${described(value)}`;
  const offences: string[] = [];
  const own = classOf(value);
  if (!(value instanceof HTMLInputElement) && !own.includes(BREAKS_ANYWHERE)) {
    offences.push(`${named} does not break anywhere (${BREAKS_ANYWHERE})`);
  }
  if (!own.some((token) => STAYS_IN_ITS_LINE.includes(token))) {
    offences.push(`${named} carries neither max-w-full nor min-w-0`);
  }
  const inside = [...value.querySelectorAll("*")].filter(
    (element) => !NOT_VALUES.some(([, selector]) => element.matches(selector))
  );
  const around: Element[] = [];
  let outer = value.parentElement;
  while (outer !== null && outer !== root) {
    around.push(outer);
    outer = outer.parentElement;
  }
  const read: readonly (readonly [string, readonly Element[]])[] = [
    ["it", [value]],
    ["inside it", inside],
    ["around it", around],
  ];
  for (const [where, elements] of read) {
    for (const element of elements) {
      const held = holdsItsLine(element);
      if (held.length === 0) continue;
      offences.push(
        `${named}: ${where === "it" ? "it" : `${described(element)} ${where}`} carries ${held.join(" ")}`
      );
    }
  }
  return offences;
}

// ---------------------------------------------------------------------------------------------
// The page colour pairs of the `ON_PAGE` tables
// ---------------------------------------------------------------------------------------------

/**
 * The files whose `ON_PAGE` table holds the page's colours for their elements inside
 * `NoTooltips`. `theme/text-on-background.test.ts` cannot pair those: it reads a class only where
 * it names an export of `classes.ts`, and `ON_PAGE.detail` names none. The case reads each file's
 * source, as that test does, rather than a list of pairs the file exports: which element draws an
 * entry, and on which ground, is a fact of the file's markup, and a list beside it could say other
 * than the markup draws.
 */
const ON_PAGE_FILES = [
  "components/MultiSelect.tsx",
  "features/doc/ApprovalChip.tsx",
  "features/inbox/AskCard.tsx",
  "features/inbox/AskThread.tsx",
  "features/issue-page/IssueFieldForms.tsx",
  "features/issue/RepoControl.tsx",
] as const;

/** The page's two grounds, the paper and the pane, on which an element of these files is drawn
 *  where no element of its own file draws a ground under it: a control is laid on one of them
 *  (`classes.ts` registers each of the page's inks on both). */
const PAGE_GROUNDS = ["page-paper", "page-pane"] as const;

/** A source file, parsed, with each `const` of it by its name: a name declared in two of its
 *  functions keeps both. */
interface ParsedSource {
  readonly declarations: ReadonlyMap<string, readonly ts.Expression[]>;
  readonly path: string;
  readonly source: ts.SourceFile;
}

function parsedSource(path: string): ParsedSource {
  const source = ts.createSourceFile(
    path,
    readFileSync(join(import.meta.dir, "..", "..", path), "utf8"),
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX
  );
  const declarations = new Map<string, ts.Expression[]>();
  const visit = (node: ts.Node) => {
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer) {
      declarations.set(node.name.text, [
        ...(declarations.get(node.name.text) ?? []),
        node.initializer,
      ]);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return { declarations, path, source };
}

/** `node` with what changes only its type taken off: brackets, `as`, `satisfies` and `!`. */
function bare(node: ts.Expression): ts.Expression {
  return ts.isParenthesizedExpression(node) ||
    ts.isAsExpression(node) ||
    ts.isSatisfiesExpression(node) ||
    ts.isNonNullExpression(node)
    ? bare(node.expression)
    : node;
}

/** What a function of the file returns: an arrow's body, or each `return` of its block. */
function returnedBy(node: ts.Expression): ts.Expression[] {
  const fn = bare(node);
  if (!ts.isArrowFunction(fn) && !ts.isFunctionExpression(fn)) return [];
  if (!ts.isBlock(fn.body)) return [fn.body];
  const returned: ts.Expression[] = [];
  const visit = (child: ts.Node) => {
    if (ts.isReturnStatement(child) && child.expression !== undefined)
      returned.push(child.expression);
    else if (!ts.isFunctionLike(child)) ts.forEachChild(child, visit);
  };
  ts.forEachChild(fn.body, visit);
  return returned;
}

/** Each entry of the table `name` names: a table of the file (`ON_PAGE`, `PAGE_URGENCY_ACCENT`),
 *  else an object `classes.ts` exports (`pageStateText`), each entry as its class strings. */
function tableEntries(
  name: string,
  file: ParsedSource,
  depth: number
): ReadonlyMap<string, readonly string[]> {
  const entries = new Map<string, readonly string[]>();
  for (const declared of file.declarations.get(name) ?? []) {
    const table = bare(declared);
    if (!ts.isObjectLiteralExpression(table)) continue;
    for (const property of table.properties) {
      if (ts.isPropertyAssignment(property) && !ts.isComputedPropertyName(property.name)) {
        entries.set(property.name.text, classStrings(property.initializer, file, depth + 1));
      }
    }
  }
  const exported: unknown = (theme as Readonly<Record<string, unknown>>)[name];
  if (entries.size === 0 && typeof exported === "object" && exported !== null) {
    for (const [key, value] of Object.entries(exported)) {
      if (typeof value === "string") entries.set(key, [value]);
    }
  }
  return entries;
}

/**
 * Every class string `node` can make at run time, read from its file's source: a ternary, `??`
 * and `||` one for each side, a template one for each combination of its parts, a name what the
 * file's `const` of that name makes or the string `classes.ts` exports under it, `table.key` that
 * entry, `table[key]` each entry, and a call of the file's own function what it returns. Anything
 * else - a prop, a hook's answer, another module's function - makes the empty string, which names
 * no colour: only an element's page colours are read, and those are written in these forms.
 */
function classStrings(node: ts.Expression, file: ParsedSource, depth = 0): string[] {
  if (depth > 24) return [""];
  const made = (expression: ts.Expression) => classStrings(expression, file, depth + 1);
  const expression = bare(node);
  if (ts.isStringLiteral(expression) || ts.isNoSubstitutionTemplateLiteral(expression)) {
    return [expression.text];
  }
  if (ts.isConditionalExpression(expression)) {
    return [...new Set([...made(expression.whenTrue), ...made(expression.whenFalse)])];
  }
  if (
    ts.isBinaryExpression(expression) &&
    (expression.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken ||
      expression.operatorToken.kind === ts.SyntaxKind.BarBarToken)
  ) {
    return [...new Set([...made(expression.left), ...made(expression.right)])];
  }
  if (ts.isTemplateExpression(expression)) {
    let strings = [expression.head.text];
    for (const span of expression.templateSpans) {
      const parts = made(span.expression);
      strings = [
        ...new Set(
          strings.flatMap((before) => parts.map((part) => `${before}${part}${span.literal.text}`))
        ),
      ];
    }
    return strings;
  }
  if (ts.isIdentifier(expression)) {
    const declared = file.declarations.get(expression.text);
    if (declared !== undefined) return [...new Set(declared.flatMap(made))];
    const exported: unknown = (theme as Readonly<Record<string, unknown>>)[expression.text];
    return typeof exported === "string" ? [exported] : [""];
  }
  if (ts.isPropertyAccessExpression(expression) && ts.isIdentifier(expression.expression)) {
    return [
      ...(tableEntries(expression.expression.text, file, depth).get(expression.name.text) ?? [""]),
    ];
  }
  if (ts.isElementAccessExpression(expression) && ts.isIdentifier(expression.expression)) {
    const entries = [...tableEntries(expression.expression.text, file, depth).values()];
    return entries.length === 0 ? [""] : [...new Set(entries.flat())];
  }
  if (ts.isCallExpression(expression) && ts.isIdentifier(expression.expression)) {
    const returned = (file.declarations.get(expression.expression.text) ?? []).flatMap(returnedBy);
    return returned.length === 0 ? [""] : [...new Set(returned.flatMap(made))];
  }
  return [""];
}

/** The `ON_PAGE` entries an expression reads, through the file's own `const`s. */
function onPageEntries(node: ts.Node, file: ParsedSource, seen = new Set<ts.Node>()): string[] {
  if (seen.has(node)) return [];
  seen.add(node);
  if (
    ts.isPropertyAccessExpression(node) &&
    ts.isIdentifier(node.expression) &&
    node.expression.text === "ON_PAGE"
  ) {
    return [node.name.text];
  }
  const read: string[] = [];
  if (ts.isIdentifier(node)) {
    for (const declared of file.declarations.get(node.text) ?? []) {
      read.push(...onPageEntries(declared, file, seen));
    }
  }
  ts.forEachChild(node, (child) => {
    read.push(...onPageEntries(child, file, seen));
  });
  return [...new Set(read)];
}

/** The page colours one class string draws, each by its state: `""` at rest, else its variants
 *  (`hover`, `refused`, `placeholder`). */
interface PageLook {
  readonly bg: ReadonlyMap<string, string>;
  readonly text: ReadonlyMap<string, string>;
}

/** The page colours of `className`, or `undefined` for `main`'s look, a string with a colour of
 *  `main`'s, which is never drawn inside `NoTooltips` (the colour case holds that). The focus
 *  rule `main` keeps inside `NoTooltips` is no ground and no text. */
function pageLook(className: string): PageLook | undefined {
  const bg = new Map<string, string>();
  const text = new Map<string, string>();
  for (const token of tokens(className)) {
    const { utility, variants } = splitToken(token);
    if (variants.some((variant) => FOCUS_VARIANTS.includes(variant))) continue;
    if (RAW_COLOR_PATTERN.test(utility)) return undefined;
    const colour = /^(bg|text)-(.+)$/.exec(utility);
    if (colour?.[2] === undefined || !Object.hasOwn(PAGE_COLORS, colour[2])) continue;
    (colour[1] === "bg" ? bg : text).set(variants.join(":"), colour[2]);
  }
  return { bg, text };
}

/** One page colour pair an element draws. */
interface DrawnPair {
  readonly entries: readonly string[];
  readonly ground: string;
  readonly line: number;
  readonly state: string;
  readonly text: string;
}

/** Each page colour pair the elements of `file` that read an `ON_PAGE` entry draw, in each state
 *  of theirs and of the elements around them: the text an element's class gives it, else the
 *  nearest around it; on the ground its class gives it, else each ground of the nearest element
 *  around it that draws one in every look it can take, with the grounds of the looks on the way
 *  that draw one; else the page's grounds. */
function onPagePairs(file: ParsedSource): DrawnPair[] {
  const pairs: DrawnPair[] = [];
  const visit = (node: ts.Node, around: readonly (readonly PageLook[])[]) => {
    const opening = ts.isJsxElement(node)
      ? node.openingElement
      : ts.isJsxSelfClosingElement(node)
        ? node
        : undefined;
    const classAttribute = opening?.attributes.properties.find(
      (attribute): attribute is ts.JsxAttribute =>
        ts.isJsxAttribute(attribute) && attribute.name.getText() === "className"
    );
    const value = classAttribute?.initializer;
    const expression =
      value === undefined
        ? undefined
        : ts.isJsxExpression(value)
          ? value.expression
          : ts.isStringLiteral(value)
            ? value
            : undefined;
    if (opening === undefined || expression === undefined) {
      ts.forEachChild(node, (child) => visit(child, around));
      return;
    }
    const looks = classStrings(expression, file).flatMap((className) => {
      const look = pageLook(className);
      return look === undefined ? [] : [look];
    });
    const entries = onPageEntries(expression, file);
    if (entries.length > 0) {
      const line = file.source.getLineAndCharacterOfPosition(opening.getStart()).line + 1;
      const states = new Set(
        [...looks, ...around.flat()].flatMap((look) => ["", ...look.bg.keys(), ...look.text.keys()])
      );
      for (const look of looks) {
        for (const state of states) {
          const own = look.text.get(state) ?? look.text.get("");
          const texts = own === undefined ? textAround(around, state) : [own];
          for (const text of texts) {
            for (const ground of groundsOf(look, around, state)) {
              pairs.push({ entries, ground, line, state, text });
            }
          }
        }
      }
    }
    ts.forEachChild(node, (child) => visit(child, [looks, ...around]));
  };
  visit(file.source, []);
  return pairs;
}

/** The text colours the nearest element around draws in `state`, which an element of no text
 *  colour of its own takes. */
function textAround(around: readonly (readonly PageLook[])[], state: string): string[] {
  for (const looks of around) {
    const texts = looks.flatMap((look) => {
      const text = look.text.get(state) ?? look.text.get("");
      return text === undefined ? [] : [text];
    });
    if (texts.length > 0) return [...new Set(texts)];
  }
  return [];
}

/** The grounds an element of `look` is drawn on in `state`: its own, else those of the elements
 *  around it out to the first that draws a ground in every look it can take, else the page's. */
function groundsOf(
  look: PageLook,
  around: readonly (readonly PageLook[])[],
  state: string
): string[] {
  const own = look.bg.get(state) ?? look.bg.get("");
  if (own !== undefined) return [own];
  const grounds = new Set<string>();
  for (const looks of around) {
    let through = looks.length === 0;
    for (const outer of looks) {
      const ground = outer.bg.get(state) ?? outer.bg.get("");
      if (ground === undefined) through = true;
      else grounds.add(ground);
    }
    if (!through) return [...grounds];
  }
  return [...grounds, ...PAGE_GROUNDS];
}

/** Whether `CONTRAST_CHECKS` holds `text` on `ground` as words, at 4.5:1: a check of the same
 *  swatches at 3:1 is a shape's (a row's twisty), which no word may lean on. */
function registeredAsText(text: string, ground: string): boolean {
  const colours: Readonly<Partial<Record<string, theme.ColorPair>>> = PAGE_COLORS;
  const foreground = colours[text];
  const background = colours[ground];
  return theme.CONTRAST_CHECKS.some(
    (check) =>
      check.minRatio === 4.5 &&
      check.foreground.light.name === foreground?.light.name &&
      check.foreground.dark.name === foreground?.dark.name &&
      check.background.light.name === background?.light.name &&
      check.background.dark.name === background?.dark.name
  );
}

// ---------------------------------------------------------------------------------------------
// The cases
// ---------------------------------------------------------------------------------------------

// Outside `NoTooltips` each shared control carries every `title` `main` gives it, in each state
// that sets one, and no other.
test("outside NoTooltips each shared control sets the title that main sets", async () => {
  for (const mount of EVERY_MOUNT) {
    if (MOVING_INSTANTS.includes(mount)) continue;
    const titles = MAIN_TITLES.get(mount);
    if (titles === undefined) throw new Error(`no titles of main recorded for ${mount.name}`);
    await readDrawn(mount, "outside", (_view, elements) => {
      expect({ mount: mount.name, titles: titlesOf(elements) }).toEqual({
        mount: mount.name,
        titles: [...titles],
      });
    });
  }
});

// Inside `NoTooltips` no shared control sets a `title` it sets on `main`,
// the two the source kept (the stale chip and `AgentUnresponsiveNote`), an open
// `MultiSelect`'s options and a repository row's "None" and its repository's text in Details among
// them. `AskCard`'s own elements are read, the asker's name and the submit among them, and its
// children of other files are not (`askCardChildren`).
test("inside NoTooltips no shared control sets a title", async () => {
  const scope = (view: RenderResult) => within(view.baseElement);
  const named = new Map<Mount, (view: RenderResult) => readonly Element[]>([
    [
      mountNamed("ApprovalChip, each state, and the Request changes dialog"),
      // The stale chip is the second of the three the mount draws, read by its place so that
      // this case reads its `title` whatever its words say.
      (view) => [...view.container.querySelectorAll("button[aria-haspopup='dialog']")].slice(1, 2),
    ],
    [agentsAtHeartbeat, (view) => [scope(view).getByTestId("agent-unresponsive")]],
    [
      mountNamed("MultiSelect, open with a pick, details and a refusal"),
      (view) => scope(view).getAllByRole("option"),
    ],
    [
      mountNamed("AskCard, an open blocking question with its session's handles open"),
      (view) => [
        scope(view).getByRole("button", { expanded: true, name: /Implementer/ }),
        scope(view).getByRole("button", { name: "Answer" }),
      ],
    ],
    [
      personAsksForApproval,
      (view) => [
        scope(view).getByText("alice"),
        scope(view).getByRole("button", { name: "Answer" }),
      ],
    ],
    [
      mountNamed(
        "RepoControl, its own repository and an inherited one, out of a row of Details and in one, and none in one"
      ),
      (view) => [
        scope(view).getByRole("button", { name: `${CONTROL_LABELS.none}, Repository of CORE-3` }),
        scope(view).getByRole("button", { name: "acme/app, Repository of CORE-4" }),
        scope(view).getByRole("button", { name: "acme/site, Repository of CORE-5" }),
      ],
    ],
  ]);
  const titled: string[] = [];
  const unread: string[] = [];
  for (const mount of EVERY_MOUNT) {
    await readDrawn(mount, "inside", (view, elements) => {
      for (const element of elements) {
        const title = element.getAttribute("title");
        if (title !== null) {
          titled.push(`${mount.name}: <${element.tagName.toLowerCase()}> title="${title}"`);
        }
      }
      // Each element `named` picks out is drawn, and is one the walk above read.
      const read = named.get(mount)?.(view);
      if (read?.length === 0) unread.push(`${mount.name}: nothing drawn`);
      for (const element of read ?? []) {
        if (!elements.includes(element)) unread.push(`${mount.name}: ${element.outerHTML}`);
      }
    });
  }
  expect(titled).toEqual([]);
  expect(unread).toEqual([]);
});

// A `title` hides a missing name from axe, so with none left each
// control must be named by its own words or its label.
test("inside NoTooltips every shared control keeps an accessible name", async () => {
  const roles = ["button", "link", "img", "status"] as const;
  const unnamed: string[] = [];
  const read = new Map<string, number>(roles.map((role) => [role, 0]));
  for (const mount of EVERY_MOUNT) {
    await readDrawn(mount, "inside", (view, elements) => {
      const drawn = new Set(elements);
      const scope = within(view.baseElement);
      for (const role of roles) {
        const named = new Set(scope.queryAllByRole(role, { name: (name) => name.trim() !== "" }));
        for (const element of scope.queryAllByRole(role)) {
          if (!drawn.has(element)) continue;
          read.set(role, (read.get(role) ?? 0) + 1);
          if (!named.has(element)) unnamed.push(`${mount.name}: ${role} ${element.outerHTML}`);
        }
      }
    });
  }
  expect(unnamed).toEqual([]);
  // A walk that met no element of a role would pass with nothing read.
  expect([...read].filter(([, count]) => count === 0)).toEqual([]);
});

// Inside `NoTooltips` each fact that only a `title` held on `main` is drawn, as text or as the
// control's name, in the words of the new issue page's word list where it holds them.
test("inside NoTooltips a fact that only a title held is drawn", async () => {
  const claimed = `${ageOf(CLAIMED_AT)} (${shortTime(CLAIMED_AT)})`;
  const heard = `${ageOf(HEARD_AT)} (${shortTime(HEARD_AT)})`;

  // `Timestamp` with `exact` says the date and the time in brackets after the age; without it,
  // the age alone.
  const timestamps: Mount = {
    name: "Timestamp, exact and not",
    node: () => (
      <>
        <Timestamp at={CLAIMED_AT} exact />
        <Timestamp at={CLAIMED_AT} />
      </>
    ),
  };
  await readDrawn(timestamps, "inside", (view) => {
    const age = ageOf(CLAIMED_AT);
    if (age === undefined) throw new Error(`Timestamp says no age for ${CLAIMED_AT}`);
    expect([...view.container.querySelectorAll("time")].map((time) => time.textContent)).toEqual([
      claimed,
      age,
    ]);
  });

  // `AgentName`'s state mark is named by its whole reading, not by its state alone.
  await readDrawn(agentsAtHeartbeat, "inside", (view) => {
    const scope = within(view.container);
    expect(scope.getAllByRole("img").map((mark) => mark.getAttribute("aria-label"))).toEqual([
      RUNNING_READING,
      UNRESPONSIVE_READING,
      "Not running",
      "Not running",
      RUNNING_READING,
    ]);
    expect(scope.getAllByRole("img", { name: RUNNING_READING })).toHaveLength(2);
  });

  // `ConnectionDot` is named by its state.
  await readDrawn(
    mountNamed("ConnectionDot, each state and a pending notice"),
    "inside",
    (view) => {
      expect(
        within(view.container)
          .getAllByRole("status")
          .map((dot) => dot.getAttribute("aria-label"))
      ).toEqual([
        "connected",
        "Connecting to the document…",
        "The document could not load",
        "offline · 2 edits saved in this browser, not sent yet.",
      ]);
    }
  );

  // `ClaimChip` draws the claim's instant and the holder's heartbeat after their ages, and the
  // moment the claim comes free, or that it is free now.
  await readDrawn(claimsAtFixedInstants, "inside", (view) => {
    const scope = within(view.container);
    const [held, lapsed, unresponsive, free] = scope.getAllByTestId("issue-claim");
    const times = (chip: HTMLElement | undefined) =>
      [...(chip?.querySelectorAll("time") ?? [])].map((time) => time.textContent);
    expect(times(held)).toEqual([claimed]);
    expect(times(lapsed)).toEqual([claimed]);
    expect(times(unresponsive)).toEqual([claimed, heard, shortTime(FREE_FROM)]);
    expect(times(free)).toEqual([claimed, heard]);
    expect(unresponsive?.querySelector("[data-testid='issue-claim-takeable']")?.textContent).toBe(
      `, ${CONTROL_LABELS.takeable} ${shortTime(FREE_FROM)}`
    );
    expect(free?.querySelector("[data-testid='issue-claim-takeable']")?.textContent).toBe(
      `, ${CONTROL_LABELS.takeable_now}`
    );
    // A registry that answered leaves no claim unjudged.
    expect(scope.queryAllByTestId("issue-claim-unchecked")).toHaveLength(0);
  });
  // ... and "· running state unknown" when the agent list cannot be read.
  await readDrawn(
    mountNamed("AgentName and ClaimChip, with no registry read"),
    "inside",
    (view) => {
      const unchecked = within(view.container).getByTestId("issue-claim-unchecked");
      expect([unchecked.previousElementSibling?.textContent, unchecked.textContent]).toEqual([
        "·",
        CONTROL_LABELS.unchecked,
      ]);
    }
  );

  // `GitHubLink` draws `<owner>/<repo>#<n>` and the title once read, the whole address while the
  // read is out or for a link GitHub does not hold, that an unsafe address is unsafe, and that
  // Dispatch holds no token for the repository. The title wraps whole where `main` truncates it.
  await readDrawn(
    mountNamed("GitHubLink, a pull request, a plain link and an unsafe one"),
    "inside",
    (view) => {
      const root = view.container;
      const pullRequest = linkTo(root, "https://github.com/owner/repository/pull/7");
      expect(childTexts(pullRequest)).toEqual([
        "owner/repository#7",
        "Ship it",
        "open",
        `${CONTROL_LABELS.github_checks}: success`,
      ]);
      const title = classOf(within(pullRequest).getByText("Ship it"));
      expect(title).toContain("[overflow-wrap:anywhere]");
      expect(title).not.toContain("truncate");
      expect(title).not.toContain("max-w-[18ch]");
      const plain = linkTo(root, "https://example.com/notes");
      expect(plain.textContent).toBe("https://example.com/notes");
      const unsafeAddress = within(root).getByText("javascript:alert(1)");
      const unsafe = unsafeAddress.parentElement;
      expect(unsafe === null ? [] : childTexts(unsafe)).toEqual([
        "javascript:alert(1)",
        `(${CONTROL_LABELS.external_link_unsafe})`,
      ]);
      // The plain link and the unsafe address wrap whole too, where `main` cuts each at 18
      // characters and leaves the rest to a `title`: the address breaks anywhere, and no element
      // of its box truncates or narrows it.
      const wrapsWhole = (address: Element, box: Element) => {
        expect(classOf(address)).toContain("[overflow-wrap:anywhere]");
        const cut = [box, ...box.querySelectorAll("*")].filter((element) =>
          classOf(element).some((token) => token === "truncate" || token === "max-w-[18ch]")
        );
        expect(cut.map((element) => element.outerHTML)).toEqual([]);
      };
      wrapsWhole(plain, plain);
      wrapsWhole(unsafeAddress, unsafe ?? unsafeAddress);
    }
  );
  await readDrawn(githubUnread, "inside", (view) => {
    const root = view.container;
    expect(linkTo(root, "https://github.com/owner/repository/issues/8").textContent).toBe(
      "https://github.com/owner/repository/issues/8"
    );
    expect(childTexts(linkTo(root, "https://github.com/acme/private/pull/9"))).toEqual([
      "acme/private#9",
      STATE_LINES.github_unreadable,
    ]);
  });

  // `RepoControl` says beside an inherited repository that it is inherited or the default: beside
  // the box, and in a row of Details beside the repository's text.
  await readDrawn(
    mountNamed(
      "RepoControl, its own repository and an inherited one, out of a row of Details and in one, and none in one"
    ),
    "inside",
    (view) => {
      const scope = within(view.container);
      const drawn = scope.getAllByText(CONTROL_LABELS.repository_inherited);
      expect(drawn).toHaveLength(2);
      expect(
        drawn[0]?.parentElement?.contains(
          scope.getByRole("combobox", { name: "Repository of CORE-2" })
        )
      ).toBe(true);
      expect(
        drawn[1]?.parentElement?.contains(
          scope.getByRole("button", { name: "acme/site, Repository of CORE-5" })
        )
      ).toBe(true);
    }
  );

  // `SubscribedAgents` says a wider subscription covers more than this issue, in place of "via"
  // and its topic.
  await readDrawn(subscribersAtHeartbeat, "inside", (view) => {
    const scope = within(view.container);
    const [removable, wider] = scope.getAllByRole("listitem");
    expect(removable?.textContent).not.toContain(STATE_LINES.subscription_wider);
    expect(wider?.textContent).toContain(STATE_LINES.subscription_wider);
    expect(scope.queryAllByText(/\bvia\b|notifications\.dispatch/)).toHaveLength(0);
  });
});

// Inside `NoTooltips` the repository's clear control carries no `title`: its name stays `main`'s,
// and its description, in the page's mark, says what the `title` said, that the issue then
// inherits a repository. A value that is not owner/name, written with Enter, draws `main`'s
// refusal in the page's failed red, with the page's mark. Outside, the control keeps `main`'s
// `title` and no description, and the refusal `main`'s red and no mark.
test("inside NoTooltips the repository's clear control is described by what its title said, and a refusal is the page's failed red", async () => {
  const user = userEvent.setup();
  const clearExplains = "Clear it, and the issue inherits a repository again";
  for (const surface of ["inside", "outside"] as const) {
    const view = draw(
      <>
        <RepoControl issueKey="CORE-1" project="CORE" repo="acme/api" resolvedRepo="acme/api" />
        <RepoControl issueKey="CORE-2" project="CORE" repo={null} resolvedRepo="acme/web" />
      </>,
      surface
    );
    try {
      const scope = within(view.container);
      const clear = scope.getByRole("button", { name: "Clear the repository of CORE-1" });
      const description = document.getElementById(clear.getAttribute("aria-describedby") ?? "");
      if (surface === "inside") {
        expect(clear.getAttribute("title")).toBeNull();
        expect(
          scope.getByRole("button", {
            description: clearExplains,
            name: "Clear the repository of CORE-1",
          })
        ).toBe(clear);
        expect(description?.hasAttribute(PAGE_CHROME_ATTRIBUTE)).toBe(true);
      } else {
        expect(clear.getAttribute("title")).toBe(clearExplains);
        expect(description).toBeNull();
      }

      // The focus starts the mappings' read, which lands before the refusal is read.
      await user.type(scope.getByRole("combobox", { name: "Repository of CORE-2" }), "nope{Enter}");
      await settleReads();
      const refusal = scope.getByRole("alert");
      expect(refusal.textContent).toBe("Write the repository as owner/name.");
      if (surface === "inside") {
        expect(classOf(refusal)).toEqual(expect.arrayContaining(tokens(theme.pageFailedText)));
        expect(
          classOf(refusal).filter((token) => tokens(theme.dangerText).includes(token))
        ).toEqual([]);
        expect(refusal.hasAttribute(PAGE_CHROME_ATTRIBUTE)).toBe(true);
      } else {
        expect(refusal.getAttribute("class")).toBe(theme.dangerText);
        expect(refusal.hasAttribute(PAGE_CHROME_ATTRIBUTE)).toBe(false);
      }
    } finally {
      undraw(view);
    }
  }
});

// Inside `NoTooltips` each word a shared control reads from the new issue page's word list is
// that list's word, found here by the list's own entry, and each element carries the mark the
// source gives it: the page's words `data-page-chrome`, a person's or an agent's
// `data-author-text` (`ApprovalSteps`' "Not requested" pill, `StepReason`'s line and its reason,
// a person's name, and the page's "An agent" outside every name somebody was given among them),
// and each approval chip `data-approval-artifact` with its document's id. No control draws
// `CONTROL_LABELS.unknown_author`: `AgentText.author` alone says it.
test("inside NoTooltips the words come from the page's word list and carry their marks", async () => {
  await readDrawn(
    mountNamed("ApprovalChip, each state, and the Request changes dialog"),
    "inside",
    (view) => {
      const chips = [...view.container.querySelectorAll("button[aria-haspopup='dialog']")];
      expect(chips.map((chip) => chip.getAttribute("data-approval-artifact"))).toEqual([
        notes.id,
        notes.id,
        notes.id,
      ]);
      expect(chips[1]?.textContent).toBe(`Approved v3 · ${CONTROL_LABELS.approval_latest} v5`);
    }
  );

  await readDrawn(
    mountNamed("ApprovalSteps, Details' size with versions and reasons"),
    "inside",
    (view) => {
      const root = view.container;
      expect(
        [...root.querySelectorAll("[data-approval-artifact]")].map((chip) =>
          chip.getAttribute("data-approval-artifact")
        )
      ).toEqual([SPEC_ID, DECISIONS_ID]);
      expectMarks(root, [
        [CONTROL_LABELS.not_requested, "page-chrome"],
        // The line's own words, the step and the review, around the reviewer and the reason.
        ["Decisions: requested changes on v3 ·", "page-chrome"],
        ["bob", "author-text"],
        ["The second decision cites no evidence.", "author-text"],
      ]);
      expect(
        root
          .querySelector("[data-approval-step-reason='decisions']")
          ?.hasAttribute(PAGE_CHROME_ATTRIBUTE)
      ).toBe(true);
      expectMarks(within(root).getByRole("link", { name: "spec.md, version 1" }), [
        ["spec.md,", "author-text"],
        [CONTROL_LABELS.version.toLowerCase(), "page-chrome"],
        ["1", "author-text"],
      ]);
    }
  );
  await readDrawn(mountNamed("ApprovalSteps, the header's compact size"), "inside", (view) => {
    expectMarks(view.container, [[CONTROL_LABELS.not_requested, "page-chrome"]]);
  });

  await readDrawn(agentsAsAuthors, "inside", (view) => {
    const root = view.container;
    expectMarks(root, [
      ["alice", "author-text"],
      [`, acting through ${AUTHOR_KIND_WORDS.agent}`, "page-chrome"],
      [CONTROL_LABELS.unnamed_author, "page-chrome"],
    ]);
    expect(
      within(root).getByText(CONTROL_LABELS.unnamed_author).closest(`[${AUTHOR_TEXT_ATTRIBUTE}]`)
    ).toBeNull();
  });
  await readDrawn(agentsAtHeartbeat, "inside", (view) => {
    expectMarks(view.container, [
      ["Implementer", "author-text"],
      ["Reviewer", "author-text"],
      ["Old planner", "author-text"],
      // A session nothing names, not an author here, reads as one.
      [CONTROL_LABELS.unnamed_author, "page-chrome"],
    ]);
  });

  await readDrawn(claimsAtFixedInstants, "inside", (view) => {
    expectMarks(view.container, [
      [PROPERTY_LABELS.claim, "page-chrome"],
      ["·", "page-chrome"],
      [CONTROL_LABELS.not_running, "page-chrome"],
      [CONTROL_LABELS.unresponsive, "page-chrome"],
      [CONTROL_LABELS.unresponsive_since, "page-chrome"],
      [`, ${CONTROL_LABELS.takeable}`, "page-chrome"],
      [`, ${CONTROL_LABELS.takeable_now}`, "page-chrome"],
      [shortTime(FREE_FROM), "author-text"],
      [`${ageOf(CLAIMED_AT)} (${shortTime(CLAIMED_AT)})`, "author-text"],
      ["Implementer", "author-text"],
      ["Reviewer", "author-text"],
    ]);
  });
  await readDrawn(
    mountNamed("AgentName and ClaimChip, with no registry read"),
    "inside",
    (view) => {
      expectMarks(view.container, [[CONTROL_LABELS.unchecked, "page-chrome"]]);
    }
  );

  await readDrawn(
    mountNamed("GitHubLink, a pull request, a plain link and an unsafe one"),
    "inside",
    (view) => {
      expectMarks(view.container, [
        ["owner/repository#7", "author-text"],
        ["Ship it", "author-text"],
        ["open", "author-text"],
        [`${CONTROL_LABELS.github_checks}:`, "page-chrome"],
        ["success", "author-text"],
        ["https://example.com/notes", "author-text"],
        ["javascript:alert(1)", "author-text"],
        [`(${CONTROL_LABELS.external_link_unsafe})`, "page-chrome"],
      ]);
    }
  );
  await readDrawn(githubUnread, "inside", (view) => {
    expectMarks(view.container, [
      ["https://github.com/owner/repository/issues/8", "author-text"],
      ["acme/private#9", "author-text"],
      [STATE_LINES.github_unreadable, "page-chrome"],
    ]);
  });

  await readDrawn(
    mountNamed("IssueComponentsLine, an explicit set, inherited, none and not attached"),
    "inside",
    (view) => {
      expectMarks(view.container, [
        [`${PROPERTY_LABELS.components}:`, "page-chrome"],
        [CONTROL_LABELS.components_inherited_from, "page-chrome"],
        ["CORE-9", "author-text"],
        ["dispatch-server", "author-text"],
        ["Dispatch server", "author-text"],
        [`${CONTROL_LABELS.components_retired}:`, "page-chrome"],
        ["old-queue", "author-text"],
        [`${CONTROL_LABELS.components_none} —`, "page-chrome"],
        ["A process change", "author-text"],
        [CONTROL_LABELS.components_not_attached, "page-chrome"],
      ]);
    }
  );
  await readDrawn(componentsWithPicker, "inside", (view) => {
    expectMarks(view.container, [
      ["+", "page-chrome"],
      [CONTROL_LABELS.components_set, "page-chrome"],
    ]);
  });
  // While it saves, the picker says the word list's "Saving components…" with the page's mark in
  // place of its host's words, and the components line's name begins with them
  // (`componentsControlName`).
  await readDrawn(componentPickerSaving, "inside", (view) => {
    expectMarks(view.container, [[ANSWER_LINES.saving_components, "page-chrome"]]);
    expect(within(view.container).getByRole("button").textContent).toBe(
      ANSWER_LINES.saving_components
    );
  });

  await readDrawn(mountNamed("IssueLabels, its chips and its picker open"), "inside", (view) => {
    expectMarks(within(view.container).getByTestId("issue-labels"), [
      ["api", "author-text"],
      ["frontend", "author-text"],
      ["+", "page-chrome"],
    ]);
  });
  await readDrawn(labelsEmptyAndSaving, "inside", (view) => {
    expectMarks(view.container, [
      [CONTROL_LABELS.add_label, "page-chrome"],
      [ANSWER_LINES.saving_labels, "page-chrome"],
      ["api", "author-text"],
    ]);
    // The control of an issue with no label is named by the words it draws, then the word list's
    // joining word and the issue's key (WCAG 2.5.3); one that saves by the words it draws then,
    // their ellipsis dropped, and here no key is given. Each name is built from the word list's
    // words.
    const named = [
      `${CONTROL_LABELS.add_label} ${CONTROL_LABELS.control_name_to} CORE-1`,
      withoutEllipsis(ANSWER_LINES.saving_labels),
    ].map((name) => within(view.container).getByRole("button", { name }));
    expect(named.map((control) => control.textContent)).toEqual([
      CONTROL_LABELS.add_label,
      ANSWER_LINES.saving_labels,
    ]);
    // Each name begins with the words its control draws, their ellipsis dropped.
    for (const control of named) {
      const drawn = withoutEllipsis((control.textContent ?? "").trim());
      expect({
        drawn,
        nameBeginsWithWords: control.getAttribute("aria-label")?.startsWith(drawn),
      }).toEqual({ drawn, nameBeginsWithWords: true });
    }
  });

  await readDrawn(
    mountNamed("IssuePriorityLine, its own priority and an inherited one"),
    "inside",
    (view) => {
      expectMarks(view.container, [
        [PROPERTY_LABELS.advances, "page-chrome"],
        ["P2 Rocq and Lean speed work restarted", "author-text"],
        ["P1 Evals collecting data now", "author-text"],
        [CONTROL_LABELS.advances_inherited, "page-chrome"],
        ["+", "page-chrome"],
      ]);
      // The Remove draws its cross as a shape, with no text to find it by, so it is read by its
      // name.
      const remove = within(view.container).getByRole("button", {
        name: "Remove P2 Rocq and Lean speed work restarted from CORE-1",
      });
      expect(markOf(remove)).toBe("page-chrome");
    }
  );
  await readDrawn(mountNamed("IssuePriorityLine, in a row of Details"), "inside", (view) => {
    // A row of Details draws the label, so the line draws none of its own.
    expectMarks(view.container, [
      [CONTROL_LABELS.add_priority, "page-chrome"],
      ["P2 Rocq and Lean speed work restarted", "author-text"],
    ]);
    expect(within(view.container).queryAllByText(PROPERTY_LABELS.advances)).toHaveLength(0);
  });
  await readDrawn(mountNamed("IssuePriorityLine, adding a priority"), "inside", (view) => {
    expectMarks(view.container, [
      [CONTROL_LABELS.priority_to_advance, "page-chrome"],
      [CONTROL_LABELS.add, "page-chrome"],
      [CONTROL_LABELS.cancel, "page-chrome"],
    ]);
    expect(
      within(view.container).getByRole("option", { name: CONTROL_LABELS.choose_priority })
    ).toBeTruthy();
  });
  await readDrawn(priorityLineLoading, "inside", (view) => {
    expect(
      within(view.container).getByRole("option", { name: ANSWER_LINES.loading_priorities })
    ).toBeTruthy();
  });
  await readDrawn(priorityLineAtCap, "inside", (view) => {
    expectMarks(view.container, [
      [STATE_LINES.advances_at_cap, "page-chrome"],
      [ANSWER_LINES.priorities_unreadable, "page-chrome"],
      ["priority-1", "author-text"],
    ]);
  });
  // A save that failed with no answer of the server's says the page's own line, so the line
  // carries the page's mark and not an author's.
  const notSaved: Mount = {
    name: "IssuePriorityLine, a save that failed with no answer of the server's",
    node: () => {
      answerIssuePatches().mockRejectedValue(new Error("the connection dropped"));
      return (
        <IssuePriorityLine
          disabled={false}
          issue={{
            advances: [{ id: "rocq-lean", inherited: false }],
            advances_revision: 1,
            key: "CORE-1",
          }}
        />
      );
    },
    settle: async (view, user) => {
      await within(view.container).findByRole("link", { name: /Rocq and Lean/ });
      await user.click(within(view.container).getByRole("button", { name: /^Remove / }));
      await within(view.container).findByRole("alert");
    },
  };
  await readDrawn(notSaved, "inside", (view) => {
    const line = within(view.container).getByRole("alert");
    expect(line.textContent).toBe(ANSWER_LINES.priorities_not_saved);
    expect({
      authorText: line.hasAttribute(AUTHOR_TEXT_ATTRIBUTE),
      pageChrome: line.hasAttribute(PAGE_CHROME_ATTRIBUTE),
    }).toEqual({ authorText: false, pageChrome: true });
  });

  await readDrawn(
    mountNamed("AssigneeControl, both variants, assigned and not"),
    "inside",
    (view) => {
      expectMarks(view.container, [
        [CONTROL_LABELS.unassigned, "page-chrome"],
        ["alice@example.com", "author-text"],
      ]);
    }
  );
  await readDrawn(
    mountNamed("PriorityControl and PriorityEditor, both variants, set and not"),
    "inside",
    (view) => {
      expectMarks(view.container, [
        ["P0", "author-text"],
        ["P1", "author-text"],
        [PROPERTY_LABELS.priority, "page-chrome"],
      ]);
    }
  );

  await readDrawn(
    mountNamed(
      "RepoControl, its own repository and an inherited one, out of a row of Details and in one, and none in one"
    ),
    "inside",
    (view) => {
      expectMarks(view.container, [
        [`${PROPERTY_LABELS.repository}:`, "page-chrome"],
        ["acme/api", "author-text"],
        ["acme/web", "author-text"],
        ["acme/app", "author-text"],
        ["acme/site", "author-text"],
        [CONTROL_LABELS.repository_inherited, "page-chrome"],
        [`, ${CONTROL_LABELS.repository_inherited}`, "page-chrome"],
        [CONTROL_LABELS.none, "page-chrome"],
      ]);
    }
  );
  await readDrawn(repoNothingResolved, "inside", (view) => {
    expectMarks(view.container, [[CONTROL_LABELS.no_repository, "page-chrome"]]);
  });

  await readDrawn(subscribersAtHeartbeat, "inside", (view) => {
    expectMarks(view.container, [
      [STATE_LINES.subscription_wider, "page-chrome"],
      ["Implementer", "author-text"],
      ["Reviewer", "author-text"],
    ]);
  });

  // An instant and a document's connection are readings, not the page's words.
  await readDrawn(mountNamed("Timestamp, exact"), "inside", (view) => {
    expect(view.container.querySelector("time")?.hasAttribute(AUTHOR_TEXT_ATTRIBUTE)).toBe(true);
  });
  await readDrawn(
    mountNamed("ConnectionDot, each state and a pending notice"),
    "inside",
    (view) => {
      expectMarks(view.container, [
        ["connected", "author-text"],
        ["The document could not load", "author-text"],
        ["2 edits saved in this browser, not sent yet.", "author-text"],
      ]);
    }
  );
});

// Outside `NoTooltips` no shared control marks anything, as on `main`.
test("outside NoTooltips no shared control carries a mark", async () => {
  const marked: string[] = [];
  for (const mount of EVERY_MOUNT) {
    await readDrawn(mount, "outside", (view) => {
      for (const element of view.baseElement.querySelectorAll(
        `[${PAGE_CHROME_ATTRIBUTE}], [${AUTHOR_TEXT_ATTRIBUTE}], [data-approval-artifact]`
      )) {
        marked.push(`${mount.name}: ${element.outerHTML}`);
      }
    });
  }
  expect(marked).toEqual([]);
});

// Inside `NoTooltips` a stale chip draws as text the latest version its
// `title` held, in a list and at the header's size alike; outside, it keeps `main`'s words and
// `title`.
test("inside NoTooltips a stale chip draws its latest version", async () => {
  const stale: Mount = {
    name: "ApprovalChip, stale, in a list and in the header",
    node: () => (
      <>
        <ApprovalChip artifact={notesWith({ latest_version: 5, state: "stale", version: 3 })} />
        <ApprovalChip
          artifact={notesWith({ latest_version: 5, state: "stale", version: 3 })}
          size="compact"
          variant="header"
        />
      </>
    ),
  };
  const chips = (view: RenderResult) => [
    ...view.container.querySelectorAll("button[aria-haspopup='dialog']"),
  ];

  await readDrawn(stale, "inside", (view, elements) => {
    expect(chips(view).map((chip) => chip.textContent)).toEqual([
      "Approved v3 · latest v5",
      "Approved v3 · latest v5",
    ]);
    expect(titlesOf(elements)).toEqual([]);
  });

  await readDrawn(stale, "outside", (view) => {
    expect(chips(view).map((chip) => [chip.textContent, chip.getAttribute("title")])).toEqual([
      ["Approved v3 · changed since", "Approved at version 3; the latest version is 5"],
      ["Approved v3 · changed since", "Approved at version 3; the latest version is 5"],
    ]);
    expect(within(view.container).queryAllByText(/latest v5/)).toHaveLength(0);
  });
});

// Inside `NoTooltips` every colour a shared control draws is one of the page's colours in
// `PAGE_COLORS`, the focus indicator excepted.
test("inside NoTooltips every colour is one of the page's colours", async () => {
  const offenders: string[] = [];
  const colourless: string[] = [];
  for (const mount of EVERY_MOUNT) {
    await readDrawn(mount, "inside", (_view, elements) => {
      const read = offPageColours(mount.name, elements);
      offenders.push(...read.offenders);
      if (read.colours === 0 && mount.colourless !== true) colourless.push(mount.name);
    });
  }
  expect(offenders).toEqual([]);
  // A mount that drew nothing would pass the walk above with nothing read.
  expect(colourless).toEqual([]);
});

// The page's typeface holds only the characters of `subset.sh`'s UNICODES, which the face's
// `unicode-range` in styles.css repeats (`fonts/inter/subset.test.ts` holds the two equal). A
// character outside them is drawn in a face of the reader's own machine, at that face's width,
// so the page draws such a glyph as an inline svg. A value the reader or a fixture wrote is no
// glyph the page draws, so author text and each of a mount's `values` are skipped.
test("inside NoTooltips no shared control draws a character the page's typeface lacks", async () => {
  const subsetSh = readFileSync(
    join(import.meta.dir, "..", "..", "fonts", "inter", "subset.sh"),
    "utf8"
  );
  const unicodes = /^UNICODES=(\S+)$/m.exec(subsetSh)?.[1];
  if (unicodes === undefined) throw new Error("subset.sh has no UNICODES= line");
  const ranges = unicodes.split(",").map((entry) => {
    const match = /^U\+([0-9A-F]{4,6})(?:-([0-9A-F]{4,6}))?$/i.exec(entry.trim());
    if (match === null) throw new Error(`${entry} in subset.sh's UNICODES is no U+ entry`);
    const first = Number.parseInt(match[1], 16);
    return [first, match[2] === undefined ? first : Number.parseInt(match[2], 16)] as const;
  });
  const offenders: string[] = [];
  let read = 0;
  for (const mount of EVERY_MOUNT) {
    await readDrawn(mount, "inside", (_view, elements) => {
      for (const element of elements) {
        if (element.closest(`[${AUTHOR_TEXT_ATTRIBUTE}]`) !== null) continue;
        for (const node of element.childNodes) {
          if (node.nodeType !== node.TEXT_NODE) continue;
          const text = node.textContent ?? "";
          if (mount.values?.some((value) => text.includes(value))) continue;
          for (const character of text) {
            read += 1;
            const point = character.codePointAt(0) ?? 0;
            if (ranges.some(([first, last]) => point >= first && point <= last)) continue;
            const code = point.toString(16).toUpperCase().padStart(4, "0");
            offenders.push(`${mount.name}: U+${code} ${character} in "${text}"`);
          }
        }
      }
    });
  }
  expect(offenders).toEqual([]);
  // A walk that read no character would pass with nothing read.
  expect(read).toBeGreaterThan(0);
});

// The components line's picker takes its trigger's class from `ComponentPicker`, which hands it
// to `MultiSelect`. Inside `NoTooltips` that trigger, "+" or "Set components", draws in the page's
// colours in `PAGE_COLORS` and keeps none of `secondaryButtonCompact`'s; outside, its class is
// `main`'s `secondaryButtonCompact`. Inside, the trigger that draws "Set components" is named by
// those words; outside, each keeps `main`'s name.
test("inside NoTooltips the components picker's trigger draws in the page's colours", async () => {
  const triggers = (view: RenderResult, surface: Surface) =>
    [
      "Edit components of CORE-1",
      surface === "inside"
        ? `${CONTROL_LABELS.components_set} ${CONTROL_LABELS.control_name_of} CORE-2`
        : "Edit components of CORE-2",
    ].map((name) => within(view.container).getByRole("button", { name }));
  const mainsColours = tokens(secondaryButtonCompact).filter(
    (token) => colourOf(splitToken(token).utility) !== undefined
  );

  await readDrawn(componentsWithPicker, "inside", (view) => {
    const drawn = triggers(view, "inside");
    expect(drawn.map((trigger) => trigger.textContent)).toEqual([
      "+",
      CONTROL_LABELS.components_set,
    ]);
    const read = offPageColours(componentsWithPicker.name, drawn);
    expect(read.offenders).toEqual([]);
    expect(read.colours).toBeGreaterThan(0);
    for (const trigger of drawn) {
      expect(classOf(trigger).filter((token) => mainsColours.includes(token))).toEqual([]);
    }
  });

  await readDrawn(componentsWithPicker, "outside", (view) => {
    expect(triggers(view, "outside").map((trigger) => trigger.getAttribute("class"))).toEqual([
      secondaryButtonCompact,
      secondaryButtonCompact,
    ]);
  });
});

// Inside `NoTooltips` `AgentName`'s link, in each of its forms and as
// the claim chip's holder, is the page's link, in the ink with its underline, and keeps none of
// `main`'s link blue; outside, each keeps `main`'s class.
test("inside NoTooltips AgentName's link draws in the page's ink with its underline", async () => {
  // `main`'s `LINK_CLASS`, as `main`'s `AgentName.tsx` builds it.
  const mainLink = `rounded outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;
  const mainsBlue = tokens(`${linkText} ${linkHoverText}`);
  // In document order: the link, the inline link, the control's live link, the holder's link.
  const links: Mount = {
    name: "AgentName, as a link, inline and a control, and the claim chip's holder",
    node: () => (
      <>
        <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} />
        <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} inline />
        <AgentName
          actor={{ id: LIVE_SESSION, kind: "session" }}
          control={(name) => <button type="button">{name}</button>}
          kind="control"
        />
        <ClaimChip claim={{ actor: { id: LIVE_SESSION, kind: "session" }, at: CLAIMED_AT }} />
      </>
    ),
    settle: async (view) => {
      await waitFor(() =>
        expect(view.container.querySelectorAll(`[data-agent-state="running"]`)).toHaveLength(4)
      );
    },
  };

  await readDrawn(links, "inside", (view) => {
    const drawn = [...view.container.querySelectorAll("a")];
    expect(drawn).toHaveLength(4);
    for (const link of drawn) {
      expect(classOf(link)).toEqual(expect.arrayContaining([...tokens(pageInkText), "underline"]));
      expect(classOf(link).filter((token) => mainsBlue.includes(token))).toEqual([]);
    }
  });

  await readDrawn(links, "outside", (view) => {
    expect(
      [...view.container.querySelectorAll("a")].map((link) => link.getAttribute("class"))
    ).toEqual([
      mainLink,
      `${mainLink} dispatch-inline-link underline`,
      `ml-1 px-1 ${mainLink}`,
      `${mainLink} dispatch-inline-link underline`,
    ]);
  });
});

// Inside `NoTooltips` every form of `AgentName`, given a session's actor or its bare id, names a
// session the agent list does not hold and no write stamped a title on by the word list's word
// for an agent nothing names, which carries the page's mark, and never by its short id,
// an address that names nobody. Its `data-agent-name` keeps the full id, and so does the copy an
// asker's row offers. Outside, each form draws `main`'s short id, with the full id as its title.
test("inside NoTooltips a session nothing names reads as An agent, never by its short id", async () => {
  // Not in the agent list `answerDefaults` answers, and no stored title on the actor or the id.
  const nameless = "0123456789abcdef";
  const actor: Actor = { id: nameless, kind: "session" };
  const control = (name: ReactNode) => <button type="button">{name}</button>;
  const forms: readonly (readonly [string, () => ReactNode])[] = [
    ["link, actor", () => <AgentName actor={actor} />],
    ["link, id", () => <AgentName sessionId={nameless} storedTitle={undefined} />],
    ["inline, actor", () => <AgentName actor={actor} inline />],
    ["inline, id", () => <AgentName inline sessionId={nameless} storedTitle={undefined} />],
    ["static, actor", () => <AgentName actor={actor} kind="static" />],
    ["static, id", () => <AgentName kind="static" sessionId={nameless} storedTitle={undefined} />],
    ["control, actor", () => <AgentName actor={actor} control={control} kind="control" />],
    [
      "control, id",
      () => (
        <AgentName control={control} kind="control" sessionId={nameless} storedTitle={undefined} />
      ),
    ],
  ];
  const eachForm: Mount = {
    name: "AgentName, a session nothing names, in each form",
    node: () =>
      forms.map(([name, form]) => (
        <div data-case={name} key={name}>
          {form()}
        </div>
      )),
    settle: async (view) => {
      await waitFor(() =>
        expect(view.container.querySelectorAll(`[data-agent-state="not-running"]`)).toHaveLength(
          forms.length
        )
      );
    },
  };
  // Each form's name as a reader sees it: a control's in the host's control, which the live
  // link's arrow stands beside; every other form's, the form's whole text.
  const drawnNames = (root: HTMLElement) =>
    [...root.querySelectorAll("[data-case]")].map((drawn) => {
      const agent = drawn.querySelector("[data-agent-name]");
      return {
        form: drawn.getAttribute("data-case"),
        id: agent?.getAttribute("data-agent-name"),
        name: (drawn.querySelector("button") ?? drawn).textContent,
        title: agent?.getAttribute("title") ?? null,
      };
    });

  await readDrawn(eachForm, "inside", (view) => {
    const root = view.container;
    const word = CONTROL_LABELS.unnamed_author;
    expect(drawnNames(root)).toEqual(
      forms.map(([form]) => ({ form, id: nameless, name: word, title: null }))
    );
    expectMarks(root, [[word, "page-chrome"]]);
    expect(
      within(root)
        .getAllByText(word)
        .map((name) => name.closest(`[${AUTHOR_TEXT_ATTRIBUTE}]`) === null)
    ).toEqual(forms.map(() => true));
    expect(
      within(root).getAllByRole("link", { name: `Open live conversation with ${word}` })
    ).toHaveLength(2);
    expect(view.baseElement.innerHTML).not.toContain("session:");
  });

  await readDrawn(eachForm, "outside", (view) => {
    expect(drawnNames(view.container)).toEqual(
      forms.map(([form]) => ({ form, id: nameless, name: "session:01234567…", title: nameless }))
    );
  });

  // A row that offers a copy of the session's id still copies the full id inside: the asker's
  // copy handles beside its name.
  const askedByNameless: Mount = {
    name: "AskCard, asked by a session nothing names, with its handles open",
    node: () => {
      const ask = openAsk({ author: actor });
      return (
        <AskCard ask={ask} getAskThread={async () => threadOf(ask)} initialThread={threadOf(ask)} />
      );
    },
    settle: async (view, user) => {
      await settleReads();
      await user.click(
        within(view.container).getByRole("button", {
          expanded: false,
          name: CONTROL_LABELS.unnamed_author,
        })
      );
      await within(view.container).findByRole("button", { name: /Copy session ID/ });
    },
  };
  await readDrawn(askedByNameless, "inside", (view) => {
    const asker = view.container.querySelector(`[data-agent-name="${nameless}"]`);
    expect(asker?.querySelector("button")?.textContent).toBe(CONTROL_LABELS.unnamed_author);
    expect(
      within(view.container).getByRole("button", { name: `Copy session ID ${nameless}` })
    ).toBeTruthy();
    expect(view.baseElement.innerHTML).not.toContain("session:");
  });
});

// `ApprovalSteps`' version link is drawn inside `NoTooltips` alone, so it is
// the page's link, in the ink with its underline, and never `main`'s link blue.
test("inside NoTooltips ApprovalSteps' version link draws in the page's colours", async () => {
  const node = () => (
    <ApprovalSteps
      entries={stepEntries()}
      primaryArtifactId={SPEC_ID}
      versions={{ asks: [specRequest], issueKey: "CORE-1" }}
    />
  );
  await readDrawn({ name: "ApprovalSteps", node }, "inside", (view) => {
    const link = within(view.container).getByRole("link", { name: "spec.md, version 1" });
    const drawn = classOf(link);
    expect(drawn).toEqual(expect.arrayContaining([...tokens(pageInkText), "underline"]));
    expect(drawn.filter((token) => tokens(linkText).includes(token))).toEqual([]);
  });

  await readDrawn({ name: "ApprovalSteps", node }, "outside", (view) => {
    expect(within(view.container).queryByRole("link", { name: "spec.md, version 1" })).toBeNull();
  });
});

// Inside `NoTooltips` each of RouteReach's markers is the page's
// warning, its waiting orange, and none keeps `main`'s callout or muted grey; outside, each keeps
// `main`'s.
test("inside NoTooltips RouteReach's markers draw in the page's colours", async () => {
  const markers = (view: RenderResult) => {
    const scope = within(view.container);
    return {
      nobody: scope.getByTestId("route-nobody"),
      notRunning: scope.getByTestId("route-not-running"),
      unknown: scope.getByTestId("route-unknown"),
      unresponsive: scope.getByTestId("issue-route-unresponsive"),
    };
  };
  const mainsTokens = tokens(
    `${calloutWarningBg} ${calloutWarningBorder} ${calloutWarningText} ${textMutedOnSurface}`
  );

  await readDrawn({ name: "RouteReach", node: routeMarkers }, "inside", (view) => {
    const drawn = markers(view);
    for (const chip of [drawn.nobody, drawn.notRunning]) {
      expect(classOf(chip)).toEqual(expect.arrayContaining(tokens(pageWaitingChip)));
    }
    for (const words of [drawn.unknown, drawn.unresponsive]) {
      expect(classOf(words)).toEqual(expect.arrayContaining(tokens(pageStateText.waiting)));
    }
    for (const marker of Object.values(drawn)) {
      expect(classOf(marker).filter((token) => mainsTokens.includes(token))).toEqual([]);
    }
  });

  await readDrawn({ name: "RouteReach", node: routeMarkers }, "outside", (view) => {
    const drawn = markers(view);
    for (const chip of [drawn.nobody, drawn.notRunning]) {
      expect(classOf(chip)).toEqual(
        expect.arrayContaining(
          tokens(`${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`)
        )
      );
    }
    for (const words of [drawn.unknown, drawn.unresponsive]) {
      expect(classOf(words)).toEqual(expect.arrayContaining(tokens(textMutedOnSurface)));
    }
    for (const marker of Object.values(drawn)) {
      expect(classOf(marker).filter((token) => token.includes("page-"))).toEqual([]);
    }
  });
});

// Inside `NoTooltips` every value a shared control draws - a name, an id, a title, a
// key, an instant - keeps its line's width and breaks anywhere, and nothing it is drawn in holds
// its width or its line, so a long one wraps inside its row rather than run past it. Each mount
// of `EVERY_MOUNT` is drawn with long values (`LONG_VALUES`, `withLongReads`). A value is an
// element with the author-text mark, or one holding a value its mount names.
test("inside NoTooltips every value a shared control draws can wrap", async () => {
  const offences: string[] = [];
  const missing: string[] = [];
  let read = 0;
  for (const mount of EVERY_MOUNT) {
    const long = withLongReads(LONG_VALUES.get(mount) ?? mount);
    await readDrawn(long, "inside", (view, elements) => {
      for (const value of drawnValues(long, elements, missing)) {
        read += 1;
        offences.push(...wrapOffences(long.name, value, view.baseElement));
      }
    });
  }
  expect(missing).toEqual([]);
  expect([...new Set(offences)]).toEqual([]);
  // A walk that found no value would pass with nothing read.
  expect(read).toBeGreaterThan(EVERY_MOUNT.length);
});

// Each page colour pair an element of an `ON_PAGE` table draws - its text on its ground, at rest,
// under the pointer, refused, and as a field's placeholder - is one `classes.ts` registers in
// `CONTRAST_CHECKS` as words, so `palette.test.ts` measures it.
test("every page colour pair of the ON_PAGE tables is registered in CONTRAST_CHECKS", () => {
  const unregistered: string[] = [];
  const unread: string[] = [];
  const empty: string[] = [];
  for (const path of ON_PAGE_FILES) {
    const file = parsedSource(path);
    const pairs = onPagePairs(file);
    const table = tableEntries("ON_PAGE", file, 0);
    // A file whose table or whose reads of it the walk does not find under the name `ON_PAGE`
    // yields no pair to check, and would pass with nothing read.
    if (table.size === 0) empty.push(`${path}: no ON_PAGE table`);
    if (pairs.length === 0) empty.push(`${path}: no element reads an ON_PAGE entry`);
    for (const { entries, ground, line, state, text } of pairs) {
      if (registeredAsText(text, ground)) continue;
      unregistered.push(
        `${path}:${line} ${entries.map((entry) => `ON_PAGE.${entry}`).join(", ")}: ${text} text on ${ground}${state === "" ? "" : ` (${state})`}`
      );
    }
    // Each entry that draws a text colour is read by an element the walk found, so no pair of
    // the table goes unread.
    const drawn = new Set(pairs.flatMap((pair) => pair.entries));
    for (const [entry, strings] of table) {
      const draws = strings.some((className) => (pageLook(className)?.text.size ?? 0) > 0);
      if (draws && !drawn.has(entry)) unread.push(`${path}: ON_PAGE.${entry}`);
    }
  }
  expect(empty).toEqual([]);
  expect(unread).toEqual([]);
  expect([...new Set(unregistered)]).toEqual([]);
});
