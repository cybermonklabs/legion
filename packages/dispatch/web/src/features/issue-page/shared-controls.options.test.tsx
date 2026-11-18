import { afterEach, beforeEach, expect, type Mock, mock, spyOn, test } from "bun:test";
import { type ArtifactRebuildReport, MAX_ISSUE_ADVANCES } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type BoundFunctions,
  type queries,
  type RenderResult,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { graphNode } from "../../__tests__/graph-fixture";
import { artifact as specDocument } from "../../__tests__/proof-document";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import type {
  Actor,
  AnswerAskInput,
  ArchitectureSource,
  Artifact,
  ArtifactApproval,
  ArtifactReview,
  Ask,
  AskRead,
  ClaimHolderLiveness,
  Comment,
  Issue,
  IssueClaim,
  IssueComponents,
  IssueDetails,
  UserIssueState,
} from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { MultiSelect } from "../../components/MultiSelect";
import { approvalPillClassName, LabelPill, Pill, StatusPill } from "../../components/Pill";
import { PinButton } from "../../components/PinButton";
import { RefusableButton } from "../../components/RefusableButton";
import { withoutEllipsis } from "../../components/tooltips";
import { pageHeaderControl, pageInkText } from "../../theme/classes";
import { mentionOptions } from "../conversation/MentionComposer";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ConnectionDot } from "../doc/ConnectionDot";
import { AskCard } from "../inbox/AskCard";
import { ApprovalSteps } from "../issue/ApprovalSteps";
import { AssigneeControl } from "../issue/AssigneeControl";
import { badgeSelectWrapper } from "../issue/badge-select";
import { ClaimChip } from "../issue/ClaimChip";
import { GitHubLink } from "../issue/GitHubLink";
import type { StepEntry } from "../issue/gated-steps";
import { IssueComponentsLine } from "../issue/IssueComponentsLine";
import { IssueLabels } from "../issue/IssueLabels";
import { PriorityControl, PriorityEditor } from "../issue/PriorityControl";
import { RepoControl } from "../issue/RepoControl";
import {
  RouteReachUnknownMarker,
  UnreachableRouteMarker,
  UnresponsiveRouteMarker,
} from "../issue/RouteReach";
import { SubscribedAgents } from "../issue/SubscribedAgents";
import { issue as marginIssue } from "../margin/margin-fixture";
import { addPriorityControlName, IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { AgentName, AgentUnresponsiveNote, useAgentText } from "../refs/AgentName";
import { Timestamp } from "../refs/Timestamp";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useDialog } from "../shell/useDialog";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { FormActions, type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { PROPERTY_LABELS } from "./page-structure";
import { ANSWER_LINES, AUTHOR_KIND_WORDS, CONTROL_LABELS } from "./page-words";
import {
  architecture,
  CLAIMED_AT,
  classOf,
  doc,
  draw,
  GONE_SESSION,
  holdsItsLine,
  idlePriorityWrite,
  LIVE_SESSION,
  listed,
  minutesFromNow,
  openAsk,
  priorities,
  proofDocument,
  SPEC_ID,
  STALLED_SESSION,
  type Surface,
  settleReads,
  specRequest,
  threadOf,
  tokens,
  undraw,
  VERIFICATION_ID,
} from "./shared-controls-fixture";
import { useIssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * The options the shared controls take: each option's default is `main`'s shape, the options of
 * the new issue page's surface change nothing outside `NoTooltips`, and each shape an option draws
 * is the one the new issue page needs in its header, in a row of Details and for an author's name.
 * Each case draws the controls through `draw` (`shared-controls-fixture.tsx`), every read answered
 * by a stub on `api` below.
 */

function specEntry(approval?: ArtifactApproval): StepEntry {
  return { document: doc(SPEC_ID, "spec.md", approval), label: "Spec", step: "spec" };
}

function verificationEntry(approval?: ArtifactApproval): StepEntry {
  return {
    document: doc(VERIFICATION_ID, "verification.md", approval),
    label: "Verification",
    step: "criteria",
  };
}

// ---------------------------------------------------------------------------------------------
// What the controls read, each answered by a stub on `api`
// ---------------------------------------------------------------------------------------------

/** A session nothing names: the registry does not list it and no write stamped its title. Its
 *  short id, `main`'s name for it, is `session:01234567…`. */
const NAMELESS_SESSION = "0123456789abcdef";

/** The project's one architecture source, so the components line draws its picker
 *  (`IssueComponentsLine.test.tsx`'s `source`, counting the one component of `architecture`). */
const source: ArchitectureSource = {
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
};

/** The `api` reads the controls make, each answered for the whole file and restored after each
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
  readonly listIssueAsks: Mock<typeof api.listIssueAsks>;
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
    getArchitecture: spyOn(api, "getArchitecture").mockResolvedValue(architecture),
    getArtifact: spyOn(api, "getArtifact").mockResolvedValue(specDocument),
    // The spec holds one block, which one ask and one comment reference.
    getArtifactBlocks: spyOn(api, "getArtifactBlocks").mockResolvedValue([
      {
        from: 0,
        id: "block-1",
        references: { asks: 1, comments: 1 },
        to: 17,
        token: "sha256:block-1",
        type: "paragraph",
      },
    ]),
    getArtifactText: spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "The live document",
      token: "sha256:spec-text",
      version: null,
    }),
    getDocumentRead: spyOn(api, "getDocumentRead").mockImplementation(async (id) => ({
      artifact_id: id,
      read_at: null,
      version: null,
    })),
    getIssueDecisions: spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      entries: [],
      open_questions: [],
    }),
    getIssueSubscribers: spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    getReferences: spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: graphNode({ id: "ask-1", kind: "ask" }),
    }),
    githubRest: spyOn(api, "githubRest").mockImplementation(async (path) => {
      const answer = (body: unknown) =>
        new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });
      if (path === "repos/owner/repository/issues/7") {
        return answer({ state: "open", title: "Ship it" });
      }
      // A merged pull request whose one check passed, for its state and its checks' chips.
      if (path === "repos/owner/repository/pulls/8") {
        return answer({ head: { sha: "abcdef" }, merged: true, state: "closed", title: "Ship it" });
      }
      if (path.startsWith("repos/owner/repository/commits/abcdef/check-runs")) {
        return answer({
          check_runs: [
            {
              check_suite: { id: 1 },
              conclusion: "success",
              id: 1,
              name: "ci",
              status: "completed",
            },
          ],
        });
      }
      if (path.startsWith("repos/owner/repository/actions/runs")) {
        return answer({
          workflow_runs: [{ check_suite_id: 1, event: "pull_request", name: "ci" }],
        });
      }
      throw new Error(`no GitHub answer for ${path}`);
    }),
    listAgents: spyOn(api, "listAgents").mockResolvedValue([
      listed(LIVE_SESSION, "Implementer"),
      listed(STALLED_SESSION, "Reviewer", "unresponsive"),
    ]),
    listIssueAsks: spyOn(api, "listIssueAsks").mockResolvedValue([]),
    listPriorities: spyOn(api, "listPriorities").mockResolvedValue(priorities),
    listProjectArchitectureSources: spyOn(api, "listProjectArchitectureSources").mockResolvedValue([
      source,
    ]),
    listRepoProjects: spyOn(api, "listRepoProjects").mockResolvedValue([]),
    listUsers: spyOn(api, "listUsers").mockResolvedValue([
      { login: "alice@example.com" },
      { login: "bob@example.com" },
    ]),
    putDocumentRead: spyOn(api, "putDocumentRead").mockImplementation(async (id, version) => ({
      artifact_id: id,
      read_at: "2026-09-09T00:00:00Z",
      version,
    })),
    whoAmI: spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    }),
  };
});

afterEach(() => {
  for (const stub of Object.values(stubs)) stub.mockRestore();
});

/** An open decision in the spec, its own block, which the document's navigator counts. */
function blockAsk(id: string, blockId: string, question: string) {
  return {
    ...openAsk({
      block_artifact: { id: specDocument.id, primary: true, slug: "spec" },
      block_id: blockId,
      id,
      question,
      urgency: "high",
    }),
    last_reply: null,
  };
}

// ---------------------------------------------------------------------------------------------
// Reading what a control drew
// ---------------------------------------------------------------------------------------------

/** What `node` draws on `surface` once its reads have landed. React numbers each `useId` from
 *  one counter for every root (`_r_<n>_`), so two mounts of one control differ by that number
 *  alone; it is read as one placeholder. */
async function markupOf(node: ReactNode, surface: Surface): Promise<string> {
  const view = draw(node, surface);
  try {
    await settleReads();
    return view.container.innerHTML.replace(/_r_[0-9a-v]+_/g, "_r_id_");
  } finally {
    undraw(view);
  }
}

/** The element a control drew inside its `<div data-case>` of a mount, by selector. */
function caseOf(view: RenderResult, name: string): HTMLElement {
  const found = view.container.querySelector<HTMLElement>(`[data-case="${name}"]`);
  if (found === null) throw new Error(`no case ${name}`);
  return found;
}

function parentOf(element: Element): HTMLElement {
  const parent = element.parentElement;
  if (parent === null) throw new Error("the element has no parent");
  return parent;
}

function optionsOf(select: HTMLElement): string[][] {
  return [...(select as HTMLSelectElement).options].map((option) => [option.value, option.text]);
}

function sessionClaim(sessionId: string): IssueClaim {
  return { actor: { id: sessionId, kind: "session" }, at: CLAIMED_AT };
}

/** A holder that missed its heartbeats ten minutes ago and keeps the claim twenty minutes more:
 *  built on each call, since it reads the clock. */
function waitingLiveness(): ClaimHolderLiveness {
  return {
    holder_state: "unresponsive",
    last_seen: minutesFromNow(-10),
    takeable_at: minutesFromNow(20),
  };
}

function explicitComponents(ids: string[]): IssueComponents {
  return { ids, inherited_from: null, mode: "explicit", reason: null, unknown: [] };
}

function componentsIssue(components: IssueComponents, key = "CORE-1") {
  return { components, key, project: "CORE", status: "todo", title: "Ship" };
}

// ---------------------------------------------------------------------------------------------
// The cases
// ---------------------------------------------------------------------------------------------

// So that nothing the page drew is lost, the link to the version an approval request asks about,
// which the panel's old card drew, is Details' approval entry's own: inside `NoTooltips` each
// entry links its document at the version the request names - not the document's latest - or the
// version a review approved, opened in place of the outline. The header's copy holds only what
// tells a reader to act, so it links nothing. `actions`, `reasons` and `testId` default to the
// header's shape of `main`: both actions on the primary document, each reason under the list, and
// `issue-approval-steps`.
test("an approval entry links its document at the version the request asks about", () => {
  const entries = [
    specEntry({ latest_version: 2, state: "awaiting", waiting_on: "human" }),
    // The request asks about version 4 and the document stands at 5, so the link names the
    // request's version and not the latest.
    verificationEntry({
      by: { id: "bob", kind: "user" },
      latest_version: 5,
      reason: "The checks name no command.",
      state: "changes_requested",
      version: 4,
    }),
  ];
  const details = draw(
    <ApprovalSteps
      entries={entries}
      primaryArtifactId={SPEC_ID}
      versions={{ asks: [specRequest], issueKey: "CORE-1" }}
    />,
    "inside"
  );
  try {
    const scope = within(details.container);
    expect(scope.getByRole("link", { name: "spec.md, version 1" }).getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts/spec-md?v=1"
    );
    expect(
      scope.getByRole("link", { name: "verification.md, version 4" }).getAttribute("href")
    ).toBe("/issues/CORE-1/artifacts/verification-md?v=4");
    // Each default: the list's test id is `main`'s, the primary document carries both actions,
    // and the step sent back says why under the list.
    expect(scope.getByRole("list", { name: "Approvals" }).dataset.testid).toBe(
      "issue-approval-steps"
    );
    const [spec] = within(scope.getByRole("list", { name: "Approvals" })).getAllByRole("listitem");
    expect(spec.dataset.approvalStep).toBe("spec");
    expect(within(spec).getByRole("button", { name: "Approve" })).toBeInTheDocument();
    expect(within(spec).getByRole("button", { name: "Request changes" })).toBeInTheDocument();
    expect(
      details.container.querySelector('[data-approval-step-reason="criteria"]')?.textContent
    ).toContain("The checks name no command.");
  } finally {
    undraw(details);
  }

  const header = draw(
    <ApprovalSteps
      actions={false}
      entries={entries}
      primaryArtifactId={SPEC_ID}
      reasons={false}
      size="compact"
      testId="issue-header-approval-steps"
    />,
    "inside"
  );
  try {
    const scope = within(header.container);
    // The list is drawn, each entry's chip with it: what is missing is missing by its option.
    expect(scope.getByRole("list", { name: "Approvals" }).dataset.testid).toBe(
      "issue-header-approval-steps"
    );
    expect(scope.queryByTestId("issue-approval-steps")).toBeNull();
    expect(scope.getByRole("button", { name: "Awaiting approval" })).toBeInTheDocument();
    expect(scope.getByRole("button", { name: "Changes requested" })).toBeInTheDocument();
    expect(scope.queryAllByRole("link")).toHaveLength(0);
    expect(scope.queryByRole("button", { name: /^Approve( v\d+)?$/ })).toBeNull();
    expect(scope.queryByRole("button", { name: "Request changes" })).toBeNull();
    expect(header.container.querySelector("[data-approval-step-reason]")).toBeNull();
  } finally {
    undraw(header);
  }
});

// Outside `NoTooltips` the options of the new issue page's surface draw exactly what `main` draws.
// Each pair draws a control without the option and with it; inside `NoTooltips` the same pair
// differs, so a pass outside is no pair that draws nothing either way. The exception is `author`
// on a session: inside, every form of `AgentName` names a session as an author is named, so
// `author` changes nothing there either, and the case names those pairs as the same inside.
test("outside NoTooltips inRow, author, versions and exact change nothing", async () => {
  const waiting = waitingLiveness();
  const throughAgent: Actor = { act: { session: LIVE_SESSION }, id: "alice", kind: "user" };
  const steps = () => [specEntry({ latest_version: 2, state: "awaiting", waiting_on: "human" })];
  const pairs: readonly (readonly [string, () => ReactNode, () => ReactNode])[] = [
    [
      "ClaimChip inRow",
      () => <ClaimChip claim={sessionClaim(LIVE_SESSION)} />,
      () => <ClaimChip claim={sessionClaim(LIVE_SESSION)} inRow />,
    ],
    [
      "ClaimChip author, a session nothing names",
      () => <ClaimChip claim={sessionClaim(NAMELESS_SESSION)} />,
      () => <ClaimChip author claim={sessionClaim(NAMELESS_SESSION)} />,
    ],
    [
      "ClaimChip author, an act made through an agent",
      () => <ClaimChip claim={{ actor: throughAgent, at: CLAIMED_AT }} />,
      () => <ClaimChip author claim={{ actor: throughAgent, at: CLAIMED_AT }} />,
    ],
    [
      "ClaimChip inRow and author, waiting to come free",
      () => <ClaimChip claim={sessionClaim(STALLED_SESSION)} liveness={waiting} />,
      () => <ClaimChip author claim={sessionClaim(STALLED_SESSION)} inRow liveness={waiting} />,
    ],
    [
      "RepoControl inRow, no repository",
      () => <RepoControl issueKey="CORE-1" project="CORE" repo={null} resolvedRepo={null} />,
      () => <RepoControl inRow issueKey="CORE-1" project="CORE" repo={null} resolvedRepo={null} />,
    ],
    [
      "RepoControl inRow, an inherited repository",
      () => <RepoControl issueKey="CORE-1" project="CORE" repo={null} resolvedRepo="acme/web" />,
      () => (
        <RepoControl inRow issueKey="CORE-1" project="CORE" repo={null} resolvedRepo="acme/web" />
      ),
    ],
    [
      "IssueComponentsLine inRow",
      () => (
        <IssueComponentsLine issue={componentsIssue(explicitComponents(["dispatch-server"]))} />
      ),
      () => (
        <IssueComponentsLine
          inRow
          issue={componentsIssue(explicitComponents(["dispatch-server"]))}
        />
      ),
    ],
    [
      "IssuePriorityLine inRow, advancing none",
      () => (
        <IssuePriorityLine
          disabled={false}
          issue={{ advances: [], advances_revision: 1, key: "CORE-1" }}
        />
      ),
      () => (
        <IssuePriorityLine
          disabled={false}
          inRow
          issue={{ advances: [], advances_revision: 1, key: "CORE-1" }}
        />
      ),
    ],
    [
      "IssuePriorityLine inRow, advancing one",
      () => (
        <IssuePriorityLine
          disabled={false}
          issue={{
            advances: [{ id: "rocq-lean", inherited: false }],
            advances_revision: 1,
            key: "CORE-1",
          }}
        />
      ),
      () => (
        <IssuePriorityLine
          disabled={false}
          inRow
          issue={{
            advances: [{ id: "rocq-lean", inherited: false }],
            advances_revision: 1,
            key: "CORE-1",
          }}
        />
      ),
    ],
    [
      "AgentName author, a session's actor",
      () => <AgentName actor={{ id: NAMELESS_SESSION, kind: "session" }} />,
      () => <AgentName actor={{ id: NAMELESS_SESSION, kind: "session" }} author />,
    ],
    [
      "AgentName author, a session's id",
      () => <AgentName sessionId={NAMELESS_SESSION} storedTitle={undefined} />,
      () => <AgentName author sessionId={NAMELESS_SESSION} storedTitle={undefined} />,
    ],
    [
      "AgentName author, an act made through an agent",
      () => <AgentName actor={throughAgent} />,
      () => <AgentName actor={throughAgent} author />,
    ],
    [
      "ApprovalSteps versions",
      () => <ApprovalSteps entries={steps()} primaryArtifactId={SPEC_ID} />,
      () => (
        <ApprovalSteps
          entries={steps()}
          primaryArtifactId={SPEC_ID}
          versions={{ asks: [specRequest], issueKey: "CORE-1" }}
        />
      ),
    ],
    [
      "Timestamp exact",
      () => <Timestamp at={CLAIMED_AT} />,
      () => <Timestamp at={CLAIMED_AT} exact />,
    ],
  ];

  const changedOutside: { name: string; plain: string; given: string }[] = [];
  const sameInside: string[] = [];
  for (const [name, plain, given] of pairs) {
    const plainOutside = await markupOf(plain(), "outside");
    const givenOutside = await markupOf(given(), "outside");
    if (givenOutside !== plainOutside) {
      changedOutside.push({ given: givenOutside, name, plain: plainOutside });
    }
    if ((await markupOf(given(), "inside")) === (await markupOf(plain(), "inside"))) {
      sameInside.push(name);
    }
  }
  expect(changedOutside).toEqual([]);
  expect(sameInside).toEqual([
    "ClaimChip author, a session nothing names",
    "AgentName author, a session's actor",
    "AgentName author, a session's id",
  ]);
});

// The header variant of the assignee and the priority is the prototype's `.person` and `.prio`,
// drawn over the same native select as the badge, at the header line's 24 px.
test("the header variant draws the person and the priority bars over the same select", async () => {
  const expectHeaderWrapper = (wrapper: HTMLElement) => {
    expect(classOf(wrapper)).toContain("min-h-6");
    expect(classOf(wrapper).filter((token) => token.startsWith("md:min-h-"))).toEqual([]);
  };

  for (const level of [0, 1, 2, 3, null] as const) {
    const header = draw(
      <PriorityEditor
        issueKey="CORE-1"
        priority={level}
        variant="header"
        write={idlePriorityWrite}
      />,
      "inside"
    );
    const badge = draw(
      <PriorityEditor issueKey="CORE-1" priority={level} write={idlePriorityWrite} />,
      "inside"
    );
    try {
      const select = within(header.container).getByRole("combobox", { name: "Priority of CORE-1" });
      const wrapper = parentOf(select);
      expect(wrapper.querySelectorAll("rect")).toHaveLength(3);
      expect(wrapper.querySelectorAll("rect[class~='fill-page-ink']")).toHaveLength(
        level === null ? 0 : 3 - level
      );
      expect(wrapper.querySelector("[aria-hidden='true']")?.textContent).toBe(
        level === null ? PROPERTY_LABELS.priority : `P${level}`
      );
      const badgeSelect = within(badge.container).getByRole("combobox", {
        name: "Priority of CORE-1",
      });
      expect(optionsOf(select)).toEqual(optionsOf(badgeSelect));
      expect((select as HTMLSelectElement).value).toBe((badgeSelect as HTMLSelectElement).value);
      expectHeaderWrapper(wrapper);
    } finally {
      undraw(header);
      undraw(badge);
    }
  }

  const user = userEvent.setup();
  for (const assignee of ["alice@example.com", null]) {
    const header = draw(
      <AssigneeControl assignee={assignee} issueKey="CORE-1" variant="header" />,
      "inside"
    );
    const badge = draw(<AssigneeControl assignee={assignee} issueKey="CORE-1" />, "inside");
    try {
      const select = within(header.container).getByRole("combobox", { name: "Assignee of CORE-1" });
      const badgeSelect = within(badge.container).getByRole("combobox", {
        name: "Assignee of CORE-1",
      });
      // A press reads who has signed in, and both selects then offer each of them.
      await user.click(select);
      await user.click(badgeSelect);
      await waitFor(() => expect(optionsOf(select)).toHaveLength(3));
      await waitFor(() => expect(optionsOf(badgeSelect)).toHaveLength(3));
      expect(optionsOf(select)).toEqual(optionsOf(badgeSelect));
      const wrapper = parentOf(select);
      const shown = wrapper.querySelector("[aria-hidden='true']");
      if (assignee === null) {
        expect(shown?.textContent).toBe(CONTROL_LABELS.unassigned);
        expect(wrapper.querySelector("[data-initial]")).toBeNull();
      } else {
        expect(shown?.textContent).toBe(assignee);
        expect(wrapper.querySelector("[data-initial]")?.getAttribute("data-initial")).toBe("A");
      }
      expectHeaderWrapper(wrapper);
    } finally {
      undraw(header);
      undraw(badge);
    }
  }
});

// At the `compact` size the chip is the prototype's `.pill` and Request changes its `.hbtn`
// (`pageHeaderControl`); Approve has that size, filled with the page's ink and never the
// application's blue. `size` acts on every surface.
test("the compact size draws the chip and both actions at the header's size, with Approve in the ink", () => {
  for (const surface of ["inside", "outside"] as const) {
    const view = draw(
      <ApprovalSteps
        entries={[specEntry({ latest_version: 1, state: "awaiting", waiting_on: "human" })]}
        primaryArtifactId={SPEC_ID}
        reasons={false}
        size="compact"
      />,
      surface
    );
    try {
      const scope = within(view.container);
      const approve = classOf(scope.getByRole("button", { name: "Approve" }));
      expect(approve).toContain("min-h-6");
      expect(approve).toContain("bg-page-ink");
      expect(approve).not.toContain("bg-sky-700");
      expect(scope.getByRole("button", { name: "Request changes" }).getAttribute("class")).toBe(
        pageHeaderControl
      );
      const chip = classOf(scope.getByRole("button", { name: "Awaiting approval" }));
      expect(chip).toContain("min-h-6");
      expect(chip).toContain("leading-[18px]");
    } finally {
      undraw(view);
    }
  }
});

/** A gate's two steps as the layout cases draw them: the spec awaiting its review, and the
 *  verification sent back with a reason. */
function layoutEntries(): StepEntry[] {
  return [
    specEntry({ latest_version: 1, state: "awaiting", waiting_on: "human" }),
    verificationEntry({
      by: { id: "bob", kind: "user" },
      latest_version: 4,
      reason: "The checks name no command.",
      state: "changes_requested",
      version: 4,
    }),
  ];
}

// `layout` defaults to `block`, which is `main`'s list. The list and each entry are boxes of their
// own that wrap within themselves, and the primary document's chip and its two actions are each
// its entry's own item, with no box around the actions. Given or not, `block` draws the same
// markup, with `main`'s options and with the header's, on both surfaces.
test("ApprovalSteps at its default layout draws main's list", async () => {
  const pairs: readonly (readonly [string, () => ReactNode, () => ReactNode])[] = [
    [
      "main's options",
      () => <ApprovalSteps entries={layoutEntries()} primaryArtifactId={SPEC_ID} />,
      () => <ApprovalSteps entries={layoutEntries()} layout="block" primaryArtifactId={SPEC_ID} />,
    ],
    [
      "the header's options",
      () => (
        <ApprovalSteps
          actionsOn={[SPEC_ID, VERIFICATION_ID]}
          entries={layoutEntries()}
          primaryArtifactId={SPEC_ID}
          reasons={false}
          size="compact"
          testId="issue-header-approval-steps"
        />
      ),
      () => (
        <ApprovalSteps
          actionsOn={[SPEC_ID, VERIFICATION_ID]}
          entries={layoutEntries()}
          layout="block"
          primaryArtifactId={SPEC_ID}
          reasons={false}
          size="compact"
          testId="issue-header-approval-steps"
        />
      ),
    ],
  ];
  const changed: string[] = [];
  for (const surface of ["outside", "inside"] as const) {
    for (const [name, plain, given] of pairs) {
      if ((await markupOf(given(), surface)) !== (await markupOf(plain(), surface))) {
        changed.push(`${name}, ${surface}`);
      }
    }
  }
  expect(changed).toEqual([]);

  const view = draw(
    <ApprovalSteps entries={layoutEntries()} primaryArtifactId={SPEC_ID} />,
    "outside"
  );
  try {
    const list = within(view.container).getByRole("list", { name: "Approvals" });
    expect(list.dataset.testid).toBe("issue-approval-steps");
    expect(list.getAttribute("class")).toBe(
      "flex w-full min-w-0 flex-wrap items-center gap-x-4 gap-y-2"
    );
    expect(parentOf(list).getAttribute("class")).toBe("flex w-full min-w-0 flex-col gap-2");
    const entries = within(list).getAllByRole("listitem");
    expect(entries.map((entry) => entry.getAttribute("class"))).toEqual([
      "flex min-w-0 flex-wrap items-center gap-2",
      "flex min-w-0 flex-wrap items-center gap-2",
    ]);
    const [spec] = entries;
    if (spec === undefined) throw new Error("no entry for the spec");
    const chipBox = parentOf(within(spec).getByRole("button", { name: "Awaiting approval" }));
    expect(chipBox.getAttribute("class")).toBe("contents");
    expect(parentOf(chipBox)).toBe(spec);
    expect(parentOf(within(spec).getByRole("button", { name: "Approve" }))).toBe(chipBox);
    expect(parentOf(within(spec).getByRole("button", { name: "Request changes" }))).toBe(chipBox);
    expect(view.container.querySelector("[class~='flex-nowrap']")).toBeNull();
    expect(
      view.container.querySelector('[data-approval-step-reason="criteria"]')?.parentElement
    ).toBe(parentOf(list));
  } finally {
    undraw(view);
  }
});

// `keepActionsTogether` is off by default, so a caller that draws the chip alone, as today's issue
// header, a document's header and the Inbox's reading pane do, gets `main`'s markup: Approve and
// Request changes are each the chip's parent's own item, with no box around them. Given as false,
// it draws the same markup, on both surfaces.
test("ApprovalChip at its defaults draws its two actions with no box around them", async () => {
  const awaiting = doc(SPEC_ID, "spec.md", {
    latest_version: 1,
    state: "awaiting",
    waiting_on: "human",
  });
  const changed: string[] = [];
  for (const surface of ["outside", "inside"] as const) {
    const given = await markupOf(
      <ApprovalChip artifact={awaiting} keepActionsTogether={false} variant="header" />,
      surface
    );
    const plain = await markupOf(<ApprovalChip artifact={awaiting} variant="header" />, surface);
    if (given !== plain) changed.push(surface);
  }
  expect(changed).toEqual([]);

  const view = draw(
    <div data-case="host">
      <ApprovalChip artifact={awaiting} variant="header" />
    </div>,
    "outside"
  );
  try {
    const host = caseOf(view, "host");
    const chipBox = parentOf(within(host).getByRole("button", { name: /^Awaiting approval/ }));
    expect(parentOf(within(host).getByRole("button", { name: "Approve" }))).toBe(chipBox);
    expect(parentOf(within(host).getByRole("button", { name: "Request changes" }))).toBe(chipBox);
    expect(host.querySelector("[class~='flex-nowrap']")).toBeNull();
  } finally {
    undraw(view);
  }
});

// At `layout="contents"` neither the list nor an entry draws a box, so in the new issue page's
// header line each entry's name, its chip and its actions are that line's items; the actions are
// one box that does not wrap, so Approve and Request changes never land on two lines. The list
// keeps its name and test id, and a reason is still drawn. `layout` acts on every surface.
test("ApprovalSteps with layout contents adds no box of its own and keeps each entry's actions together", () => {
  for (const surface of ["inside", "outside"] as const) {
    const view = draw(
      <div className="flex flex-wrap" data-case="line">
        <ApprovalSteps
          actionsOn={[SPEC_ID, VERIFICATION_ID]}
          entries={layoutEntries()}
          layout="contents"
          primaryArtifactId={SPEC_ID}
          size="compact"
          testId="issue-header-approval-steps"
        />
      </div>,
      surface
    );
    try {
      const line = caseOf(view, "line");
      /** The class of each element between `element` and the caller's line. */
      const boxesAbove = (element: Element) => {
        const boxes: string[] = [];
        for (let at = parentOf(element); at !== line; at = parentOf(at)) {
          boxes.push(at.getAttribute("class") ?? "");
        }
        return boxes;
      };
      const list = within(line).getByRole("list", { name: "Approvals" });
      expect(list.dataset.testid).toBe("issue-header-approval-steps");
      const entries = within(list).getAllByRole("listitem");
      expect(entries.map((entry) => entry.dataset.approvalStep)).toEqual(["spec", "criteria"]);
      const [spec, verification] = entries;
      if (spec === undefined || verification === undefined) {
        throw new Error("an entry is missing");
      }

      // Each action is found by the words it draws, which its name begins with: inside
      // `NoTooltips` the name goes on to say its document, since `actionsOn` names two (read in
      // `shared-writes.test.ts`).
      const groups = [spec, verification].map((entry) => {
        const approve = within(entry).getByRole("button", { name: /^Approve\b/u });
        const group = parentOf(approve);
        expect(parentOf(within(entry).getByRole("button", { name: /^Request changes\b/u }))).toBe(
          group
        );
        expect(group.children).toHaveLength(2);
        expect(classOf(group)).toEqual(
          expect.arrayContaining(["flex-nowrap", "whitespace-nowrap"])
        );
        expect(classOf(group)).not.toContain("contents");
        return group;
      });
      const reason = line.querySelector('[data-approval-step-reason="criteria"]');
      if (reason === null) throw new Error("the verification's reason is not drawn");
      expect(reason.textContent).toContain("The checks name no command.");
      const names = entries.map((entry) => {
        const name = entry.firstElementChild;
        if (name === null) throw new Error(`the ${entry.dataset.approvalStep} entry draws nothing`);
        return name;
      });
      expect(names.map((name) => name.textContent)).toEqual(["Spec", "Verification"]);
      const items: Element[] = [
        ...names,
        within(spec).getByRole("button", { name: "Awaiting approval" }),
        within(verification).getByRole("button", { name: "Changes requested" }),
        ...groups,
        reason,
      ];
      // Each item's every box up to the line draws none, so the item is the line's own.
      const boxed = items.flatMap((item) =>
        boxesAbove(item)
          .filter((box) => box !== "contents")
          .map((box) => `${item.textContent}: ${box}`)
      );
      expect(boxed).toEqual([]);
    } finally {
      undraw(view);
    }

    // A step with no request draws its "Not requested" mark, which is an item of the caller's
    // line as the others are.
    const unasked = draw(
      <div className="flex flex-wrap" data-case="line">
        <ApprovalSteps
          actionsOn={[SPEC_ID]}
          entries={[
            specEntry({ latest_version: 1, state: "awaiting", waiting_on: "human" }),
            { document: undefined, label: "Verification", step: "criteria" },
          ]}
          layout="contents"
          primaryArtifactId={SPEC_ID}
          size="compact"
          testId="issue-header-approval-steps"
        />
      </div>,
      surface
    );
    try {
      const line = caseOf(unasked, "line");
      const list = within(line).getByRole("list", { name: "Approvals" });
      const [, criteria] = within(list).getAllByRole("listitem");
      if (criteria === undefined) throw new Error("the step with no request is missing");
      const mark = within(criteria).getByText(
        surface === "inside" ? CONTROL_LABELS.not_requested : "Not requested"
      );
      const boxed: string[] = [];
      for (let at = parentOf(mark); at !== line; at = parentOf(at)) {
        const box = at.getAttribute("class") ?? "";
        if (box !== "contents") boxed.push(box);
      }
      expect(boxed, "the Not requested mark stands in a box of its own").toEqual([]);
    } finally {
      undraw(unasked);
    }
  }
});

// Inside `NoTooltips` a control drawn as the value of a row of Details draws no label of its own,
// since the row draws it, and says Details' words.
test("inRow draws no label and the words of Details", async () => {
  const waiting = waitingLiveness();
  const free: ClaimHolderLiveness = {
    holder_state: "unresponsive",
    last_seen: minutesFromNow(-90),
    takeable_at: minutesFromNow(-30),
  };
  const view = draw(
    <>
      <div data-case="held">
        <ClaimChip claim={sessionClaim(LIVE_SESSION)} inRow />
      </div>
      <div data-case="waiting">
        <ClaimChip claim={sessionClaim(STALLED_SESSION)} inRow liveness={waiting} />
      </div>
      <div data-case="free">
        <ClaimChip claim={sessionClaim(STALLED_SESSION)} inRow liveness={free} />
      </div>
      <div data-case="no repository">
        <RepoControl inRow issueKey="CORE-1" project="CORE" repo={null} resolvedRepo={null} />
      </div>
      <div data-case="repository">
        <RepoControl
          inRow
          issueKey="CORE-2"
          project="CORE"
          repo="acme/api"
          resolvedRepo="acme/api"
        />
      </div>
      <div data-case="components">
        <IssueComponentsLine
          inRow
          issue={componentsIssue(explicitComponents(["dispatch-server"]), "CORE-3")}
        />
      </div>
      <div data-case="no priority">
        <IssuePriorityLine
          disabled={false}
          inRow
          issue={{ advances: [], advances_revision: 1, key: "CORE-4" }}
        />
      </div>
      <div data-case="priority">
        <IssuePriorityLine
          disabled={false}
          inRow
          issue={{
            advances: [{ id: "rocq-lean", inherited: false }],
            advances_revision: 1,
            key: "CORE-5",
          }}
        />
      </div>
    </>,
    "inside"
  );
  try {
    await settleReads();
    const scope = within(view.container);
    await scope.findByText("Dispatch server");
    await scope.findByRole("link", { name: /Rocq and Lean/ });

    const text = view.container.textContent ?? "";
    for (const label of [
      PROPERTY_LABELS.claim,
      `${PROPERTY_LABELS.repository}:`,
      `${PROPERTY_LABELS.components}:`,
      PROPERTY_LABELS.advances,
    ]) {
      expect(text).not.toContain(label);
    }

    // The claim is plain words in the ink, and no part of it is held whole.
    for (const name of ["held", "waiting", "free"]) {
      const chip = within(caseOf(view, name)).getByTestId("issue-claim");
      expect(classOf(chip)).toEqual(expect.arrayContaining(tokens(pageInkText)));
      expect(classOf(chip)).not.toContain("rounded-full");
      expect(classOf(chip)).not.toContain("whitespace-nowrap");
      const words = [...chip.querySelectorAll("span, time")].filter(
        (element) => element.closest("[data-agent-name]") === null
      );
      expect(words.length).toBeGreaterThan(0);
      expect(words.filter((element) => classOf(element).includes("shrink-0"))).toEqual([]);
    }
    const takeableAt = waiting.takeable_at ?? "";
    const comesFree = within(caseOf(view, "waiting")).getByTestId("issue-claim-takeable");
    expect(comesFree.textContent?.startsWith(`, ${CONTROL_LABELS.takeable} `)).toBe(true);
    const instant = comesFree.querySelector("time");
    expect(instant?.getAttribute("dateTime")).toBe(takeableAt);
    expect(instant?.textContent).toContain(
      new Date(takeableAt).toLocaleDateString(undefined, { dateStyle: "short" })
    );
    expect(instant?.textContent).toContain(
      new Date(takeableAt).toLocaleTimeString(undefined, { timeStyle: "short" })
    );
    expect(within(caseOf(view, "free")).getByTestId("issue-claim-takeable").textContent).toBe(
      `, ${CONTROL_LABELS.takeable_now}`
    );

    // With nothing resolved the row draws `main`'s empty box, with its placeholder and a
    // description in the page's words, and no word of its own such as "None".
    const noRepository = within(caseOf(view, "no repository"));
    const emptyBox = noRepository.getByRole("combobox", {
      description: CONTROL_LABELS.no_repository,
      name: "Repository of CORE-1",
    });
    expect([(emptyBox as HTMLInputElement).value, emptyBox.getAttribute("placeholder")]).toEqual([
      "",
      "owner/name",
    ]);
    expect(noRepository.queryByText(CONTROL_LABELS.none)).toBeNull();
    expect(
      within(caseOf(view, "repository")).getByRole("combobox", { name: "Repository of CORE-2" })
    ).toBeInTheDocument();

    const components = within(caseOf(view, "components")).getByTestId("issue-components");
    expect(within(components).getByText("Dispatch server")).toBeInTheDocument();
    expect(within(components).queryByText("dispatch-server")).toBeNull();

    expect(
      within(caseOf(view, "no priority")).getByRole("button", { name: "Add a priority to CORE-4" })
        .textContent
    ).toBe(CONTROL_LABELS.add_priority);
    expect(view.container.querySelector('[data-testid="issue-advances"]')).toBeNull();
  } finally {
    undraw(view);
  }
});

/** Fixed instants for the claims whose markup a case records: the holder's last heartbeat, a
 *  moment it comes free that is still ahead, and one already past. */
const CLAIM_HEARD_AT = "2026-09-24T05:50:00Z";
const CLAIM_FREE_FROM = "2099-09-24T06:00:00Z";
const CLAIM_FREED_AT = "2026-09-24T06:20:00Z";

/** Each state a claim is drawn in: held, lapsed, unresponsive with its heartbeat and the moment it
 *  comes free - still ahead (`waiting`) or past (`free`) - and, with the agent list unreadable,
 *  unchecked. */
const CLAIM_STATES = ["held", "lapsed", "waiting", "free", "unchecked"] as const;
type ClaimState = (typeof CLAIM_STATES)[number];

function claimIn(state: ClaimState, inRow: boolean): ReactNode {
  if (state === "lapsed") return <ClaimChip claim={sessionClaim(GONE_SESSION)} inRow={inRow} />;
  if (state === "waiting" || state === "free") {
    return (
      <ClaimChip
        claim={sessionClaim(STALLED_SESSION)}
        inRow={inRow}
        liveness={{
          holder_state: "unresponsive",
          last_seen: CLAIM_HEARD_AT,
          takeable_at: state === "waiting" ? CLAIM_FREE_FROM : CLAIM_FREED_AT,
        }}
      />
    );
  }
  return <ClaimChip claim={sessionClaim(LIVE_SESSION)} inRow={inRow} />;
}

/** What `read` finds in the claim `state` draws on `surface`, once the agent list has answered,
 *  or, for `unchecked`, failed. */
async function readClaim<T>(
  state: ClaimState,
  surface: Surface,
  inRow: boolean,
  read: (chip: HTMLElement) => T
): Promise<T> {
  if (state === "unchecked") {
    stubs.listAgents.mockRejectedValueOnce(new Error("Envoy listener unreachable"));
  }
  const view = draw(claimIn(state, inRow), surface);
  try {
    await waitFor(() => expect(stubs.listAgents).toHaveBeenCalled());
    await settleReads();
    return read(within(view.container).getByTestId("issue-claim"));
  } finally {
    undraw(view);
  }
}

/** A claim's markup as its own elements draw it: each element's tag and class and each text
 *  node, in order. The holder's `AgentName` and each `<time>` stand by their tag and class: what
 *  they hold is theirs, and a time's words are the clock's. */
function claimShape(chip: Element): string[] {
  const shape: string[] = [];
  const walk = (node: Node) => {
    if (node.nodeType === node.TEXT_NODE) {
      shape.push(JSON.stringify(node.textContent));
      return;
    }
    if (!(node instanceof Element)) return;
    shape.push(`<${node.tagName.toLowerCase()} class="${node.getAttribute("class") ?? ""}">`);
    if (node.hasAttribute("data-agent-name") || node.tagName === "TIME") return;
    for (const child of node.childNodes) walk(child);
  };
  walk(chip);
  return shape;
}

/** A text node as `claimShape` records it. */
function textNode(words: string): string {
  return JSON.stringify(words);
}

/** The claim's markup in each state as `ClaimChip` drew it before a claim in a row of Details
 *  became a sentence (`claimShape`): `main`'s pill outside `NoTooltips`, with `inRow` or without,
 *  and the header's pill inside it. Inside, each of the page's words is built from its list
 *  (`PROPERTY_LABELS`, `CONTROL_LABELS`); outside, each is `main`'s string. */
const RECORDED_CLAIMS: Readonly<Record<Surface, Readonly<Record<ClaimState, readonly string[]>>>> =
  {
    inside: {
      free: [
        '<span class="inline-flex min-w-0 max-w-full flex-wrap items-center rounded-full px-2 py-1 text-xs font-medium [overflow-wrap:anywhere] border-page-hair bg-page-pane text-page-muted min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        textNode(PROPERTY_LABELS.claim),
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<time class="min-w-0 [overflow-wrap:anywhere] max-w-full [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        textNode(CONTROL_LABELS.unresponsive),
        '<span class="">',
        textNode(` ${CONTROL_LABELS.unresponsive_since} `),
        '<time class="max-w-full [overflow-wrap:anywhere]">',
        '<span class="">',
        textNode(`, ${CONTROL_LABELS.takeable_now}`),
      ],
      held: [
        '<span class="inline-flex min-w-0 max-w-full flex-wrap items-center rounded-full px-2 py-1 text-xs font-medium [overflow-wrap:anywhere] bg-page-mark text-page-ink min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        textNode(PROPERTY_LABELS.claim),
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<time class="min-w-0 [overflow-wrap:anywhere] max-w-full [overflow-wrap:anywhere]">',
      ],
      lapsed: [
        '<span class="inline-flex min-w-0 max-w-full flex-wrap items-center rounded-full px-2 py-1 text-xs font-medium [overflow-wrap:anywhere] border-page-hair bg-page-pane text-page-muted min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        textNode(PROPERTY_LABELS.claim),
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<time class="min-w-0 [overflow-wrap:anywhere] max-w-full [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        textNode(CONTROL_LABELS.not_running),
      ],
      unchecked: [
        '<span class="inline-flex min-w-0 max-w-full flex-wrap items-center rounded-full px-2 py-1 text-xs font-medium [overflow-wrap:anywhere] bg-page-mark text-page-ink min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        textNode(PROPERTY_LABELS.claim),
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<time class="min-w-0 [overflow-wrap:anywhere] max-w-full [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        textNode(CONTROL_LABELS.unchecked),
      ],
      waiting: [
        '<span class="inline-flex min-w-0 max-w-full flex-wrap items-center rounded-full px-2 py-1 text-xs font-medium [overflow-wrap:anywhere] bg-page-mark text-page-ink min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        textNode(PROPERTY_LABELS.claim),
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<time class="min-w-0 [overflow-wrap:anywhere] max-w-full [overflow-wrap:anywhere]">',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        '"·"',
        '<span class="min-w-0 [overflow-wrap:anywhere]">',
        textNode(CONTROL_LABELS.unresponsive),
        '<span class="">',
        textNode(` ${CONTROL_LABELS.unresponsive_since} `),
        '<time class="max-w-full [overflow-wrap:anywhere]">',
        '<span class="">',
        textNode(`, ${CONTROL_LABELS.takeable} `),
        '<time class="min-w-0 [overflow-wrap:anywhere]">',
      ],
    },
    outside: {
      free: [
        '<span class="inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-100 dark:bg-slate-800 text-slate-600 dark:text-slate-400 min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        '"Claimed by"',
        '<span class="min-w-0 [overflow-wrap:anywhere] min-w-0 truncate">',
        '<span class="shrink-0">',
        '"·"',
        '<time class="shrink-0">',
        '<span class="shrink-0">',
        '"·"',
        '<span class="shrink-0">',
        '"unresponsive"',
        '<span class="">',
        '" since "',
        '<time class="">',
        '<span class="">',
        '", takeable now"',
      ],
      held: [
        '<span class="inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        '"Claimed by"',
        '<span class="min-w-0 [overflow-wrap:anywhere] min-w-0 truncate">',
        '<span class="shrink-0">',
        '"·"',
        '<time class="shrink-0">',
      ],
      lapsed: [
        '<span class="inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-100 dark:bg-slate-800 text-slate-600 dark:text-slate-400 min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        '"Claimed by"',
        '<span class="min-w-0 [overflow-wrap:anywhere] min-w-0 truncate">',
        '<span class="shrink-0">',
        '"·"',
        '<time class="shrink-0">',
        '<span class="shrink-0">',
        '"·"',
        '<span class="shrink-0">',
        '"not running"',
      ],
      unchecked: [
        '<span class="inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        '"Claimed by"',
        '<span class="min-w-0 [overflow-wrap:anywhere] min-w-0 truncate">',
        '<span class="shrink-0">',
        '"·"',
        '<time class="shrink-0">',
      ],
      waiting: [
        '<span class="inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 min-w-0 max-w-full gap-1">',
        '<span class="shrink-0">',
        '"Claimed by"',
        '<span class="min-w-0 [overflow-wrap:anywhere] min-w-0 truncate">',
        '<span class="shrink-0">',
        '"·"',
        '<time class="shrink-0">',
        '<span class="shrink-0">',
        '"·"',
        '<span class="shrink-0">',
        '"unresponsive"',
        '<span class="">',
        '" since "',
        '<time class="">',
        '<span class="">',
        '", takeable "',
        '<time class="">',
      ],
    },
  };

/** The display tokens that make an element a flex or grid box, whose items each keep a line of
 *  their own when they do not fit. */
const BOX_DISPLAYS = ["flex", "inline-flex", "grid", "inline-grid"];

// Inside `NoTooltips` a claim in a row of Details is plain words that wrap between words as a
// sentence does (the prototype's `.frow .fv`): no flex box moves a piece that does not fit whole
// to a line of its own, and a space stands between each two pieces. Outside `NoTooltips`, with
// `inRow` or without, and inside it as the header's pill, the claim's markup is the one recorded
// in `RECORDED_CLAIMS`.
test("inRow draws the claim as a sentence that wraps between words", async () => {
  for (const state of CLAIM_STATES) {
    const read = await readClaim(state, "inside", true, (chip) => {
      const timeAt = (selector: string) => chip.querySelector(selector)?.textContent ?? "";
      const claimed = timeAt(":scope > time");
      const unresponsive = `${CONTROL_LABELS.unresponsive} ${CONTROL_LABELS.unresponsive_since} ${timeAt(
        "[data-testid='issue-claim-unresponsive-since'] > time"
      )},`;
      const pieces: Record<ClaimState, string[]> = {
        free: ["Reviewer", "·", claimed, "·", unresponsive, CONTROL_LABELS.takeable_now],
        held: ["Implementer", "·", claimed],
        lapsed: [CONTROL_LABELS.unnamed_author, "·", claimed, "·", CONTROL_LABELS.not_running],
        unchecked: [CONTROL_LABELS.unnamed_author, "·", claimed, "·", CONTROL_LABELS.unchecked],
        waiting: [
          "Reviewer",
          "·",
          claimed,
          "·",
          unresponsive,
          CONTROL_LABELS.takeable,
          new Date(CLAIM_FREE_FROM).toLocaleString(undefined, {
            dateStyle: "short",
            timeStyle: "short",
          }),
        ],
      };
      return {
        claimed,
        className: classOf(chip),
        sentence: pieces[state].join(" "),
        text: chip.textContent,
      };
    });
    expect(read.claimed).not.toBe("");
    expect(read.className.filter((token) => BOX_DISPLAYS.includes(token))).toEqual([]);
    expect(read.className).toEqual(
      expect.arrayContaining(["max-w-full", "[overflow-wrap:anywhere]"])
    );
    expect(read.text).toBe(read.sentence);
  }

  const drawn: Record<string, readonly string[]> = {};
  const want: Record<string, readonly string[]> = {};
  for (const state of CLAIM_STATES) {
    drawn[`outside ${state}`] = await readClaim(state, "outside", false, claimShape);
    drawn[`outside inRow ${state}`] = await readClaim(state, "outside", true, claimShape);
    drawn[`inside ${state}`] = await readClaim(state, "inside", false, claimShape);
    want[`outside ${state}`] = RECORDED_CLAIMS.outside[state];
    want[`outside inRow ${state}`] = RECORDED_CLAIMS.outside[state];
    want[`inside ${state}`] = RECORDED_CLAIMS.inside[state];
  }
  expect(drawn).toEqual(want);
});

// At a phone's 320 px the claim in a row of Details wraps word by word: inside `NoTooltips`, drawn
// with the most words the chip draws (an unresponsive holder, the heartbeat it was last seen at
// and the moment it comes free, each instant a `<time>`), no piece of the claim carries a class of
// `HOLDS_ITS_LINE` or an inline `flex-shrink: 0` (`holdsItsLine`). The separators between the
// pieces are hidden from a reader and hold no word, and `AgentName`'s state mark is a dot with no
// word in it.
test("in Details' row every word of the claim wraps, and none is held whole against the row", async () => {
  const read = await readClaim("waiting", "inside", true, (chip) => ({
    held: [chip, ...chip.querySelectorAll("*")]
      .filter(
        (element) =>
          element.getAttribute("aria-hidden") !== "true" &&
          !element.matches("[data-agent-name] > [role='img']")
      )
      .flatMap((element) =>
        holdsItsLine(element).map(
          (token) =>
            `<${element.tagName.toLowerCase()} class="${element.getAttribute("class") ?? ""}">${element.textContent}: ${token}`
        )
      ),
    times: chip.querySelectorAll("time").length,
    unresponsive: chip.querySelector("[data-testid='issue-claim-unresponsive']") !== null,
  }));
  expect({ times: read.times, unresponsive: read.unresponsive }).toEqual({
    times: 3,
    unresponsive: true,
  });
  expect(read.held).toEqual([]);
});

// Inside `NoTooltips` the priority's add control in a row of Details is, from `md`, as tall as the
// controls beside it there (the repository's box, `main`'s input, read from the box drawn in a
// row of Details), and keeps the 44 px below `md`. Outside, its class is `main`'s.
test("the priority's add control has the height of its siblings in Details", async () => {
  const repository = draw(
    <RepoControl inRow issueKey="CORE-1" project="CORE" repo="acme/api" resolvedRepo="acme/api" />,
    "inside"
  );
  const siblings = (() => {
    try {
      return classOf(
        within(repository.container).getByRole("combobox", { name: "Repository of CORE-1" })
      ).filter((token) => token.startsWith("md:min-h-"));
    } finally {
      undraw(repository);
    }
  })();
  expect(siblings).toEqual(["md:min-h-7"]);
  for (const surface of ["inside", "outside"] as const) {
    const view = draw(addablePriorityLine(surface), surface);
    try {
      await settleReads();
      const add = addControlOf(view, surface);
      if (surface === "inside") {
        expect(classOf(add).filter((token) => token.startsWith("md:min-h-"))).toEqual(siblings);
        expect(classOf(add)).toEqual(expect.arrayContaining(["min-h-11", "py-2", "md:py-1"]));
      } else {
        expect(add.getAttribute("class")).toBe(
          "inline-flex min-h-11 shrink-0 items-center rounded-full px-2 py-2 text-sm whitespace-nowrap outline-none bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100"
        );
      }
    } finally {
      undraw(view);
    }
  }
});

// `chipClassName` is the chip's whole shape - only what holds a chip on its line is kept - and the
// labels wrap. It acts on every surface; without it the labels keep `main`'s strings. Inside
// `NoTooltips` what holds the chip on its line also breaks a long label rather than keeping it
// whole.
test("chipClassName replaces the chip's class and lets labels wrap", async () => {
  const given = "rounded-[5px] border px-1.5 leading-5";
  const mainChip =
    "inline-flex min-h-11 shrink-0 items-center rounded-full px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300";
  const mainRail = "relative flex shrink-0 flex-nowrap items-center gap-1";
  const labels = (chipClassName: string | undefined) => (
    <IssueLabels
      chipClassName={chipClassName}
      disabled={false}
      labels={["api", "frontend"]}
      onSave={async () => undefined}
      project="CORE"
      saving={false}
      variant="rail"
    />
  );

  for (const surface of ["outside", "inside"] as const) {
    const view = draw(labels(given), surface);
    try {
      const rail = within(view.container).getByTestId("issue-labels");
      expect(classOf(rail)).toContain("flex-wrap");
      expect(classOf(rail)).not.toContain("flex-nowrap");
      for (const label of ["api", "frontend"]) {
        const chip = within(rail).getByText(label);
        expect(chip.getAttribute("class")).toBe(
          surface === "outside"
            ? `inline-flex shrink-0 items-center whitespace-nowrap ${given}`
            : `inline-flex max-w-full items-center [overflow-wrap:anywhere] ${given}`
        );
      }
    } finally {
      undraw(view);
    }
  }

  const plain = draw(labels(undefined), "outside");
  try {
    const rail = within(plain.container).getByTestId("issue-labels");
    expect(rail.getAttribute("class")).toBe(mainRail);
    for (const label of ["api", "frontend"]) {
      expect(within(rail).getByText(label).getAttribute("class")).toBe(mainChip);
    }
  } finally {
    undraw(plain);
  }
});

// Inside `NoTooltips` a label chip stays within its line and breaks a long label anywhere, with
// `chipClassName` and without it, because a chip that kept a 40-character label with no space
// whole (the longest label `IssueLabels` takes) ran past Details. Outside, each chip keeps
// `main`'s string, and the prefix it keeps before a given `chipClassName`.
test("inside NoTooltips a label chip wraps a long label and is never cut", () => {
  const given = "rounded-[5px] border px-1.5 leading-5";
  const long = "dispatchissuepagedetailslabelwithnospace";
  expect(long).toHaveLength(40);
  expect(long).not.toMatch(/\s/u);
  const mainChip =
    "inline-flex min-h-11 shrink-0 items-center rounded-full px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300";
  const wraps = ["[overflow-wrap:anywhere]", "max-w-full"];
  const cuts = ["shrink-0", "whitespace-nowrap"];

  for (const chipClassName of [undefined, given]) {
    for (const variant of ["default", "rail"] as const) {
      for (const surface of ["inside", "outside"] as const) {
        const view = draw(
          <IssueLabels
            chipClassName={chipClassName}
            disabled={false}
            labels={[long]}
            onSave={async () => undefined}
            project="CORE"
            saving={false}
            variant={variant}
          />,
          surface
        );
        try {
          const chip = within(view.container).getByText(long);
          const drawn = classOf(chip);
          // Each read names its case, so a failure says which chip was cut.
          const which = { chipClassName, surface, variant };
          if (surface === "inside") {
            expect({
              ...which,
              cuts: drawn.filter((token) => cuts.includes(token)),
              wraps: drawn.filter((token) => wraps.includes(token)).sort(),
            }).toEqual({ ...which, cuts: [], wraps });
          } else {
            expect({ ...which, class: chip.getAttribute("class") }).toEqual({
              ...which,
              class:
                chipClassName === undefined
                  ? mainChip
                  : `inline-flex shrink-0 items-center whitespace-nowrap ${given}`,
            });
          }
        } finally {
          undraw(view);
        }
      }
    }
  }
});

// Inside `NoTooltips` the control of an issue with no label says what it does, as the priority's
// says "Add a priority", because its row of Details already names the property and the
// property's name in the control drew "Labels None Labels". Its accessible name there begins with
// the words it draws and ends with the joining word of `page-words.ts` and the issue's key, as the
// priority's "Add a priority to CORE-1" does, so a voice user says what they read (WCAG 2.5.3
// Label in Name); with no key given it is those words alone. Outside it says `main`'s word and
// keeps `main`'s name, "Edit labels", key or no key. Each name inside is built from the words of
// `page-words.ts`.
test("inside NoTooltips the empty labels control says Add a label", () => {
  for (const variant of ["default", "rail"] as const) {
    for (const surface of ["inside", "outside"] as const) {
      for (const issueKey of ["CORE-1", undefined]) {
        const view = draw(
          <IssueLabels
            disabled={false}
            issueKey={issueKey}
            labels={[]}
            onSave={async () => undefined}
            project="CORE"
            saving={false}
            variant={variant}
          />,
          surface
        );
        try {
          const name =
            surface === "outside"
              ? "Edit labels"
              : issueKey === undefined
                ? CONTROL_LABELS.add_label
                : `${CONTROL_LABELS.add_label} ${CONTROL_LABELS.control_name_to} ${issueKey}`;
          const control = within(view.container).getByRole("button", { name });
          const which = { issueKey, surface, variant };
          expect({ ...which, word: control.textContent }).toEqual({
            ...which,
            word: surface === "inside" ? CONTROL_LABELS.add_label : "Labels",
          });
          if (surface === "inside") {
            // Its name begins with the words it draws.
            const drawn = withoutEllipsis((control.textContent ?? "").trim());
            expect({
              ...which,
              nameBeginsWithWord: control.getAttribute("aria-label")?.startsWith(drawn),
            }).toEqual({ ...which, nameBeginsWithWord: true });
            // Neither its words nor any attribute of it says the property's name.
            expect({ ...which, control: control.outerHTML.includes("Labels") }).toEqual({
              ...which,
              control: false,
            });
          }
        } finally {
          undraw(view);
        }
      }
    }
  }
});

// While its labels save, the control inside `NoTooltips` draws the page's `saving_labels` whatever
// it holds, and it stays focusable, marked busy. Its accessible name then begins with those words,
// their ellipsis dropped, and ends with the joining word of `page-words.ts` and the issue's key
// ("Saving labels of CORE-1"), or is those words alone with no key given, so a voice user says
// what they read (WCAG 2.5.3 Label in Name). Outside it draws `main`'s "Saving labels…" and keeps
// `main`'s name, "Edit labels", in every state. Each name and word inside is built from the words
// of `page-words.ts`.
test("inside NoTooltips the saving labels control is named by its visible words", () => {
  const saving = withoutEllipsis(ANSWER_LINES.saving_labels);
  for (const labels of [[], ["api"]]) {
    for (const surface of ["inside", "outside"] as const) {
      for (const issueKey of ["CORE-1", undefined]) {
        const view = draw(
          <IssueLabels
            disabled={false}
            issueKey={issueKey}
            labels={labels}
            onSave={async () => undefined}
            project="CORE"
            saving
          />,
          surface
        );
        try {
          const name =
            surface === "outside"
              ? "Edit labels"
              : issueKey === undefined
                ? saving
                : `${saving} ${CONTROL_LABELS.control_name_of} ${issueKey}`;
          const control = within(view.container).getByRole("button", { name });
          const which = { issueKey, labels, surface };
          expect({
            ...which,
            busy: control.getAttribute("aria-disabled"),
            word: control.textContent,
          }).toEqual({
            ...which,
            busy: "true",
            word: surface === "inside" ? ANSWER_LINES.saving_labels : "Saving labels…",
          });
          if (surface === "inside") {
            // Its name begins with the words it draws, their ellipsis dropped.
            const drawn = withoutEllipsis((control.textContent ?? "").trim());
            expect({
              ...which,
              nameBeginsWithWords: control.getAttribute("aria-label")?.startsWith(drawn),
            }).toEqual({ ...which, nameBeginsWithWords: true });
          }
        } finally {
          undraw(view);
        }
      }
    }
  }
});

/** Each actor `useAgentText().author` names, by what the row holds, and what `label` names it. */
function AuthorProbe({
  actors,
}: {
  actors: readonly (readonly [string, Actor | undefined])[];
}): ReactNode {
  const text = useAgentText();
  return (
    <ul>
      {actors.map(([name, actor]) => (
        <li data-case={`author: ${name}`} key={name}>
          <span data-author="">{text.author(actor)}</span>
          <span data-label="">{actor === undefined ? "" : text.label(actor)}</span>
        </li>
      ))}
    </ul>
  );
}

/** What `useAgentText` spells for the session nothing names where the text names the session
 *  rather than says what is drawn: `sessionText`, and the title of the mention picker's option for
 *  that session listed untitled, the `@` text a sent message keeps. */
function SpelledProbe(): ReactNode {
  const text = useAgentText();
  const option = mentionOptions([listed(NAMELESS_SESSION, "")], text).find(
    (each) => each.target === `session:${NAMELESS_SESSION}`
  );
  return (
    <div data-case="spelled, nameless">
      <span data-session-text="">{text.sessionText(NAMELESS_SESSION, undefined)}</span>
      <span data-mention-title="">{option?.title}</span>
    </div>
  );
}

/** How `view` spells the session nothing names: `label`, `sessionText` and the mention option's
 *  title, as `AuthorProbe` and `SpelledProbe` draw them. */
function spelledNames(view: RenderResult): Record<string, string | null | undefined> {
  const spelled = caseOf(view, "spelled, nameless");
  return {
    label: caseOf(view, "author: nameless").querySelector("[data-label]")?.textContent,
    mentionTitle: spelled.querySelector("[data-mention-title]")?.textContent,
    sessionText: spelled.querySelector("[data-session-text]")?.textContent,
  };
}

/** The sentences built of the session nothing names, as `view` draws them, each up to its first
 *  comma: the Unsubscribe dialog's question, opened by its control, and each route marker's words
 *  for a screen reader (`sr-only`). */
async function namelessSentences(view: RenderResult): Promise<(string | undefined)[]> {
  const subscribed = within(caseOf(view, "subscribed, nameless"));
  await userEvent.setup().click(subscribed.getByRole("button", { name: "Unsubscribe" }));
  const dialog = await subscribed.findByRole("dialog", { name: "Unsubscribe" });
  return [
    dialog.querySelector("p"),
    ...caseOf(view, "routes, nameless").querySelectorAll(".sr-only"),
  ].map((element) => element?.textContent?.split(",")[0]);
}

// Inside `NoTooltips`, an author is named by a title - the agent list's, else the one its write
// was stamped with - else the page's word for an agent, and never by the short id that tells no
// two sessions apart; a person's act made through an agent names the agent by what it is. Each
// sentence a reader takes beside a drawn name there (the Unsubscribe dialog's question, a route
// marker's words for a screen reader) names a session as every form of `AgentName` does. Text
// that names a session rather than says what is drawn - `label`, `sessionText`, and the mention a
// sent message keeps - stays `main`'s on both surfaces, since the page's word names nobody.
// Outside, `author` names an actor as `label` does, and those sentences keep `main`'s short id.
test("author names a session by its title and never by its short id", async () => {
  // The page's words for an agent nothing names, for no actor, and for an act made through an
  // agent, as the Verification document reads them.
  expect([
    CONTROL_LABELS.unnamed_author,
    CONTROL_LABELS.unknown_author,
    `, acting through ${AUTHOR_KIND_WORDS.agent}`,
  ]).toEqual(["An agent", "Author unknown", ", acting through an agent"]);
  const throughAgent: Actor = { act: { session: LIVE_SESSION }, id: "alice", kind: "user" };
  const actors: readonly (readonly [string, Actor | undefined])[] = [
    ["listed", { id: LIVE_SESSION, kind: "session" }],
    ["stamped", { id: GONE_SESSION, kind: "session", origin: { session_title: "Old planner" } }],
    ["nameless", { id: NAMELESS_SESSION, kind: "session" }],
    ["through an agent", throughAgent],
    ["no actor", undefined],
  ];
  const node = (
    <>
      <div data-case="actor, listed">
        <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} author />
      </div>
      <div data-case="id, listed">
        <AgentName author sessionId={LIVE_SESSION} storedTitle={undefined} />
      </div>
      <div data-case="actor, stamped">
        <AgentName
          actor={{ id: GONE_SESSION, kind: "session", origin: { session_title: "Old planner" } }}
          author
        />
      </div>
      <div data-case="id, stamped">
        <AgentName author sessionId={GONE_SESSION} storedTitle="Old planner" />
      </div>
      <div data-case="actor, nameless">
        <AgentName actor={{ id: NAMELESS_SESSION, kind: "session" }} author />
      </div>
      <div data-case="id, nameless">
        <AgentName author sessionId={NAMELESS_SESSION} storedTitle={undefined} />
      </div>
      <div data-case="through an agent">
        <AgentName actor={throughAgent} author />
      </div>
      <div data-case="nameless, not an author">
        <AgentName actor={{ id: NAMELESS_SESSION, kind: "session" }} />
      </div>
      <div data-case="subscribed, nameless">
        <SubscribedAgents
          onUnsubscribe={() => undefined}
          ownerLabel="CORE-1"
          subscribers={[
            {
              last_seen: Date.now(),
              live: true,
              removable: true,
              session_id: NAMELESS_SESSION,
              title: "",
              topics: ["notifications.dispatch.issue.CORE-1.>"],
              via: "notifications.dispatch.issue.CORE-1.>",
            },
          ]}
        />
      </div>
      <div data-case="routes, nameless">
        <UnresponsiveRouteMarker
          issue={{
            route: "role:reviewer",
            route_holder: NAMELESS_SESSION,
            route_status: "holder_unresponsive",
            route_status_reason: null,
          }}
        />
        <UnreachableRouteMarker
          issue={{
            route: `session:${NAMELESS_SESSION}`,
            route_holder: null,
            route_status: "no_holder",
            route_status_reason: null,
          }}
        />
      </div>
      <AuthorProbe actors={actors} />
      <SpelledProbe />
    </>
  );

  const inside = draw(node, "inside");
  try {
    await waitFor(() =>
      expect(inside.container.querySelector(`[data-agent-state="running"]`)).not.toBeNull()
    );
    await settleReads();
    const named = (name: string) => caseOf(inside, name).textContent;
    const authored = (name: string) =>
      caseOf(inside, `author: ${name}`).querySelector("[data-author]")?.textContent;
    expect(named("actor, listed")).toBe("Implementer");
    expect(named("id, listed")).toBe("Implementer");
    expect(named("actor, stamped")).toBe("Old planner");
    expect(named("id, stamped")).toBe("Old planner");
    expect(named("actor, nameless")).toBe(CONTROL_LABELS.unnamed_author);
    expect(named("id, nameless")).toBe(CONTROL_LABELS.unnamed_author);
    expect(named("through an agent")).toBe(`alice, acting through ${AUTHOR_KIND_WORDS.agent}`);
    // Without `author` the same session reads the same: inside, every form of `AgentName` names
    // a session nothing names by the page's word.
    expect(named("nameless, not an author")).toBe(CONTROL_LABELS.unnamed_author);

    expect(authored("listed")).toBe("Implementer");
    expect(authored("stamped")).toBe("Old planner");
    expect(authored("nameless")).toBe(CONTROL_LABELS.unnamed_author);
    expect(authored("through an agent")).toBe(`alice, acting through ${AUTHOR_KIND_WORDS.agent}`);
    expect(authored("no actor")).toBe(CONTROL_LABELS.unknown_author);
    // Every sentence built of a session's drawn name says it as `AgentName` draws it, so the
    // Unsubscribe dialog and a route marker's words for a screen reader name no short id.
    expect(await namelessSentences(inside)).toEqual([
      `Unsubscribe ${CONTROL_LABELS.unnamed_author} from CORE-1? They will be told.`,
      `. ${CONTROL_LABELS.unnamed_author} has missed its heartbeats without ending`,
      `. ${CONTROL_LABELS.unnamed_author} is not running right now`,
    ]);
    for (const name of ["subscribed, nameless", "routes, nameless"]) {
      expect(caseOf(inside, name).textContent).not.toContain("session:");
    }
    // The text that names the session keeps `main`'s short id inside too: a mention composed
    // here stores `@session <id>`, never the page's word, which would name nobody.
    expect(spelledNames(inside)).toEqual({
      label: "session:01234567…",
      mentionTitle: "session:01234567…",
      sessionText: "session:01234567…",
    });
  } finally {
    undraw(inside);
  }

  const outside = draw(node, "outside");
  try {
    await waitFor(() =>
      expect(outside.container.querySelector(`[data-agent-state="running"]`)).not.toBeNull()
    );
    await settleReads();
    for (const [name, actor] of actors) {
      const row = caseOf(outside, `author: ${name}`);
      const author = row.querySelector("[data-author]")?.textContent;
      if (actor === undefined) {
        expect(author).toBe("");
      } else {
        expect(author).toBe(row.querySelector("[data-label]")?.textContent ?? "");
        expect(author).not.toBe("");
      }
    }
    // Outside, `main`'s short id names the session nothing names, in `label`, `sessionText`, the
    // mention's title and every sentence.
    expect(spelledNames(outside)).toEqual({
      label: "session:01234567…",
      mentionTitle: "session:01234567…",
      sessionText: "session:01234567…",
    });
    expect(await namelessSentences(outside)).toEqual([
      "Unsubscribe session:01234567… from CORE-1? They will be told.",
      ". session:01234567… has missed its heartbeats without ending",
      ". session:01234567… is not running right now",
    ]);
  } finally {
    undraw(outside);
  }
});

// The components line reads the architecture to name a component by its title only inside
// `NoTooltips`, in a row of Details; everywhere else an explicit set names its ids, as on `main`,
// and reads it only once the picker opens.
test("outside NoTooltips an explicit component set makes no architecture read", async () => {
  const reads = async (inRow: boolean, surface: Surface) => {
    stubs.getArchitecture.mockClear();
    const view = draw(
      <IssueComponentsLine
        inRow={inRow}
        issue={componentsIssue(explicitComponents(["dispatch-server"]))}
      />,
      surface
    );
    try {
      await settleReads();
      // The line is drawn, so a count of no reads is the line's and not an empty mount's.
      expect(within(view.container).getByTestId("issue-components")).toBeInTheDocument();
      return stubs.getArchitecture.mock.calls.length;
    } finally {
      undraw(view);
    }
  };

  expect(await reads(false, "outside")).toBe(0);
  expect(await reads(true, "outside")).toBe(0);
  expect(await reads(false, "inside")).toBe(0);
  expect(await reads(true, "inside")).toBe(1);
});

// At its defaults each shared control draws `main`'s classes. Each string below is `main`'s class,
// its theme's constants written out from `main`'s `theme/classes.ts`, for every class a shared
// control builds from a template of its own, and `null` where `main` draws the element with no
// class.
test("the default shapes keep main's class strings", async () => {
  const want: Record<string, string | null> = {
    "approvalPillClassName approved":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-emerald-700 dark:border-emerald-400 text-emerald-700 dark:text-emerald-400",
    "approvalPillClassName awaiting":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-amber-700 dark:border-amber-400 text-amber-800 dark:text-amber-300",
    "approvalPillClassName changes_requested":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-rose-700 dark:border-rose-400 text-rose-700 dark:text-rose-400",
    "approvalPillClassName draft":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-dashed border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300",
    "approvalPillClassName stale":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-amber-700 dark:border-amber-400 text-amber-800 dark:text-amber-300",
    badgeSelectWrapper:
      "relative inline-flex min-h-11 shrink-0 items-center rounded-sm md:min-h-8 has-focus-visible:ring-2 has-focus-visible:ring-sky-600 dark:has-focus-visible:ring-sky-400",

    "ApprovalSteps list's host": "flex w-full min-w-0 flex-col gap-2",
    "ApprovalSteps list": "flex w-full min-w-0 flex-wrap items-center gap-x-4 gap-y-2",
    // `main`'s features/issue/ApprovalSteps.tsx: a step's `<li>`.
    "ApprovalSteps entry": "flex min-w-0 flex-wrap items-center gap-2",
    "ApprovalSteps step name": "text-xs font-medium text-slate-500 dark:text-slate-400",
    "ApprovalSteps chip":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-amber-700 dark:border-amber-400 text-amber-800 dark:text-amber-300",
    "ApprovalSteps Not requested":
      "inline-flex min-w-0 items-center rounded-full border px-2 py-1 text-center text-xs font-medium [overflow-wrap:anywhere] border-dashed border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300",
    "ApprovalSteps Approve":
      "min-h-11 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 bg-sky-700 text-white not-refused:hover:bg-sky-800 refused:cursor-not-allowed refused:bg-slate-300 refused:text-slate-700 dark:refused:bg-slate-700 dark:refused:text-slate-300",
    "ApprovalSteps Request changes":
      "min-h-11 rounded-lg border px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400",
    "ApprovalSteps reason":
      "min-w-0 text-xs [overflow-wrap:anywhere] text-slate-700 dark:text-slate-300",

    // `main`'s features/doc/ApprovalChip.tsx: the Reviews dialog's backdrop, its box, its heading,
    // Close, the line it says while the reviews load, once their read failed and with none yet,
    // and a review with its reason.
    "ApprovalChip reviews backdrop": "fixed inset-0 z-40 bg-slate-950/40",
    "ApprovalChip reviews dialog":
      "pointer-events-auto w-full max-w-md space-y-3 rounded-lg border p-4 shadow-xl border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900",
    "ApprovalChip reviews heading": "text-sm font-semibold text-slate-700 dark:text-slate-300",
    "ApprovalChip reviews Close": "text-sm font-medium text-slate-500 dark:text-slate-400",
    "ApprovalChip reviews loading": "text-sm text-slate-500 dark:text-slate-400",
    "ApprovalChip reviews unread": "text-sm text-rose-700 dark:text-rose-400",
    "ApprovalChip no reviews": "text-sm text-slate-500 dark:text-slate-400",
    "ApprovalChip review": "text-sm font-medium text-slate-900 dark:text-slate-100",
    "ApprovalChip review's reason": "mt-1 text-sm text-slate-700 dark:text-slate-300",
    // `main`'s features/doc/ApprovalChip.tsx: the Request changes dialog's backdrop, its box, its
    // heading, its label, the reason box, a refused request's line, Cancel and its Request changes.
    "ApprovalChip request backdrop": "fixed inset-0 z-40 bg-slate-950/40",
    "ApprovalChip request dialog":
      "pointer-events-auto w-full max-w-sm space-y-3 rounded-lg border p-4 shadow-xl border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900",
    "ApprovalChip request heading": "text-sm font-semibold text-slate-700 dark:text-slate-300",
    "ApprovalChip request label": "block text-sm font-medium text-slate-700 dark:text-slate-300",
    "ApprovalChip request reason box":
      "mt-1 block w-full rounded-lg border px-2 py-1.5 text-sm font-normal outline-none border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-900 dark:focus:border-sky-400",
    "ApprovalChip request refused": "text-sm text-rose-700 dark:text-rose-400",
    "ApprovalChip request Cancel":
      "rounded-lg border px-3 py-1.5 text-sm font-medium border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400",
    "ApprovalChip request Request changes":
      "rounded-lg px-3 py-1.5 text-sm font-medium bg-sky-700 text-white not-refused:hover:bg-sky-800 refused:cursor-not-allowed refused:bg-slate-300 refused:text-slate-700 dark:refused:bg-slate-700 dark:refused:text-slate-300",

    "AssigneeControl wrapper":
      "relative inline-flex min-h-11 shrink-0 items-center rounded-sm md:min-h-8 has-focus-visible:ring-2 has-focus-visible:ring-sky-600 dark:has-focus-visible:ring-sky-400 ",
    "AssigneeControl badge, assigned":
      "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold bg-sky-100 dark:bg-sky-950 text-sky-800 dark:text-sky-300",
    "AssigneeControl badge, unassigned":
      "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",
    "PriorityControl wrapper":
      "relative inline-flex min-h-11 shrink-0 items-center rounded-sm md:min-h-8 has-focus-visible:ring-2 has-focus-visible:ring-sky-600 dark:has-focus-visible:ring-sky-400 ",
    "PriorityControl badge, P0":
      "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold bg-rose-100 dark:bg-rose-950 text-rose-800 dark:text-rose-300",
    "PriorityControl badge, P1":
      "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold bg-amber-100 dark:bg-amber-950 text-amber-800 dark:text-amber-300",
    "PriorityControl badge, none":
      "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",

    "ClaimChip, held":
      "inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 min-w-0 max-w-full gap-1",
    "ClaimChip holder": "min-w-0 [overflow-wrap:anywhere] min-w-0 truncate",
    "ClaimChip dot": "shrink-0",
    "ClaimChip age": "shrink-0",
    "ClaimChip, lapsed":
      "inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-100 dark:bg-slate-800 text-slate-600 dark:text-slate-400 min-w-0 max-w-full gap-1",
    "ClaimChip not running": "shrink-0",
    // `main`'s features/issue/ClaimChip.tsx: the dot before a lapsed claim's "not running", and
    // an unresponsive holder's dot and words.
    "ClaimChip lapsed dot": "shrink-0",
    "ClaimChip unresponsive dot": "shrink-0",
    "ClaimChip unresponsive": "shrink-0",

    // `main`'s features/issue/RepoControl.tsx: the line, its label, the box's wrapper, the box
    // holding the issue's own repository and the box of an inherited one, the clear control, the
    // box's description and the refusal of a value that is not owner/name.
    "RepoControl line":
      "flex shrink-0 items-center gap-2 text-sm text-slate-700 dark:text-slate-300",
    "RepoControl label": "font-medium",
    "RepoControl wrapper": "relative inline-flex shrink-0 items-center",
    "RepoControl box, its own":
      "min-h-11 w-[200px] rounded-sm border py-0.5 pl-1.5 font-mono text-xs outline-none focus-visible:ring-2 disabled:opacity-50 md:min-h-7 pr-8 border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-900 dark:focus:border-sky-400 focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400",
    "RepoControl box, inherited":
      "min-h-11 w-[200px] rounded-sm border py-0.5 pl-1.5 font-mono text-xs outline-none focus-visible:ring-2 disabled:opacity-50 md:min-h-7 pr-1.5 border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-900 dark:focus:border-sky-400 focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400",
    "RepoControl clear":
      "absolute inset-y-0 right-0 inline-flex w-8 items-center justify-center rounded-sm text-base outline-none focus-visible:ring-2 disabled:opacity-50 text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100 focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400",
    "RepoControl description": "sr-only",
    "RepoControl refusal": "text-rose-700 dark:text-rose-400",

    "IssueComponentsLine line":
      "flex shrink-0 items-center gap-2 text-sm text-slate-700 dark:text-slate-300",
    "IssueComponentsLine chip":
      "inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium bg-sky-100 dark:bg-sky-950 text-sky-800 dark:text-sky-300",
    "IssueComponentsLine retired chip":
      "inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium bg-amber-100 dark:bg-amber-950 text-amber-800 dark:text-amber-300",
    "IssueComponentsLine inherited from": "shrink-0 text-xs text-slate-500 dark:text-slate-400",
    "IssueComponentsLine inherited link":
      "dispatch-inline-link underline text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "IssueComponentsLine not attached": "text-slate-500 dark:text-slate-400",
    // `main`'s features/issue/IssueComponentsLine.tsx: a none attachment with its reason, and a
    // refused save's line.
    "IssueComponentsLine none": "text-slate-500 dark:text-slate-400",
    "IssueComponentsLine refusal": "shrink-0 text-xs text-rose-700 dark:text-rose-400",

    "IssueLabels line": "relative mt-2 flex flex-wrap items-center gap-2",
    "IssueLabels chip":
      "inline-flex min-h-11 shrink-0 items-center rounded-full px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300",
    "IssueLabels trigger":
      "min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400 refused:text-slate-400 dark:refused:text-slate-600",
    // `main`'s features/issue/IssueLabels.tsx: the line at `variant="rail"`.
    "IssueLabels rail line": "relative flex shrink-0 flex-nowrap items-center gap-1",

    "IssuePriorityLine line":
      "flex shrink-0 items-center gap-2 text-sm text-slate-700 dark:text-slate-300",
    "IssuePriorityLine entry": "flex shrink-0 items-center gap-1",
    "IssuePriorityLine link":
      "dispatch-inline-link shrink-0 whitespace-nowrap underline text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "IssuePriorityLine inherited": "shrink-0 text-xs text-slate-500 dark:text-slate-400",
    "IssuePriorityLine remove":
      "shrink-0 text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100",
    "IssuePriorityLine add":
      "inline-flex min-h-11 shrink-0 items-center rounded-full px-2 py-2 text-sm whitespace-nowrap outline-none bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100",
    "IssuePriorityLine form": "flex shrink-0 items-center gap-2",
    "IssuePriorityLine picker":
      "min-h-11 max-w-56 min-w-0 shrink-0 truncate rounded-lg px-2 py-1 text-sm md:min-h-8 border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-950 dark:focus:border-sky-400",
    "IssuePriorityLine Add":
      "min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 refused:text-slate-400 dark:refused:text-slate-600",
    "IssuePriorityLine Cancel":
      "min-h-11 rounded px-2 py-1 text-sm font-medium md:min-h-8 text-slate-500 dark:text-slate-400 hover:text-slate-700 dark:hover:text-slate-300",
    // `main`'s features/priorities/IssuePriorityLine.tsx: the line at the cap, the refusal of the
    // priorities' read and that of a save.
    "IssuePriorityLine at the cap": "shrink-0 text-xs text-slate-500 dark:text-slate-400",
    "IssuePriorityLine priorities unread": "shrink-0 text-xs text-rose-700 dark:text-rose-400",
    "IssuePriorityLine not saved": "shrink-0 text-xs text-rose-700 dark:text-rose-400",

    "AgentName link":
      "rounded outline-none focus-visible:ring-2 focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400 text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "AgentName inline link":
      "rounded outline-none focus-visible:ring-2 focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400 text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300 dispatch-inline-link underline",
    "AgentName live link":
      "ml-1 px-1 rounded outline-none focus-visible:ring-2 focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400 text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "AgentName mark, running":
      "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle bg-emerald-500 dark:bg-emerald-400",
    "AgentName mark, unresponsive":
      "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle border-2 border-solid border-amber-500 dark:border-amber-400",
    "AgentName mark, not running":
      "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle border-2 border-solid border-slate-400 dark:border-slate-600",
    "AgentName mark, unknown":
      "mr-1 inline-block h-2 w-2 shrink-0 rounded-full align-middle border border-dashed border-slate-400 dark:border-slate-500",
    AgentUnresponsiveNote: "shrink-0 text-xs text-amber-800 dark:text-amber-300",

    RouteReachUnknownMarker: "text-xs text-slate-500 dark:text-slate-400",
    UnresponsiveRouteMarker:
      "inline-flex min-w-0 max-w-full items-center gap-1 text-xs text-slate-500 dark:text-slate-400",
    UnreachableRouteMarker:
      "inline-flex min-w-0 max-w-full items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-semibold border-amber-200 dark:border-amber-900 bg-amber-50 dark:bg-amber-950 text-amber-950 dark:text-amber-300",

    "GitHubLink link":
      "inline-flex max-w-full items-center gap-2 text-sm underline text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "GitHubLink state":
      "shrink-0 rounded-full px-2 py-0.5 text-xs bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",
    // `main`'s features/issue/GitHubLink.tsx: a pull request's state chip, merged, and its
    // checks chip.
    "GitHubLink state, merged":
      "shrink-0 rounded-full px-2 py-0.5 text-xs bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",
    "GitHubLink checks":
      "shrink-0 rounded-full px-2 py-0.5 text-xs bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",

    "SubscribedAgents heading": "text-sm font-semibold text-slate-700 dark:text-slate-300",

    Pill: "inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-100 dark:bg-slate-800 text-slate-600 dark:text-slate-400 ",
    "LabelPill, selected":
      "inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-200 dark:bg-slate-700 text-slate-700 dark:text-slate-300 ",
    StatusPill:
      "inline-flex shrink-0 items-center rounded-full px-2 py-1 text-xs font-medium whitespace-nowrap bg-slate-100 dark:bg-slate-800 text-slate-800 dark:text-slate-200 gap-1.5 font-semibold tracking-wide uppercase",
    "StatusPill dot": "size-1.5 rounded-full bg-slate-500 dark:bg-slate-400",

    "CopyButton, icon":
      "inline-flex min-h-11 shrink-0 items-center rounded-lg md:min-h-8 min-w-11 justify-center md:min-w-8 text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300 ",
    "CopyButton, labelled":
      "inline-flex min-h-11 shrink-0 items-center rounded-lg md:min-h-8 max-w-full gap-1 px-1 font-mono text-xs font-medium select-text text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300 ",
    "CopyButton copied": "ml-1 text-xs font-medium text-emerald-700 dark:text-emerald-400",
    "CopyButton failed": "ml-1 text-xs font-medium text-rose-700 dark:text-rose-400",
    // `main`'s components/CopyButton.tsx: the `<code>` a failed copy prints the value in.
    "CopyButton failed value": "select-all font-mono",

    "PinButton, pinned":
      "flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg disabled:cursor-not-allowed disabled:opacity-50 md:min-h-8 md:min-w-8 bg-sky-700 text-white not-refused:hover:bg-sky-800 ",
    "PinButton, unpinned":
      "flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg disabled:cursor-not-allowed disabled:opacity-50 md:min-h-8 md:min-w-8 border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400 ",
    "PinButton, quiet":
      "flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg disabled:cursor-not-allowed disabled:opacity-50  text-slate-500 dark:text-slate-400 hover:text-slate-700 dark:hover:text-slate-300 ",

    "RefusableButton primary":
      "rounded-lg px-3 py-2 text-sm font-semibold bg-sky-700 text-white not-refused:hover:bg-sky-800 refused:cursor-not-allowed refused:bg-slate-300 refused:text-slate-700 dark:refused:bg-slate-700 dark:refused:text-slate-300 ",
    "RefusableButton secondary-compact":
      "min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400 refused:text-slate-400 dark:refused:text-slate-600 ",

    "ConnectionDot connected": "h-2.5 w-2.5 rounded-full bg-emerald-500 dark:bg-emerald-400",
    "ConnectionDot connecting": "h-2.5 w-2.5 rounded-full bg-amber-500 dark:bg-amber-400",
    "ConnectionDot failed": "h-2.5 w-2.5 rounded-full bg-rose-500 dark:bg-rose-400",
    "ConnectionDot offline": "h-2.5 w-2.5 rounded-full bg-slate-400 dark:bg-slate-500",
    "ConnectionDot notice": "text-xs text-slate-500 dark:text-slate-400",

    "MultiSelect trigger":
      "min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold border-slate-200 dark:border-slate-800 bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",
    "MultiSelect panel":
      "z-50 max-h-72 w-64 max-w-[calc(100vw-1rem)] overflow-auto rounded-lg border shadow-lg border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900 border-slate-200 dark:border-slate-800",
    "MultiSelect search's host":
      "sticky top-0 border-b p-2 border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900",
    "MultiSelect search":
      "block min-h-11 w-full rounded-lg px-3 py-2 text-sm outline-none border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-950 dark:focus:border-sky-400",
    "MultiSelect row, active":
      "flex min-h-11 w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-b-0 border-slate-200 dark:border-slate-800 text-slate-700 dark:text-slate-300 hover:border-sky-400 dark:hover:border-sky-300 bg-sky-50 dark:bg-sky-950 border-sky-500 dark:border-sky-400",
    "MultiSelect row":
      "flex min-h-11 w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-b-0 border-slate-200 dark:border-slate-800 text-slate-700 dark:text-slate-300 hover:border-sky-400 dark:hover:border-sky-300 ",
    "MultiSelect checkbox":
      "grid size-4 shrink-0 place-items-center rounded border text-xs leading-none border-slate-300 dark:border-slate-700",
    "MultiSelect option label": "truncate min-w-0 flex-1",
    "MultiSelect option detail": "shrink-0 text-xs tabular-nums text-slate-500 dark:text-slate-400",
    "MultiSelect empty": "px-3 py-2 text-sm text-slate-500 dark:text-slate-400",
    "MultiSelect refusal":
      "border-t px-3 py-2 text-sm border-slate-200 dark:border-slate-800 text-rose-700 dark:text-rose-400",
    "MultiSelect clear's host":
      "sticky bottom-0 border-t p-2 border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900",
    "MultiSelect clear":
      "min-h-11 w-full rounded-lg px-3 py-2 text-sm font-medium bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",

    // `main`'s features/doc/ProofDocument.tsx: the open-decisions `<nav>`.
    "ProofDocument open decisions navigator": "flex min-h-11 items-center gap-2 md:h-7 md:min-h-0",
    "ProofDocument open decisions":
      "inline-flex h-6 shrink-0 items-center rounded-full px-2 text-xs font-semibold bg-sky-100 dark:bg-sky-950 text-sky-800 dark:text-sky-300",
    "ProofDocument open decision link":
      "block min-w-0 max-w-[90ch] truncate text-sm font-medium underline text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "ProofDocument Next open decision":
      "ml-auto flex min-h-11 min-w-11 items-center justify-center rounded-full md:min-h-7 md:min-w-7 border-slate-300 dark:border-slate-700 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400 text-slate-700 dark:text-slate-300",
    "ProofDocument block references":
      "pointer-events-auto absolute right-0 min-h-11 min-w-11 rounded-full text-xs font-semibold bg-sky-100 dark:bg-sky-950 text-sky-800 dark:text-sky-300",
    // `main`'s features/doc/ProofDocument.tsx: the line naming what changed since the reader's
    // last read.
    "ProofDocument read line": "text-sm text-slate-700 dark:text-slate-300",
    "ProofDocument repair refusal": "text-rose-700 dark:text-rose-400",
    "ProofDocument rebuild refusal": "text-rose-700 dark:text-rose-400",
    "ProofDocument Rebuild":
      "min-h-11 rounded-lg border px-3 py-2 text-sm font-medium border-slate-300 dark:border-slate-700 text-slate-700 dark:text-slate-300 not-refused:hover:border-sky-500 dark:not-refused:hover:border-sky-400",
    "ProofDocument rebuild failure": "text-rose-700 dark:text-rose-400",
    "ProofDocument load failure": "text-rose-700 dark:text-rose-400",

    "AskCard card":
      "relative rounded-xl border-l-4 shadow-sm px-4 pt-5 pb-4 mt-3 border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900 border-l-rose-500 dark:border-l-rose-400",
    "AskCard urgency notch":
      "absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase bg-white dark:bg-slate-900 text-rose-800 dark:text-rose-300",
    "AskCard question":
      "text-[15px] leading-relaxed font-medium text-slate-900 dark:text-slate-100",
    "AskCard asker line":
      "@container mt-1.5 flex flex-wrap items-center gap-x-1.5 text-xs text-slate-500 dark:text-slate-400",
    // `main`'s features/inbox/AskCard.tsx: the group of the turn and the asker.
    "AskCard turn and asker":
      "flex min-w-0 items-center gap-x-1.5 whitespace-nowrap @max-sm:flex-wrap",
    "AskCard turn": "shrink-0 font-semibold @max-sm:basis-full text-slate-700 dark:text-slate-300",
    // `main`'s features/inbox/AskCard.tsx: the turn an agent holds.
    "AskCard turn, an agent's":
      "min-w-0 truncate font-semibold @max-sm:basis-full text-slate-700 dark:text-slate-300",
    "AskCard asker":
      "flex min-w-0 items-center gap-1 text-slate-500 dark:text-slate-400 hover:text-slate-700 dark:hover:text-slate-300",
    // `main`'s features/inbox/AskCard.tsx: the asker's `AgentName`, its class after `main`'s
    // `AGENT_SPAN_CLASS` (features/refs/AgentName.tsx).
    "AskCard asker's name":
      "min-w-0 [overflow-wrap:anywhere] flex min-w-0 items-center whitespace-pre",
    "AskCard hand-on line":
      "mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-slate-500 dark:text-slate-400",
    "AskCard hand-on badge":
      "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold bg-slate-100 dark:bg-slate-800 text-slate-700 dark:text-slate-300",
    // `main`'s features/inbox/AskCard.tsx: the span that says who handed the question on, which
    // has no class.
    "AskCard handed on": null,
    "AskCard answer label": "sr-only",
    "AskCard answer box":
      "block w-full rounded-lg px-3 py-2 font-normal outline-none border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-950 dark:focus:border-sky-400",
    "AskCard Answer":
      "min-h-11 rounded-lg px-3 py-2 text-sm font-semibold bg-sky-700 text-white not-refused:hover:bg-sky-800 refused:cursor-not-allowed refused:bg-slate-300 refused:text-slate-700 dark:refused:bg-slate-700 dark:refused:text-slate-300",
    "AskCard Ask back":
      "min-h-11 rounded-lg border px-3 py-2 text-sm font-medium border-slate-200 dark:border-slate-800 text-slate-700 dark:text-slate-300 hover:border-sky-400 dark:hover:border-sky-300",
    "AskCard Ask back instead":
      "min-h-11 rounded-lg px-3 py-2 text-sm font-semibold bg-sky-700 text-white not-refused:hover:bg-sky-800 refused:cursor-not-allowed refused:bg-slate-300 refused:text-slate-700 dark:refused:bg-slate-700 dark:refused:text-slate-300",
    "AskCard Answer with it anyway":
      "min-h-11 rounded-lg border px-3 py-2 text-sm font-medium border-slate-200 dark:border-slate-800 text-slate-700 dark:text-slate-300 hover:border-sky-400 dark:hover:border-sky-300",
    "AskCard compact card":
      "relative rounded-xl border-l-4 shadow-sm px-3 pt-4 pb-3 mt-3 border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900 border-l-amber-500 dark:border-l-amber-400",
    "AskCard compact urgency notch":
      "absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase bg-white dark:bg-slate-900 text-amber-800 dark:text-amber-300",
    "AskCard approval requested":
      "text-[10px] font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400",
    "AskCard approval document":
      "mt-1 text-sm text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "AskCard own words": "min-h-11 text-sm font-medium text-slate-700 dark:text-slate-300",
    "AskCard reason label": "mb-1 block text-sm font-medium text-slate-700 dark:text-slate-300",
    "AskCard submit hint": "text-xs text-slate-500 dark:text-slate-400",
    // `main`'s features/inbox/UnsentText.tsx, as AskCard draws it: the unsent answer's box, its
    // note and its words.
    "AskCard unsent answer": "rounded-xl border p-3 border-slate-200 dark:border-slate-800 mt-3",
    "AskCard unsent answer note": "text-xs text-slate-500 dark:text-slate-400",
    "AskCard unsent answer words":
      "mt-1 text-sm whitespace-pre-wrap text-slate-900 dark:text-slate-100",
    // `main`'s features/inbox/AskCard.tsx: a decision question's "Open in document" line, the
    // warning that the question changed under the reader, and Cancel while an answer is changed.
    "AskCard block link":
      "mt-2 text-sm text-sky-700 dark:text-sky-400 hover:text-sky-900 dark:hover:text-sky-300",
    "AskCard question changed": "mt-3 text-sm font-medium text-amber-800 dark:text-amber-300",
    "AskCard Cancel, changing an answer":
      "min-h-11 rounded-lg border px-3 py-2 text-sm font-medium border-slate-200 dark:border-slate-800 text-slate-700 dark:text-slate-300 hover:border-sky-400 dark:hover:border-sky-300",
  };

  const read: Record<string, string | null> = {};
  const classAt = (name: string, element: Element | null | undefined) => {
    read[name] = element?.getAttribute("class") ?? null;
  };
  for (const state of ["approved", "awaiting", "changes_requested", "draft", "stale"]) {
    read[`approvalPillClassName ${state}`] = approvalPillClassName(state);
  }
  read.badgeSelectWrapper = badgeSelectWrapper;

  const user = userEvent.setup();
  const view = draw(
    <>
      <div data-case="approval steps">
        <ApprovalSteps
          entries={[
            specEntry({ latest_version: 1, state: "awaiting", waiting_on: "human" }),
            { document: undefined, label: "Verification", step: "criteria" },
          ]}
          primaryArtifactId={SPEC_ID}
        />
      </div>
      {/* Neither of the gate's two steps above was sent back, so a step sent back, which draws
          its reason under the list, is drawn in a list of its own. */}
      <div data-case="approval reason">
        <ApprovalSteps
          entries={[
            verificationEntry({
              by: { id: "bob", kind: "user" },
              latest_version: 3,
              reason: "The checks name no command.",
              state: "changes_requested",
              version: 3,
            }),
          ]}
          primaryArtifactId={SPEC_ID}
        />
      </div>
      <div data-case="assignee">
        <AssigneeControl assignee="alice@example.com" issueKey="CORE-1" />
      </div>
      <div data-case="unassigned">
        <AssigneeControl assignee={null} issueKey="CORE-2" />
      </div>
      <div data-case="P0">
        <PriorityControl issueKey="CORE-1" priority={0} />
      </div>
      <div data-case="P1">
        <PriorityEditor issueKey="CORE-2" priority={1} write={idlePriorityWrite} />
      </div>
      <div data-case="no priority">
        <PriorityControl issueKey="CORE-3" priority={null} />
      </div>
      <div data-case="held">
        <ClaimChip claim={sessionClaim(LIVE_SESSION)} />
      </div>
      <div data-case="lapsed">
        <ClaimChip claim={sessionClaim(GONE_SESSION)} />
      </div>
      <div data-case="unresponsive claim">
        <ClaimChip claim={sessionClaim(STALLED_SESSION)} liveness={waitingLiveness()} />
      </div>
      <div data-case="repository">
        <RepoControl issueKey="CORE-1" project="CORE" repo="acme/api" resolvedRepo="acme/api" />
      </div>
      <div data-case="inherited repository">
        <RepoControl issueKey="CORE-2" project="CORE" repo={null} resolvedRepo="acme/web" />
      </div>
      <div data-case="components">
        <IssueComponentsLine
          issue={componentsIssue({
            ids: ["dispatch-server"],
            inherited_from: "CORE-9",
            mode: "explicit",
            reason: null,
            unknown: ["old-queue"],
          })}
        />
      </div>
      <div data-case="not attached">
        <IssueComponentsLine
          issue={componentsIssue(
            { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
            "CORE-2"
          )}
        />
      </div>
      <div data-case="labels">
        <IssueLabels
          disabled={false}
          labels={["api"]}
          onSave={async () => undefined}
          project="CORE"
          saving={false}
        />
      </div>
      <div data-case="rail labels">
        <IssueLabels
          disabled={false}
          labels={["api"]}
          onSave={async () => undefined}
          project="CORE"
          saving={false}
          variant="rail"
        />
      </div>
      <div data-case="priorities">
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
      </div>
      <div data-case="running">
        <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} />
      </div>
      <div data-case="inline">
        <AgentName actor={{ id: LIVE_SESSION, kind: "session" }} inline />
      </div>
      <div data-case="control">
        <AgentName
          actor={{ id: LIVE_SESSION, kind: "session" }}
          control={(name) => <button type="button">{name}</button>}
          kind="control"
        />
      </div>
      <div data-case="unresponsive">
        <AgentName actor={{ id: STALLED_SESSION, kind: "session" }} />
      </div>
      <div data-case="not running">
        <AgentName actor={{ id: GONE_SESSION, kind: "session" }} />
      </div>
      <div data-case="unresponsive note">
        <AgentUnresponsiveNote lastSeen={Date.now() - 10 * 60_000} state="unresponsive" />
      </div>
      <div data-case="route">
        <RouteReachUnknownMarker testId="route-unknown" />
        <UnresponsiveRouteMarker
          issue={{
            route: "role:reviewer",
            route_holder: STALLED_SESSION,
            route_status: "holder_unresponsive",
            route_status_reason: null,
          }}
        />
        <UnreachableRouteMarker
          issue={{
            route: "role:architect",
            route_holder: null,
            route_status: "no_holder",
            route_status_reason: null,
          }}
        />
      </div>
      <div data-case="github">
        <GitHubLink link={{ url: "https://github.com/owner/repository/issues/7" }} />
      </div>
      <div data-case="github pull request">
        <GitHubLink link={{ url: "https://github.com/owner/repository/pull/8" }} />
      </div>
      <div data-case="subscribers">
        <SubscribedAgents
          onUnsubscribe={() => undefined}
          ownerLabel="CORE-1"
          subscribers={[
            {
              last_seen: Date.now(),
              live: true,
              removable: true,
              session_id: LIVE_SESSION,
              title: "Implementer",
              topics: ["notifications.dispatch.issue.CORE-1.>"],
              via: "notifications.dispatch.issue.CORE-1.>",
            },
          ]}
        />
      </div>
      <div data-case="pills">
        <Pill>label</Pill>
        <LabelPill selected>selected</LabelPill>
        <StatusPill>in progress</StatusPill>
      </div>
    </>,
    "outside"
  );
  stubs.listAgents.mockRejectedValueOnce(new Error("Envoy listener unreachable"));
  const unknown = draw(<AgentName actor={{ id: LIVE_SESSION, kind: "session" }} />, "outside");
  try {
    const scope = within(view.container);
    await scope.findByRole("link", { name: /Rocq and Lean/ });
    await scope.findByText("open");
    await within(caseOf(view, "github pull request")).findByText("checks: success");
    await waitFor(() =>
      expect(view.container.querySelector('[data-agent-state="unresponsive"]')).not.toBeNull()
    );
    await waitFor(() =>
      expect(unknown.container.querySelector('[data-agent-state="unknown"]')).not.toBeNull()
    );
    await settleReads();

    const steps = within(caseOf(view, "approval steps"));
    const list = steps.getByRole("list", { name: "Approvals" });
    classAt("ApprovalSteps list's host", list.parentElement);
    classAt("ApprovalSteps list", list);
    classAt(
      "ApprovalSteps entry",
      caseOf(view, "approval steps").querySelector('[data-approval-step="spec"]')
    );
    classAt(
      "ApprovalSteps step name",
      caseOf(view, "approval steps").querySelector('[data-approval-step="spec"] > span')
    );
    classAt("ApprovalSteps chip", steps.getByRole("button", { name: "Awaiting approval" }));
    classAt("ApprovalSteps Not requested", steps.getByText("Not requested"));
    classAt("ApprovalSteps Approve", steps.getByRole("button", { name: "Approve" }));
    classAt(
      "ApprovalSteps Request changes",
      steps.getByRole("button", { name: "Request changes" })
    );
    classAt(
      "ApprovalSteps reason",
      caseOf(view, "approval reason").querySelector('[data-approval-step-reason="criteria"]')
    );

    const assignee = within(caseOf(view, "assignee")).getByRole("combobox");
    classAt("AssigneeControl wrapper", assignee.parentElement);
    classAt("AssigneeControl badge, assigned", assignee.parentElement?.querySelector("span"));
    classAt(
      "AssigneeControl badge, unassigned",
      caseOf(view, "unassigned").querySelector("span[aria-hidden='true']")
    );
    const p0 = within(caseOf(view, "P0")).getByRole("combobox");
    classAt("PriorityControl wrapper", p0.parentElement);
    classAt("PriorityControl badge, P0", p0.parentElement?.querySelector("span"));
    classAt(
      "PriorityControl badge, P1",
      caseOf(view, "P1").querySelector("span[aria-hidden='true']")
    );
    classAt(
      "PriorityControl badge, none",
      caseOf(view, "no priority").querySelector("span[aria-hidden='true']")
    );

    const held = within(caseOf(view, "held")).getByTestId("issue-claim");
    classAt("ClaimChip, held", held);
    classAt("ClaimChip holder", held.querySelector("[data-agent-name]"));
    classAt("ClaimChip dot", held.querySelector(":scope > span[aria-hidden='true']"));
    classAt("ClaimChip age", held.querySelector(":scope > time"));
    const lapsed = within(caseOf(view, "lapsed")).getByTestId("issue-claim");
    classAt("ClaimChip, lapsed", lapsed);
    classAt(
      "ClaimChip lapsed dot",
      lapsed.querySelectorAll(":scope > span[aria-hidden='true']")[1]
    );
    classAt(
      "ClaimChip not running",
      within(caseOf(view, "lapsed")).getByTestId("issue-claim-lapsed")
    );
    const unresponsive = within(caseOf(view, "unresponsive claim")).getByTestId("issue-claim");
    classAt(
      "ClaimChip unresponsive dot",
      unresponsive.querySelectorAll(":scope > span[aria-hidden='true']")[1]
    );
    classAt("ClaimChip unresponsive", within(unresponsive).getByTestId("issue-claim-unresponsive"));

    const ownRepository = within(caseOf(view, "repository"));
    const repository = ownRepository.getByRole("combobox", { name: "Repository of CORE-1" });
    classAt("RepoControl line", repository.parentElement?.parentElement);
    classAt("RepoControl label", ownRepository.getByText("Repository:"));
    classAt("RepoControl wrapper", repository.parentElement);
    classAt("RepoControl box, its own", repository);
    classAt(
      "RepoControl clear",
      ownRepository.getByRole("button", { name: "Clear the repository of CORE-1" })
    );
    classAt(
      "RepoControl description",
      document.getElementById(repository.getAttribute("aria-describedby") ?? "")
    );
    const inheritedRepository = within(caseOf(view, "inherited repository"));
    const inherited = inheritedRepository.getByRole("combobox", { name: "Repository of CORE-2" });
    classAt("RepoControl box, inherited", inherited);
    // A value that is not owner/name, written with Enter, draws the refusal; the focus starts the
    // mappings' read, which lands before the next read of the mount.
    await user.type(inherited, "nope{Enter}");
    await settleReads();
    classAt("RepoControl refusal", inheritedRepository.getByRole("alert"));

    const components = within(caseOf(view, "components"));
    classAt("IssueComponentsLine line", components.getByTestId("issue-components"));
    classAt("IssueComponentsLine chip", components.getByText("dispatch-server"));
    classAt("IssueComponentsLine retired chip", components.getByText("retired: old-queue"));
    const inheritedLink = components.getByRole("link", { name: "CORE-9" });
    classAt("IssueComponentsLine inherited from", inheritedLink.parentElement);
    classAt("IssueComponentsLine inherited link", inheritedLink);
    classAt(
      "IssueComponentsLine not attached",
      within(caseOf(view, "not attached")).getByText("Not attached")
    );

    const labels = within(caseOf(view, "labels"));
    classAt("IssueLabels line", labels.getByTestId("issue-labels"));
    classAt("IssueLabels chip", labels.getByText("api"));
    classAt("IssueLabels trigger", labels.getByRole("button", { name: "Edit labels" }));
    classAt(
      "IssueLabels rail line",
      within(caseOf(view, "rail labels")).getByTestId("issue-labels")
    );

    const advances = within(caseOf(view, "priorities"));
    const priorityLink = advances.getByRole("link", { name: /Rocq and Lean/ });
    classAt("IssuePriorityLine line", advances.getByTestId("issue-advances"));
    classAt("IssuePriorityLine entry", priorityLink.parentElement);
    classAt("IssuePriorityLine link", priorityLink);
    classAt("IssuePriorityLine inherited", advances.getByText("inherited"));
    classAt("IssuePriorityLine remove", advances.getByRole("button", { name: /^Remove / }));
    const add = advances.getByRole("button", { name: "Add a priority to CORE-1" });
    classAt("IssuePriorityLine add", add);
    await user.click(add);
    const picker = await advances.findByRole("combobox", { name: "Priority to advance" });
    classAt("IssuePriorityLine form", picker.closest("form"));
    classAt("IssuePriorityLine picker", picker);
    classAt("IssuePriorityLine Add", advances.getByRole("button", { name: "Add" }));
    classAt("IssuePriorityLine Cancel", advances.getByRole("button", { name: "Cancel" }));

    classAt("AgentName link", caseOf(view, "running").querySelector("a"));
    classAt("AgentName inline link", caseOf(view, "inline").querySelector("a"));
    classAt(
      "AgentName live link",
      within(caseOf(view, "control")).getByRole("link", { name: /^Open live conversation/ })
    );
    classAt("AgentName mark, running", caseOf(view, "running").querySelector("[role='img']"));
    classAt(
      "AgentName mark, unresponsive",
      caseOf(view, "unresponsive").querySelector("[role='img']")
    );
    classAt(
      "AgentName mark, not running",
      caseOf(view, "not running").querySelector("[role='img']")
    );
    classAt("AgentName mark, unknown", unknown.container.querySelector("[role='img']"));
    classAt(
      "AgentUnresponsiveNote",
      within(caseOf(view, "unresponsive note")).getByTestId("agent-unresponsive")
    );

    const route = within(caseOf(view, "route"));
    classAt("RouteReachUnknownMarker", route.getByTestId("route-unknown"));
    classAt("UnresponsiveRouteMarker", route.getByTestId("issue-route-unresponsive"));
    classAt("UnreachableRouteMarker", route.getByTestId("issue-route-unreachable"));

    const github = within(caseOf(view, "github"));
    classAt("GitHubLink link", github.getByRole("link"));
    classAt("GitHubLink state", github.getByText("open"));
    const pullRequest = within(caseOf(view, "github pull request"));
    classAt("GitHubLink state, merged", pullRequest.getByText("merged"));
    classAt("GitHubLink checks", pullRequest.getByText("checks: success"));

    classAt(
      "SubscribedAgents heading",
      within(caseOf(view, "subscribers")).getByRole("heading", { name: "Subscribed agents" })
    );

    const pills = within(caseOf(view, "pills"));
    classAt("Pill", pills.getByText("label"));
    classAt("LabelPill, selected", pills.getByText("selected"));
    const status = pills.getByText("in progress");
    classAt("StatusPill", status);
    classAt("StatusPill dot", status.querySelector("span[aria-hidden='true']"));
  } finally {
    undraw(view);
    undraw(unknown);
  }

  // The components' own controls, each at its defaults: a copy that lands, one that fails with
  // its value already in its words and one that fails and prints its value, each pin, each look
  // of a refusable button, each connection state and the closed picker.
  const controls = draw(
    <>
      <div data-case="copy, icon">
        <CopyButton value="CORE-1" what="issue key" />
      </div>
      <div data-case="copy, labelled">
        <CopyButton value="session-1" what="session ID">
          session-1
        </CopyButton>
      </div>
      <div data-case="copy, failed value">
        <CopyButton value="CORE-2" what="issue key" />
      </div>
      <div data-case="pins">
        <PinButton label="Unpin CORE-1" onClick={() => undefined} pinned />
        <PinButton label="Pin CORE-1" onClick={() => undefined} pinned={false} />
        <PinButton label="Pin this turn" onClick={() => undefined} pinned={false} quiet />
      </div>
      <div data-case="refusable">
        <RefusableButton refusal="Write a reason first.">Send</RefusableButton>
        <RefusableButton look="secondary-compact" refusal={null}>
          Attach
        </RefusableButton>
      </div>
      <div data-case="connection">
        <ConnectionDot connection="connected" pending={undefined} />
        <ConnectionDot connection="connecting" pending={undefined} />
        <ConnectionDot connection="failed" pending={undefined} />
        <ConnectionDot
          connection="offline"
          pending={{ count: 2, readOnly: false, rebuiltAt: undefined, stored: true }}
        />
      </div>
      <div data-case="closed picker">
        <MultiSelect
          emptyMessage="No labels yet."
          label="Labels"
          onChange={() => undefined}
          onOpenChange={() => undefined}
          open={false}
          options={["api"]}
          searchLabel="Search labels"
          selected={[]}
        />
      </div>
    </>,
    "outside"
  );
  const originalClipboard = navigator.clipboard;
  try {
    const scope = within(controls.container);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: async () => undefined },
    });
    await user.click(scope.getByRole("button", { name: "Copy issue key CORE-1" }));
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: async () => Promise.reject(new Error("denied")) },
    });
    await user.click(scope.getByRole("button", { name: "Copy session ID session-1" }));
    await user.click(scope.getByRole("button", { name: "Copy issue key CORE-2" }));
    await scope.findByText("Copied");
    await within(caseOf(controls, "copy, labelled")).findByText(/Copy failed/);
    classAt(
      "CopyButton failed value",
      await within(caseOf(controls, "copy, failed value")).findByText("CORE-2", {
        selector: "code",
      })
    );
    classAt("CopyButton, icon", scope.getByRole("button", { name: "Copy issue key CORE-1" }));
    classAt(
      "CopyButton, labelled",
      scope.getByRole("button", { name: "Copy session ID session-1" })
    );
    classAt("CopyButton copied", caseOf(controls, "copy, icon").querySelector("[aria-live]"));
    classAt("CopyButton failed", caseOf(controls, "copy, labelled").querySelector("[aria-live]"));

    classAt("PinButton, pinned", scope.getByRole("button", { name: "Unpin CORE-1" }));
    classAt("PinButton, unpinned", scope.getByRole("button", { name: "Pin CORE-1" }));
    classAt("PinButton, quiet", scope.getByRole("button", { name: "Pin this turn" }));

    classAt("RefusableButton primary", scope.getByRole("button", { name: "Send" }));
    classAt("RefusableButton secondary-compact", scope.getByRole("button", { name: "Attach" }));

    const dots = within(caseOf(controls, "connection")).getAllByRole("status");
    for (const [index, state] of ["connected", "connecting", "failed", "offline"].entries()) {
      classAt(`ConnectionDot ${state}`, dots[index]?.querySelector("span[aria-hidden]"));
    }
    classAt("ConnectionDot notice", dots[3]?.querySelectorAll("span[aria-hidden]")[1]);

    classAt(
      "MultiSelect trigger",
      within(caseOf(controls, "closed picker")).getByRole("button", { name: "Labels" })
    );
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    undraw(controls);
  }

  // The picker open, its popover portalled under `document.body`: a pick, a detail, a refusal
  // and Clear, and a second picker with nothing to pick.
  const pickers = draw(
    <>
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
      <MultiSelect
        emptyMessage="No topics yet."
        label="Topics"
        onChange={() => undefined}
        onOpenChange={() => undefined}
        open
        options={[]}
        searchLabel="Search topics"
        selected={[]}
      />
    </>,
    "outside"
  );
  try {
    const body = within(pickers.baseElement);
    const listbox = body.getByRole("listbox", { name: "Labels options" });
    classAt("MultiSelect panel", listbox.parentElement);
    const search = body.getByRole("combobox", { name: "Search labels" });
    classAt("MultiSelect search's host", search.parentElement);
    classAt("MultiSelect search", search);
    const [active, row] = within(listbox).getAllByRole("option");
    classAt("MultiSelect row, active", active);
    classAt("MultiSelect row", row);
    classAt("MultiSelect checkbox", active.querySelector("span[aria-hidden='true']"));
    classAt("MultiSelect option label", within(active).getByText("api"));
    classAt("MultiSelect option detail", within(active).getByText("3"));
    classAt("MultiSelect refusal", body.getByText("Labels must be 1–40 characters."));
    const clear = body.getByRole("button", { name: "Clear labels" });
    classAt("MultiSelect clear's host", clear.parentElement);
    classAt("MultiSelect clear", clear);
    classAt("MultiSelect empty", body.getByText("No topics yet."));
  } finally {
    undraw(pickers);
  }

  // The lines' refusals: an issue at the cap whose priorities could not be read, a save of what
  // an issue advances that failed, and a none attachment, whose change the server then refused.
  stubs.listPriorities.mockRejectedValueOnce(new Error("the priorities read failed"));
  const patchIssue = answerIssuePatches().mockRejectedValue(new Error("the connection dropped"));
  const refusals = draw(
    <>
      <div data-case="at the cap">
        <IssuePriorityLine
          disabled={false}
          issue={{
            advances: Array.from({ length: MAX_ISSUE_ADVANCES }, (_, index) => ({
              id: `priority-${index + 1}`,
              inherited: false,
            })),
            advances_revision: 1,
            key: "CORE-1",
          }}
        />
      </div>
      <div data-case="not saved">
        <IssuePriorityLine
          disabled={false}
          issue={{
            advances: [{ id: "rocq-lean", inherited: false }],
            advances_revision: 1,
            key: "CORE-2",
          }}
        />
      </div>
      <div data-case="none attached">
        <IssueComponentsLine
          issue={componentsIssue(
            {
              ids: [],
              inherited_from: null,
              mode: "none",
              reason: "A process change",
              unknown: [],
            },
            "CORE-3"
          )}
        />
      </div>
    </>,
    "outside"
  );
  try {
    const atCap = within(caseOf(refusals, "at the cap"));
    classAt("IssuePriorityLine at the cap", atCap.getByText(/^An issue advances at most/));
    classAt(
      "IssuePriorityLine priorities unread",
      await atCap.findByText("Could not load the priorities.")
    );
    const notSaved = within(caseOf(refusals, "not saved"));
    await user.click(notSaved.getByRole("button", { name: /^Remove / }));
    classAt(
      "IssuePriorityLine not saved",
      await notSaved.findByText("Could not save what this issue advances.")
    );
    const none = within(caseOf(refusals, "none attached"));
    classAt("IssueComponentsLine none", none.getByText(/^None — /));
    await user.click(await none.findByRole("button", { name: "Edit components of CORE-3" }));
    const body = within(refusals.baseElement);
    await user.click(
      await body.findByRole("option", { name: "dispatch-server · Dispatch server" })
    );
    // The picker closes, and saves, on an Escape its search box takes.
    body.getByRole("combobox", { name: "Search components" }).focus();
    await user.keyboard("{Escape}");
    classAt("IssueComponentsLine refusal", await none.findByRole("alert"));
  } finally {
    undraw(refusals);
    patchIssue.mockRestore();
  }

  // The approval chip's two dialogs: its reviews once read, while their read is out, once it
  // failed and with none yet, and a request for changes whose write failed.
  const review: ArtifactReview = {
    actor: { id: "bob", kind: "user" },
    artifact_id: "artifact-reviewed",
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason: "Please tighten the intro.",
    state: "changes_requested",
    version: 3,
  };
  const listArtifactReviews = spyOn(api, "listArtifactReviews").mockImplementation((id) => {
    if (id === "artifact-loading") return new Promise<ArtifactReview[]>(() => undefined);
    if (id === "artifact-unread") return Promise.reject(new Error("the reviews read failed"));
    return Promise.resolve(id === "artifact-unreviewed" ? [] : [review]);
  });
  const createArtifactReview = spyOn(api, "createArtifactReview").mockRejectedValue(
    new Error("the connection dropped")
  );
  const approvedV3 = { latest_version: 3, state: "approved", version: 3 } as const;
  const chips = draw(
    <>
      <div data-case="reviewed">
        <ApprovalChip
          artifact={doc("artifact-reviewed", "notes.md", {
            latest_version: 3,
            state: "changes_requested",
            version: 3,
          })}
          variant="header"
        />
      </div>
      <div data-case="reviews loading">
        <ApprovalChip artifact={doc("artifact-loading", "notes.md", approvedV3)} />
      </div>
      <div data-case="reviews unread">
        <ApprovalChip artifact={doc("artifact-unread", "notes.md", approvedV3)} />
      </div>
      <div data-case="reviews unreviewed">
        <ApprovalChip artifact={doc("artifact-unreviewed", "notes.md", approvedV3)} />
      </div>
    </>,
    "outside"
  );
  try {
    const reviewed = within(caseOf(chips, "reviewed"));
    await user.click(reviewed.getByRole("button", { name: "Changes requested" }));
    const reviews = await reviewed.findByRole("dialog", { name: "Reviews" });
    await within(reviews).findByText("Please tighten the intro.");
    classAt("ApprovalChip reviews backdrop", reviews.parentElement?.previousElementSibling);
    classAt("ApprovalChip reviews dialog", reviews);
    classAt(
      "ApprovalChip reviews heading",
      within(reviews).getByRole("heading", { name: "Reviews" })
    );
    const close = within(reviews).getByRole("button", { name: "Close" });
    classAt("ApprovalChip reviews Close", close);
    const entry = within(reviews).getByRole("listitem");
    classAt("ApprovalChip review", entry.firstElementChild);
    classAt("ApprovalChip review's reason", entry.lastElementChild);
    await user.click(close);
    for (const [state, key, words] of [
      ["loading", "ApprovalChip reviews loading", "Loading reviews…"],
      ["unread", "ApprovalChip reviews unread", "Could not load reviews."],
      ["unreviewed", "ApprovalChip no reviews", "No reviews yet."],
    ] as const) {
      const chip = within(caseOf(chips, `reviews ${state}`));
      await user.click(chip.getByRole("button", { name: "Approved v3" }));
      const dialog = await chip.findByRole("dialog", { name: "Reviews" });
      classAt(key, await within(dialog).findByText(words));
      await user.click(within(dialog).getByRole("button", { name: "Close" }));
    }

    await user.click(reviewed.getByRole("button", { name: "Request changes" }));
    const request = await reviewed.findByRole("dialog", { name: "Request changes" });
    const requestScope = within(request);
    classAt("ApprovalChip request backdrop", request.parentElement?.previousElementSibling);
    classAt("ApprovalChip request dialog", request);
    classAt(
      "ApprovalChip request heading",
      requestScope.getByRole("heading", { name: "Request changes" })
    );
    const reason = requestScope.getByRole("textbox", { name: "Reason" });
    classAt("ApprovalChip request label", reason.closest("label"));
    classAt("ApprovalChip request reason box", reason);
    classAt("ApprovalChip request Cancel", requestScope.getByRole("button", { name: "Cancel" }));
    const send = requestScope.getByRole("button", { name: "Request changes" });
    classAt("ApprovalChip request Request changes", send);
    await user.type(reason, "Tighten the intro.");
    await user.click(send);
    classAt("ApprovalChip request refused", await requestScope.findByRole("alert"));
  } finally {
    undraw(chips);
    listArtifactReviews.mockRestore();
    createArtifactReview.mockRestore();
  }

  // The document's own lines and controls: its open decisions, a block's references and the
  // line naming what changed since the reader last read it (version 1, and the spec's one block
  // changed in version 2), then each refusal its admission or its rebuild draws.
  stubs.listIssueAsks.mockResolvedValue([
    blockAsk("ask-b1", "b-1", "Should we ship?"),
    blockAsk("ask-b2", "b-2", "Which region?"),
  ]);
  stubs.getDocumentRead.mockImplementation(async (id) => ({
    artifact_id: id,
    read_at: "2026-09-09T00:00:00Z",
    version: 1,
  }));
  stubs.getReferences.mockImplementation(async (_reference, options) =>
    options?.kind === "part_of"
      ? {
          edges: [
            {
              created_at: "2026-09-10T00:00:00Z",
              direction: "in",
              excerpt: { block_id: "block-1", text: "The live document" },
              kind: "part_of",
              node: graphNode({
                block_id: "block-1",
                changed_version: 2,
                id: "CORE-1/spec#block-1",
                kind: "block",
              }),
              source_seq: null,
            },
          ],
          node: graphNode({ id: specDocument.id, kind: "artifact" }),
        }
      : { edges: [], node: graphNode({ id: "ask-1", kind: "ask" }) }
  );
  const decisions = draw(proofDocument(), "outside");
  try {
    const scope = within(decisions.container);
    const nav = await scope.findByRole("navigation", { name: "Open decisions" });
    classAt("ProofDocument open decisions navigator", nav);
    classAt("ProofDocument open decisions", nav.querySelector("span"));
    classAt("ProofDocument open decision link", within(nav).getByRole("link"));
    classAt(
      "ProofDocument Next open decision",
      within(nav).getByRole("button", { name: "Next open decision" })
    );
    classAt(
      "ProofDocument block references",
      await scope.findByRole("button", { name: "2 references on block" })
    );
    classAt(
      "ProofDocument read line",
      await scope.findByText(/^Changed since you last read version 1:/)
    );
  } finally {
    undraw(decisions);
  }
  stubs.listIssueAsks.mockResolvedValue([]);
  stubs.getDocumentRead.mockImplementation(async (id) => ({
    artifact_id: id,
    read_at: null,
    version: null,
  }));
  stubs.getReferences.mockResolvedValue({
    edges: [],
    node: graphNode({ id: "ask-1", kind: "ask" }),
  });
  // Every read of the document's text is refused, not the first alone: a load that reads the
  // text more than once, as one that reads it before it connects and again when it admits the
  // document does, meets the same refusal each time.
  stubs.getArtifactText.mockRejectedValue(
    new ApiError(409, {
      code: "DOC_SCHEMA",
      error: "The document holds a node outside its schema.",
    })
  );
  const repair = draw(proofDocument(), "outside");
  try {
    classAt(
      "ProofDocument repair refusal",
      await within(repair.container).findByText("The document holds a node outside its schema.")
    );
  } finally {
    undraw(repair);
  }

  stubs.getArtifactText.mockRejectedValue(
    new ApiError(409, { code: "DOCUMENT_UNLOADABLE", error: "The stored history cannot load." })
  );
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  const rebuildArtifact = spyOn(api, "rebuildArtifact").mockRejectedValue(
    new ApiError(422, { code: "REBUILD_REFUSED", error: "The history cannot be rebuilt now." })
  );
  const rebuild = draw(proofDocument(), "outside");
  try {
    const scope = within(rebuild.container);
    classAt(
      "ProofDocument rebuild refusal",
      await scope.findByText("The stored history cannot load.")
    );
    const rebuildButton = scope.getByRole("button", { name: "Rebuild from the latest version" });
    classAt("ProofDocument Rebuild", rebuildButton);
    await user.click(rebuildButton);
    classAt(
      "ProofDocument rebuild failure",
      await scope.findByText("The history cannot be rebuilt now.")
    );
  } finally {
    undraw(rebuild);
    confirm.mockRestore();
    rebuildArtifact.mockRestore();
  }

  stubs.getArtifactText.mockRejectedValue(
    new ApiError(404, { code: "PROXY_NOT_FOUND", error: "proxy not found" })
  );
  const unloaded = draw(proofDocument(), "outside");
  try {
    classAt(
      "ProofDocument load failure",
      await within(unloaded.container).findByText("This document could not load: proxy not found")
    );
  } finally {
    undraw(unloaded);
  }

  // The card's own elements: a blocking question in full, who handed it on, its answer box and
  // its actions, and the actions a question-shaped answer offers.
  const question = openAsk({ asked_of_at: "2026-10-09T00:00:00Z", asked_of_by: "bob@example.com" });
  const questionCard = draw(
    <AskCard
      ask={question}
      getAskThread={async () => threadOf(question)}
      initialThread={threadOf(question)}
    />,
    "outside"
  );
  try {
    await settleReads();
    const card = within(questionCard.container);
    const article = card.getByRole("article");
    classAt("AskCard card", article);
    classAt("AskCard urgency notch", article.querySelector(":scope > span[aria-hidden='true']"));
    classAt("AskCard question", article.querySelector(".dispatch-markdown")?.parentElement);
    const turn = card.getByTestId("turn-ask-1");
    classAt("AskCard turn", turn);
    classAt("AskCard asker line", turn.parentElement?.parentElement);
    classAt("AskCard turn and asker", turn.parentElement);
    const asker = card.getByRole("button", { expanded: false, name: /Implementer/ });
    classAt("AskCard asker", asker);
    classAt("AskCard asker's name", asker.closest("[data-agent-name]"));
    const handOn = card.getByRole("combobox", { name: "Hand this question to" });
    classAt("AskCard hand-on line", handOn.parentElement?.parentElement);
    classAt(
      "AskCard hand-on badge",
      handOn.parentElement?.querySelector("span[aria-hidden='true']")
    );
    classAt("AskCard handed on", card.getByText(/^handed on by bob@example\.com/));
    const answerBox = card.getByRole("textbox", { name: "Your answer" });
    classAt("AskCard answer label", answerBox.previousElementSibling);
    classAt("AskCard answer box", answerBox);
    classAt("AskCard Answer", card.getByRole("button", { name: "Answer" }));
    classAt("AskCard Ask back", card.getByRole("button", { name: "Ask back" }));
    await user.type(answerBox, "Which one?");
    await user.click(card.getByRole("button", { name: "Answer" }));
    classAt(
      "AskCard Ask back instead",
      await card.findByRole("button", { name: "Ask back instead" })
    );
    classAt(
      "AskCard Answer with it anyway",
      card.getByRole("button", { name: "Answer with it anyway" })
    );
  } finally {
    undraw(questionCard);
  }

  // A turn an agent holds, and the draft typed into the card above, which outlives that card: a
  // card of the same question, closed under the reader since, keeps it as the answer it never
  // sent.
  const agentsTurn = openAsk({ id: "ask-3", waiting_on: "agent" });
  const closed = openAsk({
    resolution: {
      actor: { id: "alice", kind: "user" },
      at: "2026-10-08T03:00:00Z",
      kind: "retracted",
      reason: "A newer question supersedes this one.",
    },
    state: "resolved",
  });
  const laterCards = draw(
    <>
      <div data-case="agent's turn">
        <AskCard
          ask={agentsTurn}
          getAskThread={async () => threadOf(agentsTurn)}
          initialThread={threadOf(agentsTurn)}
        />
      </div>
      <div data-case="unsent">
        <AskCard
          ask={closed}
          getAskThread={async () => threadOf(closed)}
          initialThread={threadOf(closed)}
        />
      </div>
    </>,
    "outside"
  );
  try {
    await settleReads();
    classAt(
      "AskCard turn, an agent's",
      within(caseOf(laterCards, "agent's turn")).getByTestId("turn-ask-3")
    );
    const unsent = within(caseOf(laterCards, "unsent")).getByTestId("ask-unsent-answer");
    classAt("AskCard unsent answer", unsent);
    classAt("AskCard unsent answer note", unsent.querySelector("p"));
    classAt("AskCard unsent answer words", within(unsent).getByText("Which one?"));
  } finally {
    undraw(laterCards);
  }

  // A compact approval request: its label, its document, the disclosure of the reader's own
  // words, and the reason Request changes asks for.
  const approval = openAsk({
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
  const approvalCard = draw(
    <AskCard
      ask={approval}
      getAskThread={async () => threadOf(approval)}
      initialThread={threadOf(approval)}
      variant="compact"
    />,
    "outside"
  );
  try {
    await settleReads();
    const card = within(approvalCard.container);
    const article = card.getByRole("article");
    classAt("AskCard compact card", article);
    classAt(
      "AskCard compact urgency notch",
      article.querySelector(":scope > span[aria-hidden='true']")
    );
    classAt("AskCard approval requested", card.getByText("Approval requested"));
    classAt("AskCard approval document", card.getByRole("link", { name: /spec\.md/ }).closest("p"));
    classAt(
      "AskCard own words",
      card.getByRole("button", { name: "Add a note or answer in your own words" })
    );
    await user.click(card.getByRole("radio", { name: /Request changes/ }));
    classAt("AskCard reason label", await card.findByText("Reason (required)"));
    classAt("AskCard submit hint", card.getByText(/^Add a reason to send/));
  } finally {
    undraw(approvalCard);
  }

  // A decision question's card, a card whose question changed under the answer the reader sent,
  // and a card changing the reader's recorded answer.
  const decision = blockAsk("ask-4", "b-1", "Should we ship?");
  const edited = openAsk({ id: "ask-5" });
  const recorded = openAsk({
    answer: { at: "2026-10-08T01:00:00Z", selected: ["Ship"], text: "First note", user: "alice" },
    id: "ask-6",
    state: "answered",
  });
  const otherCards = draw(
    <>
      <div data-case="decision">
        <AskCard
          ask={decision}
          getAskThread={async () => threadOf(decision)}
          initialThread={threadOf(decision)}
        />
      </div>
      <div data-case="edited">
        <AskCard
          answerAsk={async () => {
            throw new ApiError(409, { code: "ASK_EDITED", error: "question changed" });
          }}
          ask={edited}
          getAskThread={async () => threadOf(edited)}
          initialThread={threadOf(edited)}
        />
      </div>
      <div data-case="changing">
        <AskCard
          ask={recorded}
          getAskThread={async () => threadOf(recorded)}
          initialThread={threadOf(recorded)}
          mode="change"
        />
      </div>
    </>,
    "outside"
  );
  try {
    await settleReads();
    classAt(
      "AskCard block link",
      within(caseOf(otherCards, "decision"))
        .getByRole("link", { name: "Open in document" })
        .closest("p")
    );
    const changed = within(caseOf(otherCards, "edited"));
    await user.click(changed.getByRole("radio", { name: "Ship" }));
    await user.click(changed.getByRole("button", { name: "Answer" }));
    classAt(
      "AskCard question changed",
      await changed.findByText(/^This question changed while you were answering/)
    );
    classAt(
      "AskCard Cancel, changing an answer",
      await within(caseOf(otherCards, "changing")).findByRole("button", { name: "Cancel" })
    );
  } finally {
    undraw(otherCards);
  }

  expect(read).toEqual(want);
});

// A value that wraps inside `NoTooltips` keeps `main`'s classes outside it, the two the table
// above does not read among them: a labelled CopyButton's words carry no class, and a route
// marker that names its route, as today's issue header draws it, truncates that name.
test("outside NoTooltips a labelled CopyButton's words and a route marker's route keep main's classes", () => {
  const view = draw(
    <>
      <div data-case="copy">
        <CopyButton value="session-1" what="session ID">
          session-1
        </CopyButton>
      </div>
      <div data-case="route">
        <UnresponsiveRouteMarker
          issue={{
            route: "role:reviewer",
            route_holder: STALLED_SESSION,
            route_status: "holder_unresponsive",
            route_status_reason: null,
          }}
          showRoute
        />
        <UnreachableRouteMarker
          issue={{
            route: "role:architect",
            route_holder: null,
            route_status: "no_holder",
            route_status_reason: null,
          }}
          showRoute
        />
      </div>
    </>,
    "outside"
  );
  try {
    const words = within(caseOf(view, "copy")).getByText("session-1");
    expect(words.tagName).toBe("SPAN");
    expect(words.getAttribute("class")).toBeNull();
    const routes = [...caseOf(view, "route").querySelectorAll("span")].filter((span) =>
      /^role:(reviewer|architect):$/.test(span.textContent ?? "")
    );
    expect(routes.map((span) => span.getAttribute("class"))).toEqual([
      "min-w-0 truncate",
      "min-w-0 truncate",
    ]);
  } finally {
    undraw(view);
  }
});

// ---------------------------------------------------------------------------------------------
// The focus and Escape in the controls' own forms
// ---------------------------------------------------------------------------------------------

/**
 * Stands for Details on a phone around the controls it draws: a sheet of `useDialog` under the
 * application's key registry (`KeymapProvider`), whose Escape is the registry's dialog binding.
 * That binding takes a press inside a field (`inEditable`) and passes over one a handler already
 * marked `defaultPrevented` (`keymap.ts`), which is how a control's own Escape keeps the sheet
 * open. The sheet stays drawn: `onClose` is told of each press that would have closed it.
 */
function EscapeSheet({
  children,
  onClose,
}: {
  children: ReactNode;
  onClose: () => void;
}): ReactNode {
  const { containerRef } = useDialog<HTMLDivElement>({ onClose, open: true });
  return <div ref={containerRef}>{children}</div>;
}

/** Records each Escape pressed while it listens, from `document`'s capture phase, so a case can
 *  read once the press is over whether a handler marked it `defaultPrevented`. */
function recordEscapes(): { readonly presses: KeyboardEvent[]; readonly stop: () => void } {
  const presses: KeyboardEvent[] = [];
  const record = (event: KeyboardEvent) => {
    if (event.key === "Escape") presses.push(event);
  };
  document.addEventListener("keydown", record, true);
  return { presses, stop: () => document.removeEventListener("keydown", record, true) };
}

/** The priority line of an issue that advances none on `surface`: `main`'s line outside
 *  `NoTooltips`, and the value of a row of Details inside it. */
function addablePriorityLine(surface: Surface): ReactNode {
  return (
    <IssuePriorityLine
      disabled={false}
      inRow={surface === "inside"}
      issue={{ advances: [], advances_revision: 1, key: "CORE-1" }}
    />
  );
}

/** The line's add control, checked to be the one `surface` draws: `main`'s "+" outside
 *  `NoTooltips`, and the words of Details inside it. */
function addControlOf(view: RenderResult, surface: Surface): HTMLElement {
  const add = within(view.container).getByRole("button", {
    name: addPriorityControlName("CORE-1"),
  });
  expect(add.textContent).toBe(surface === "inside" ? CONTROL_LABELS.add_priority : "+");
  return add;
}

/** The add form's first field, the priority picker, once the form is open. */
function pickerOf(view: RenderResult): Promise<HTMLElement> {
  return within(view.container).findByRole("combobox", {
    name: CONTROL_LABELS.priority_to_advance,
  });
}

function pickerShown(view: RenderResult): boolean {
  return (
    within(view.container).queryByRole("combobox", { name: CONTROL_LABELS.priority_to_advance }) !==
    null
  );
}

// The ruling that focus handling may only improve: the add control the form replaces leaves the
// page, so opening the form puts the focus in its first field rather than leaving it on the body.
// On both sides of `NoTooltips`.
test("the priority's add control puts the focus in its form", async () => {
  const user = userEvent.setup();
  for (const surface of ["outside", "inside"] as const) {
    const view = draw(addablePriorityLine(surface), surface);
    try {
      // A keyboard reaches the control and presses Enter on it.
      addControlOf(view, surface).focus();
      await user.keyboard("{Enter}");
      expect(document.activeElement).toBe(await pickerOf(view));
    } finally {
      undraw(view);
    }
  }
});

// The same ruling: Cancel leaves the page with the form, so the focus returns to the add control
// drawn again in its place rather than dropping to the body.
test("Cancel in the priority's form returns the focus to its add control", async () => {
  const user = userEvent.setup();
  for (const surface of ["outside", "inside"] as const) {
    const view = draw(addablePriorityLine(surface), surface);
    try {
      await user.click(addControlOf(view, surface));
      expect(document.activeElement).toBe(await pickerOf(view));
      await user.click(within(view.container).getByRole("button", { name: CONTROL_LABELS.cancel }));
      expect(pickerShown(view)).toBe(false);
      expect(document.activeElement).toBe(addControlOf(view, surface));
    } finally {
      undraw(view);
    }
  }
});

// Escape in the form is the form's: it closes the form, returns the focus to the add control and
// reaches nothing around the line - neither a listener on `document`, where a panel listens, nor
// the dialog binding that closes Details on a phone. Only a second Escape, pressed on the add
// control, reaches them.
test("Escape in the priority's form closes the form alone and returns the focus to its add control", async () => {
  const user = userEvent.setup();
  for (const surface of ["outside", "inside"] as const) {
    // What each Escape reached past the line, in order.
    const reached: string[] = [];
    const onDocument = (event: KeyboardEvent) => {
      if (event.key === "Escape") reached.push("document");
    };
    document.addEventListener("keydown", onDocument);
    const escapes = recordEscapes();
    const view = draw(
      <KeymapProvider>
        <EscapeSheet onClose={() => reached.push("sheet")}>
          {addablePriorityLine(surface)}
        </EscapeSheet>
      </KeymapProvider>,
      surface
    );
    try {
      // The sheet's first focus lands on its one control before the case presses anything.
      await waitFor(() => expect(document.activeElement).toBe(addControlOf(view, surface)));
      await user.click(addControlOf(view, surface));
      expect(document.activeElement).toBe(await pickerOf(view));

      await user.keyboard("{Escape}");
      expect(pickerShown(view)).toBe(false);
      const add = addControlOf(view, surface);
      expect(document.activeElement).toBe(add);
      expect(reached).toEqual([]);
      expect(escapes.presses.map((event) => event.defaultPrevented)).toEqual([true]);

      await user.keyboard("{Escape}");
      expect(reached).toEqual(["document", "sheet"]);
    } finally {
      escapes.stop();
      document.removeEventListener("keydown", onDocument);
      undraw(view);
    }
  }
});

// Add closes the priority's form and draws the add control in its place, refused while Add's
// write is out, and inside `NoTooltips` until the read of the issue after it is drawn as well:
// there the write returns its reads, and it stops pending only once they land. Inside, the add
// control is `aria-disabled` in place of `disabled`, which holds no focus, so it takes the focus
// at once and keeps it, and a press of it opens no form, while the write is out and while its
// read is. Outside `NoTooltips` it is `disabled` while the write alone is out, as on `main`, so
// the focus waits on the body and moves to it once the write settles. On both sides the focus is
// on the add control once the read is drawn, and a focus the reader moved elsewhere while the
// write was out stays where they put it.
test("inside NoTooltips Add in the priority form keeps the focus on the add control until the issue is read", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    for (const reader of ["waits", "moves on"] as const) {
      const unemulate = emulateChromiumFocusFixup();
      const reads = issueReads([]);
      const written = Promise.withResolvers<Issue>();
      const patchIssue = answerIssuePatches().mockImplementation(() => written.promise);
      const view = draw(
        <>
          <button type="button">Elsewhere</button>
          <ReadPriorityLine inRow={surface === "inside"} read={reads.read} />
        </>,
        surface
      );
      try {
        const scope = within(view.container);
        await scope.findByRole("button", { name: addPriorityControlName("CORE-1") });
        addControlOf(view, surface).focus();
        await user.keyboard("{Enter}");
        const picker = await pickerOf(view);
        await waitFor(() => expect(within(picker).getAllByRole("option")).toHaveLength(3));
        await user.selectOptions(picker, "evals-now");
        scope.getByRole("button", { name: CONTROL_LABELS.add }).focus();
        await user.keyboard("{Enter}");
        await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
        await settleReads();
        expect(pickerShown(view)).toBe(false);
        const add = addControlOf(view, surface);
        if (surface === "inside") {
          expectRefusedHoldingFocus(add);
          await user.keyboard("{Enter}");
          await settleReads();
          expect(pickerShown(view)).toBe(false);
          expectRefusedHoldingFocus(add);
        } else {
          expectDisabledAsOnMain(add);
          expect(document.activeElement).toBe(document.body);
        }
        const elsewhere = scope.getByRole("button", { name: "Elsewhere" });
        if (reader === "moves on") elsewhere.focus();

        // The write lands, and the read after it is held.
        const answerReread = await landHeld(reads.read, () => {
          reads.advances = ["evals-now"];
          written.resolve({} as Issue);
        });
        expect(add.isConnected).toBe(true);
        if (surface === "inside" && reader === "waits") {
          // A press while the read is out opens no form, and the add control is still refused.
          await user.keyboard("{Enter}");
          await settleReads();
          expect(pickerShown(view)).toBe(false);
          expectRefusedHoldingFocus(add);
        } else if (surface === "outside") {
          // The write stopped pending as it landed, before its read, and a focus left on the body
          // moved to the add control then.
          expect(add.hasAttribute("disabled")).toBe(false);
          expect(document.activeElement).toBe(reader === "waits" ? add : elsewhere);
        }

        answerReread(reads.answer());
        await scope.findByRole("link", { name: "P1 Evals collecting data now" });
        await settleReads();
        expect(add.isConnected).toBe(true);
        expect(add.hasAttribute("aria-disabled")).toBe(false);
        expect(add.hasAttribute("disabled")).toBe(false);
        expect(document.activeElement).toBe(reader === "waits" ? add : elsewhere);
        expect(patchIssue).toHaveBeenCalledTimes(1);
      } finally {
        written.resolve({} as Issue);
        patchIssue.mockRestore();
        undraw(view);
        unemulate();
      }
    }
  }
});

// The priority's form stays open while a Remove's write is out, and its Add is refused then, and
// inside `NoTooltips` until the read of the issue after the write is drawn as well. Inside it is
// `aria-disabled` in place of `disabled`, so it keeps the focus, and a press of it sends nothing
// and leaves the form open; once the read is drawn it takes the next press. Outside `NoTooltips`
// it is `disabled` while the write is out, as on `main`.
test("inside NoTooltips the priority form's Add is refused while a Remove's write and its read are out", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const reads = issueReads(["rocq-lean"]);
    const written = Promise.withResolvers<Issue>();
    const patchIssue = answerIssuePatches().mockImplementation(() => written.promise);
    const view = draw(<ReadPriorityLine read={reads.read} />, surface);
    try {
      const scope = within(view.container);
      const remove = await scope.findByRole("button", {
        name: "Remove P2 Rocq and Lean speed work restarted from CORE-1",
      });
      await user.click(scope.getByRole("button", { name: addPriorityControlName("CORE-1") }));
      const picker = await pickerOf(view);
      await waitFor(() => expect(within(picker).getAllByRole("option")).toHaveLength(2));
      await user.selectOptions(picker, "evals-now");
      await user.click(remove);
      await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
      await settleReads();
      const submit = scope.getByRole("button", { name: CONTROL_LABELS.add });
      if (surface === "outside") {
        expectDisabledAsOnMain(submit);
        continue;
      }

      submit.focus();
      expectRefusedHoldingFocus(submit);
      await user.keyboard("{Enter}");
      await settleReads();
      expect(patchIssue).toHaveBeenCalledTimes(1);
      expect(pickerShown(view)).toBe(true);
      expectRefusedHoldingFocus(submit);

      // The write lands, and the read after it is held: a press then sends nothing either.
      const answerReread = await landHeld(reads.read, () => {
        reads.advances = [];
        written.resolve({} as Issue);
      });
      await user.keyboard("{Enter}");
      await settleReads();
      expect(patchIssue).toHaveBeenCalledTimes(1);
      expect(pickerShown(view)).toBe(true);
      expectRefusedHoldingFocus(submit);

      answerReread(reads.answer());
      await waitFor(() => expect(remove.isConnected).toBe(false));
      await settleReads();
      expect(document.activeElement).toBe(submit);
      expect(submit.hasAttribute("aria-disabled")).toBe(false);
      await user.keyboard("{Enter}");
      await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(2));
      expect(patchIssue.mock.calls[1]?.[1].advances).toEqual(["evals-now"]);
    } finally {
      written.resolve({} as Issue);
      patchIssue.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// The line's own `disabled`, which today's header sets on a closed issue, disables the add
// control on both sides of `NoTooltips`. Inside, `aria-disabled` holds the control only while a
// write of the line is out, and never stands in for the line's `disabled`: a press opens no form.
test("inside NoTooltips the add control of a disabled priority line is disabled and opens no form", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const view = draw(
      <IssuePriorityLine
        disabled
        inRow={surface === "inside"}
        issue={{ advances: [], advances_revision: 1, key: "CORE-1" }}
      />,
      surface
    );
    try {
      const add = addControlOf(view, surface);
      expect(add).toBeDisabled();
      expect(add.hasAttribute("aria-disabled")).toBe(false);
      await user.click(add);
      await settleReads();
      expect(pickerShown(view)).toBe(false);
    } finally {
      undraw(view);
    }
  }
});

/** A document header's approval chip, on the document as `ProofDocument` reads it: an approval
 *  invalidates that read, and the chip draws the document the read answers next. */
function ReadApprovalChip({ id }: { id: string }): ReactNode {
  const read = useQuery({ queryFn: () => api.getArtifact(id), queryKey: ["artifact", id] });
  return read.data === undefined ? null : <ApprovalChip artifact={read.data} variant="header" />;
}

/** The spec as the chip's read answers it before the reader reviews it: awaiting their approval. */
function awaitingSpec(): Artifact {
  return doc(SPEC_ID, "spec.md", { latest_version: 2, state: "awaiting", waiting_on: "human" });
}

/** The reader's review of the spec's version 2, as Dispatch answers it. */
function specReview(state: ArtifactReview["state"], reason: string | null = null): ArtifactReview {
  return {
    actor: { id: "alice", kind: "user" },
    artifact_id: SPEC_ID,
    ask_id: null,
    content_sha256: null,
    content_sha256_source: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    kind: "person",
    reason,
    state,
    version: 2,
  };
}

/** A read a case holds, answering `Answer`: a `spyOn` of an `api` read, or the `read` of
 *  `IssueReads` or `AskReads`. */
interface ReadStub<Answer> {
  readonly mock: { readonly calls: readonly unknown[] };
  mockImplementation(read: () => Promise<Answer>): unknown;
}

/** Lets a write `land` while every call of `read` from here on is held, and waits until the read
 *  the write's invalidation makes has started. Returns what answers the held reads. */
async function landHeld<Answer>(
  read: ReadStub<Answer>,
  land: () => void
): Promise<(answer: Answer) => void> {
  const made = read.mock.calls.length;
  const reread = Promise.withResolvers<Answer>();
  read.mockImplementation(() => reread.promise);
  land();
  await waitFor(() => expect(read).toHaveBeenCalledTimes(made + 1));
  await settleReads();
  return reread.resolve;
}

/** Inside `NoTooltips`, `control` holds the focus and refuses with `aria-disabled`, never
 *  `disabled`. The focus is read first: with `emulateChromiumFocusFixup` installed, a `disabled`
 *  control has dropped it to the body, which is what a reader in Chromium loses. */
function expectRefusedHoldingFocus(control: HTMLElement): void {
  expect(document.activeElement).toBe(control);
  expect(control.getAttribute("aria-disabled")).toBe("true");
  expect(control.hasAttribute("disabled")).toBe(false);
}

/** Outside `NoTooltips`, `control` refuses with `disabled` alone, as on `main`. */
function expectDisabledAsOnMain(control: HTMLElement): void {
  expect(control.hasAttribute("disabled")).toBe(true);
  expect(control.hasAttribute("aria-disabled")).toBe(false);
}

// Approve is refused while its write is out, and inside `NoTooltips` until the read after it is
// drawn as well: there the write returns its reads, and it stops pending only once they land.
// Inside, Approve is `aria-disabled` in place of `disabled`, which drops the focus to the body in a
// browser, so it keeps the focus, and a press sends no second approval, while the write is out and
// while its read is. Once that read draws the approval approved, Approve leaves the page, and the
// focus that fell with it goes to the chip that replaces it. Outside `NoTooltips` Approve is
// `disabled` while the write alone is out, as on `main`: the focus falls to the body, and nothing
// moves it.
test("inside NoTooltips Approve pressed by Enter keeps the focus until the approval is read, then hands it to the chip", async () => {
  const user = userEvent.setup();
  const approved = doc(SPEC_ID, "spec.md", { latest_version: 2, state: "approved", version: 2 });
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(<ReadApprovalChip id={SPEC_ID} />, surface);
    try {
      const scope = within(view.container);
      const approve = await scope.findByRole("button", { name: "Approve" });
      approve.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      await settleReads();
      if (surface === "inside") {
        expectRefusedHoldingFocus(approve);
        await user.keyboard("{Enter}");
        await settleReads();
        expect(createArtifactReview).toHaveBeenCalledTimes(1);
      } else {
        expectDisabledAsOnMain(approve);
        expect(document.activeElement).toBe(document.body);
      }

      // The approval lands, and the read after it is held.
      const answerReread = await landHeld(stubs.getArtifact, () =>
        written.resolve(specReview("approved"))
      );
      expect(approve.isConnected).toBe(true);
      if (surface === "inside") {
        // A press while the read is out sends no second approval, and Approve is still refused.
        await user.keyboard("{Enter}");
        await settleReads();
        expect(createArtifactReview).toHaveBeenCalledTimes(1);
        expectRefusedHoldingFocus(approve);
      } else {
        // The write stopped pending as it landed, before its read.
        expect(approve.hasAttribute("disabled")).toBe(false);
        expect(document.activeElement).toBe(document.body);
      }

      answerReread(approved);
      const chip = await scope.findByRole("button", { name: "Approved v2" });
      await settleReads();
      expect(approve.isConnected).toBe(false);
      expect(document.activeElement).toBe(surface === "inside" ? chip : document.body);
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
    } finally {
      written.reject(new Error("the connection dropped"));
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// A refused approval leaves Approve on the page. Inside `NoTooltips` Approve held the focus all
// along, since it was never `disabled`, keeps it once the write fails, and takes the next press,
// which sends the approval again. Outside `NoTooltips` its `disabled` dropped the focus to the
// body while the write was out, nothing moves it back, and Approve takes a press again, as on
// `main`.
test("inside NoTooltips Approve keeps the focus when its write fails and takes the next press", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(<ReadApprovalChip id={SPEC_ID} />, surface);
    try {
      const scope = within(view.container);
      const approve = await scope.findByRole("button", { name: "Approve" });
      approve.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      await settleReads();
      if (surface === "inside") expectRefusedHoldingFocus(approve);
      else expectDisabledAsOnMain(approve);

      written.reject(new Error("the connection dropped"));
      const refusal = surface === "inside" ? "aria-disabled" : "disabled";
      await waitFor(() => expect(approve.hasAttribute(refusal)).toBe(false));
      await settleReads();
      expect(approve.isConnected).toBe(true);
      expect(approve.hasAttribute("aria-disabled")).toBe(false);
      expect(approve.hasAttribute("disabled")).toBe(false);
      expect(document.activeElement).toBe(surface === "inside" ? approve : document.body);

      if (surface === "outside") approve.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(2));
    } finally {
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// The chip takes the focus from the body alone. A reader who moves on while Approve's write is out
// keeps the focus where they put it once the approval and the read after it land and Approve
// leaves the page, on both sides of `NoTooltips`.
test("inside NoTooltips Approve leaves a focus the reader moved elsewhere where it is", async () => {
  const user = userEvent.setup();
  const approved = doc(SPEC_ID, "spec.md", { latest_version: 2, state: "approved", version: 2 });
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(
      <>
        <ReadApprovalChip id={SPEC_ID} />
        <button type="button">Elsewhere</button>
      </>,
      surface
    );
    try {
      const scope = within(view.container);
      const approve = await scope.findByRole("button", { name: "Approve" });
      approve.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      const elsewhere = scope.getByRole("button", { name: "Elsewhere" });
      elsewhere.focus();

      const answerReread = await landHeld(stubs.getArtifact, () =>
        written.resolve(specReview("approved"))
      );
      expect(document.activeElement).toBe(elsewhere);
      answerReread(approved);
      await scope.findByRole("button", { name: "Approved v2" });
      await settleReads();
      expect(approve.isConnected).toBe(false);
      expect(document.activeElement).toBe(elsewhere);
    } finally {
      written.reject(new Error("the connection dropped"));
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// A dialog that drops its focus to the body is a keyboard trap broken open. While a change request
// is out, and inside `NoTooltips` until the read after it is drawn as well, the Request changes
// dialog refuses. Inside, its text box is read-only and its Request changes and Cancel are
// `aria-disabled`, in place of the `disabled` that drops the focus in a browser: the focus stays
// where it is, a press sends no second request and Cancel closes nothing. Once the new approval is
// read the dialog closes and gives the focus back to the header's Request changes, which opened it
// and is `aria-disabled` too while the write is out. Outside `NoTooltips` the text box, both
// buttons and the header's Request changes are `disabled` while the write is out, and Escape
// closes the dialog then, as on `main`.
test("inside NoTooltips the Request changes dialog keeps its focus until the change request is read", async () => {
  const user = userEvent.setup();
  const reason = "The plan names no owner";
  const changesRequested = doc(SPEC_ID, "spec.md", {
    latest_version: 2,
    state: "changes_requested",
    version: 2,
  });
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    // The registry's dialog binding is what Escape reaches the dialog by.
    const view = draw(
      <KeymapProvider>
        <ReadApprovalChip id={SPEC_ID} />
      </KeymapProvider>,
      surface
    );
    try {
      const scope = within(view.container);
      const requestChanges = await scope.findByRole("button", { name: "Request changes" });
      requestChanges.focus();
      await user.keyboard("{Enter}");
      const dialog = within(await scope.findByRole("dialog", { name: "Request changes" }));
      const box = dialog.getByRole("textbox", { name: "Reason" });
      await user.type(box, reason);
      const submit = dialog.getByRole("button", { name: "Request changes" });
      const cancel = dialog.getByRole("button", { name: "Cancel" });
      submit.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      await waitFor(() => expect(submit.textContent).toBe("Submitting…"));
      await settleReads();

      if (surface === "outside") {
        for (const control of [box, submit, cancel, requestChanges]) {
          expectDisabledAsOnMain(control);
        }
        expect(box.hasAttribute("readonly")).toBe(false);
        await user.keyboard("{Escape}");
        await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
        continue;
      }

      expect(box.hasAttribute("disabled")).toBe(false);
      expect(box.hasAttribute("readonly")).toBe(true);
      for (const control of [cancel, requestChanges]) {
        expect(control.getAttribute("aria-disabled")).toBe("true");
        expect(control.hasAttribute("disabled")).toBe(false);
      }
      expectRefusedHoldingFocus(submit);
      await user.keyboard("{Enter}");
      await settleReads();
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
      expectRefusedHoldingFocus(submit);
      expect(requestChanges.getAttribute("aria-disabled")).toBe("true");
      cancel.focus();
      await user.keyboard("{Enter}");
      await settleReads();
      expect(scope.queryByRole("dialog")).not.toBeNull();
      expectRefusedHoldingFocus(cancel);

      // The change request lands, and the read after it is held.
      submit.focus();
      const answerReread = await landHeld(stubs.getArtifact, () =>
        written.resolve(specReview("changes_requested", reason))
      );
      expect(scope.queryByRole("dialog")).not.toBeNull();
      // A press while the read is out sends no second change request, and the dialog's Request
      // changes is still refused.
      await user.keyboard("{Enter}");
      await settleReads();
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
      expect(submit.textContent).toBe("Submitting…");
      expectRefusedHoldingFocus(submit);

      answerReread(changesRequested);
      await scope.findByRole("button", { name: "Changes requested" });
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();
      expect(document.activeElement).toBe(requestChanges);
      expect(requestChanges.hasAttribute("aria-disabled")).toBe(false);
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
    } finally {
      written.reject(new Error("the connection dropped"));
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

/** Opens the Request changes dialog from the header's Request changes, types `reason` and sends it
 *  by a press of the dialog's own Request changes, then waits until `createArtifactReview`, which
 *  the case stubs, has been called. Returns the chip's Approve, or null where it draws none, and
 *  its Request changes; the dialog is still open. */
async function sendChangeRequest(
  user: UserEvent,
  view: RenderResult,
  createArtifactReview: Mock<typeof api.createArtifactReview>,
  reason: string
): Promise<{ approve: HTMLElement | null; requestChanges: HTMLElement }> {
  const scope = within(view.container);
  const requestChanges = await scope.findByRole("button", { name: "Request changes" });
  const approve = scope.queryByRole("button", { name: "Approve" });
  requestChanges.focus();
  await user.keyboard("{Enter}");
  const dialog = within(await scope.findByRole("dialog", { name: "Request changes" }));
  await user.type(dialog.getByRole("textbox", { name: "Reason" }), reason);
  dialog.getByRole("button", { name: "Request changes" }).focus();
  await user.keyboard("{Enter}");
  await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
  await settleReads();
  return { approve, requestChanges };
}

/** A refusal Dispatch gives a review: the version it would record has moved on. */
function reviewVersionMoved(): ApiError {
  return new ApiError(409, {
    code: "REVIEW_VERSION_MOVED",
    error: "The review would record version 3; read it first.",
  });
}

// A write keeps trying until Dispatch decides it, and a dialog that refused to close while it did
// would hold the reader behind its focus trap, away from the shell's Stop trying. Inside
// `NoTooltips` Escape closes the Request changes dialog while its change request is out and leaves
// the write going: the dialog gives the focus back to the header's Request changes, which stays
// `aria-disabled` beside Approve, a press of either doing nothing, until the change request lands
// and its read is drawn, and one change request goes out in all. Outside `NoTooltips` Escape
// closes the dialog and lets go of the write, so Approve and Request changes take a press again,
// as on `main`.
test("inside NoTooltips Escape closes the Request changes dialog while its change request is out and leaves the write going", async () => {
  const user = userEvent.setup();
  const reason = "The plan names no owner";
  const changesRequested = doc(SPEC_ID, "spec.md", {
    latest_version: 2,
    state: "changes_requested",
    version: 2,
  });
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(
      <KeymapProvider>
        <ReadApprovalChip id={SPEC_ID} />
      </KeymapProvider>,
      surface
    );
    try {
      const scope = within(view.container);
      const { approve, requestChanges } = await sendChangeRequest(
        user,
        view,
        createArtifactReview,
        reason
      );
      if (approve === null) throw new Error("the chip draws no Approve");
      await user.keyboard("{Escape}");
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();

      if (surface === "outside") {
        for (const control of [approve, requestChanges]) {
          expect(control.hasAttribute("disabled")).toBe(false);
          expect(control.hasAttribute("aria-disabled")).toBe(false);
        }
        continue;
      }

      expectRefusedHoldingFocus(requestChanges);
      await user.keyboard("{Enter}");
      await settleReads();
      expect(scope.queryByRole("dialog")).toBeNull();
      expectRefusedHoldingFocus(requestChanges);
      approve.focus();
      expectRefusedHoldingFocus(approve);
      await user.keyboard("{Enter}");
      await settleReads();
      expectRefusedHoldingFocus(approve);
      expect(createArtifactReview).toHaveBeenCalledTimes(1);

      // The change request lands, and the read after it is held: both are still refused.
      requestChanges.focus();
      const answerReread = await landHeld(stubs.getArtifact, () =>
        written.resolve(specReview("changes_requested", reason))
      );
      await user.keyboard("{Enter}");
      await settleReads();
      expect(scope.queryByRole("dialog")).toBeNull();
      expectRefusedHoldingFocus(requestChanges);
      expect(approve.getAttribute("aria-disabled")).toBe("true");

      answerReread(changesRequested);
      await scope.findByRole("button", { name: "Changes requested" });
      await waitFor(() => expect(requestChanges.hasAttribute("aria-disabled")).toBe(false));
      await settleReads();
      expect(document.activeElement).toBe(requestChanges);
      expect(approve.hasAttribute("aria-disabled")).toBe(false);
      expect(scope.queryByRole("dialog")).toBeNull();
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
    } finally {
      written.reject(new Error("the connection dropped"));
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// A change request Dispatch refuses while its dialog is open leaves the dialog open with its error
// and the reason the reader wrote. Inside `NoTooltips` nothing in it was `disabled`, so the focus
// stays on its Request changes, and the next press there sends the change request again. Once
// nothing is out Escape closes the dialog and gives the focus back to the header's Request
// changes. Outside `NoTooltips` the `disabled` submit dropped the focus to the body while the
// write was out, and nothing puts it back, as on `main`.
test("inside NoTooltips a refused change request keeps its dialog, its error and its focus", async () => {
  const user = userEvent.setup();
  const reason = "The plan names no owner";
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const refuse: ((error: Error) => void)[] = [];
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(() => {
      const written = Promise.withResolvers<ArtifactReview>();
      refuse.push(written.reject);
      return written.promise;
    });
    const view = draw(
      <KeymapProvider>
        <ReadApprovalChip id={SPEC_ID} />
      </KeymapProvider>,
      surface
    );
    try {
      const scope = within(view.container);
      const { requestChanges } = await sendChangeRequest(user, view, createArtifactReview, reason);
      const box = scope.getByRole("dialog", { name: "Request changes" });
      const dialog = within(box);
      const submit = dialog.getByRole("button", { name: "Submitting…" });
      for (const sent of [1, 2]) {
        if (sent === 2) {
          // Outside, the focus the `disabled` submit dropped is put back on it by hand.
          if (surface === "outside") submit.focus();
          await user.keyboard("{Enter}");
          await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(2));
          await settleReads();
        }
        expect(dialog.queryByRole("alert")).toBeNull();
        refuse.at(-1)?.(reviewVersionMoved());
        const alert = await dialog.findByRole("alert");
        await settleReads();
        expect(alert.textContent).toBe("Could not request changes. Try again.");
        expect(scope.getByRole("dialog", { name: "Request changes" })).toBe(box);
        expect(dialog.getByRole("textbox", { name: "Reason" })).toHaveValue(reason);
        expect(submit.hasAttribute("disabled")).toBe(false);
        expect(submit.hasAttribute("aria-disabled")).toBe(false);
        expect(document.activeElement).toBe(surface === "inside" ? submit : document.body);
      }

      await user.keyboard("{Escape}");
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();
      expect(document.activeElement).toBe(requestChanges);
      expect(requestChanges.hasAttribute("disabled")).toBe(false);
      expect(requestChanges.hasAttribute("aria-disabled")).toBe(false);
      expect(createArtifactReview).toHaveBeenCalledTimes(2);
    } finally {
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// A change request refused after the reader closed its dialog leaves nothing refused. Inside
// `NoTooltips` the refusal moves no focus: it stays on the header's Request changes, where the
// dialog gave it back, and Request changes opens the dialog again, which shows the refusal, as
// `main`'s dialog shows any refusal of a review left over. Outside `NoTooltips` Escape let go of
// the write, and the dialog opens again with no error, as on `main`.
test("inside NoTooltips a change request refused after its dialog closed lets the dialog open again", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(
      <KeymapProvider>
        <ReadApprovalChip id={SPEC_ID} />
      </KeymapProvider>,
      surface
    );
    try {
      const scope = within(view.container);
      const { approve, requestChanges } = await sendChangeRequest(
        user,
        view,
        createArtifactReview,
        "The plan names no owner"
      );
      await user.keyboard("{Escape}");
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();
      written.reject(reviewVersionMoved());
      await waitFor(() => expect(requestChanges.hasAttribute("aria-disabled")).toBe(false));
      await settleReads();
      expect(document.activeElement).toBe(requestChanges);
      for (const control of [approve, requestChanges]) {
        expect(control?.hasAttribute("disabled")).toBe(false);
        expect(control?.hasAttribute("aria-disabled")).toBe(false);
      }

      await user.keyboard("{Enter}");
      const dialog = within(await scope.findByRole("dialog", { name: "Request changes" }));
      await settleReads();
      expect(dialog.getByRole("textbox", { name: "Reason" })).toHaveValue("");
      expect(dialog.queryByRole("alert")?.textContent).toBe(
        surface === "inside" ? "Could not request changes. Try again." : undefined
      );
      await user.keyboard("{Escape}");
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();
      expect(document.activeElement).toBe(requestChanges);
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
    } finally {
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// While Approve's write is out, and inside `NoTooltips` until the read after it is drawn as well,
// Request changes is refused too. Inside it is `aria-disabled` in place of `disabled`, which
// refuses no press by itself, so the button's own press does nothing then: no dialog opens over a
// write that is out, and the focus stays on the button, where the approval's read leaves it.
// Outside `NoTooltips` it is `disabled` while the write is out, as on `main`.
test("inside NoTooltips Request changes opens no dialog while Approve's write is out", async () => {
  const user = userEvent.setup();
  const approved = doc(SPEC_ID, "spec.md", { latest_version: 2, state: "approved", version: 2 });
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(<ReadApprovalChip id={SPEC_ID} />, surface);
    try {
      const scope = within(view.container);
      const approve = await scope.findByRole("button", { name: "Approve" });
      approve.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
      await settleReads();
      const requestChanges = scope.getByRole("button", { name: "Request changes" });
      if (surface === "outside") {
        expectDisabledAsOnMain(requestChanges);
        continue;
      }

      requestChanges.focus();
      expectRefusedHoldingFocus(requestChanges);
      await user.keyboard("{Enter}");
      await settleReads();
      expect(scope.queryByRole("dialog")).toBeNull();
      expectRefusedHoldingFocus(requestChanges);

      // The approval lands and the read after it is held: a press then opens no dialog either.
      const answerReread = await landHeld(stubs.getArtifact, () =>
        written.resolve(specReview("approved"))
      );
      expect(document.activeElement).toBe(requestChanges);
      await user.keyboard("{Enter}");
      await settleReads();
      expect(scope.queryByRole("dialog")).toBeNull();
      expectRefusedHoldingFocus(requestChanges);

      answerReread(approved);
      await scope.findByRole("button", { name: "Approved v2" });
      await settleReads();
      expect(requestChanges.hasAttribute("aria-disabled")).toBe(false);
      expect(document.activeElement).toBe(requestChanges);
      expect(scope.queryByRole("dialog")).toBeNull();
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
    } finally {
      written.reject(new Error("the connection dropped"));
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

/** The chip as a header that draws its actions only while its request waits draws it, as the new
 *  page's header does: the read after a review then removes Approve and Request changes. */
function ReadWaitingApprovalChip({ id }: { id: string }): ReactNode {
  const read = useQuery({ queryFn: () => api.getArtifact(id), queryKey: ["artifact", id] });
  return read.data === undefined ? null : (
    <ApprovalChip
      artifact={read.data}
      showActions={read.data.approval?.state === "awaiting"}
      variant="header"
    />
  );
}

// Where the read after a change request removes Request changes, the dialog has no opener to give
// the focus back to. Inside `NoTooltips` the chip's one hand-over then goes to the chip, as it does
// once an approval removes Approve, and not to the body. Outside `NoTooltips` the chip moves no
// focus, as on `main`.
test("inside NoTooltips a change request whose read removes Request changes hands the focus to the chip", async () => {
  const user = userEvent.setup();
  const changesRequested = doc(SPEC_ID, "spec.md", {
    latest_version: 2,
    state: "changes_requested",
    version: 2,
  });
  for (const surface of ["inside", "outside"] as const) {
    let read = awaitingSpec();
    stubs.getArtifact.mockImplementation(async () => read);
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(async () => {
      read = changesRequested;
      return specReview("changes_requested", "The plan names no owner");
    });
    const view = draw(<ReadWaitingApprovalChip id={SPEC_ID} />, surface);
    try {
      const scope = within(view.container);
      const requestChanges = await scope.findByRole("button", { name: "Request changes" });
      requestChanges.focus();
      await user.keyboard("{Enter}");
      const dialog = within(await scope.findByRole("dialog", { name: "Request changes" }));
      await user.type(dialog.getByRole("textbox", { name: "Reason" }), "The plan names no owner");
      dialog.getByRole("button", { name: "Request changes" }).focus();
      await user.keyboard("{Enter}");
      const chip = await scope.findByRole("button", { name: "Changes requested" });
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
      expect(scope.queryByRole("button", { name: "Request changes" })).toBeNull();
      expect(document.activeElement).toBe(surface === "inside" ? chip : document.body);
    } finally {
      createArtifactReview.mockRestore();
      undraw(view);
    }
  }
});

// A change request whose dialog the reader closed while it was out had the focus given back to
// Request changes at once. Inside `NoTooltips`, once the change request lands and its read removes
// Request changes, the chip's one hand-over takes the focus that fell to the body, as it does when
// the dialog stays open to the end. Outside `NoTooltips` the chip moves no focus, as on `main`.
test("inside NoTooltips a change request whose dialog was closed hands the focus to the chip once its read removes Request changes", async () => {
  const user = userEvent.setup();
  const reason = "The plan names no owner";
  const changesRequested = doc(SPEC_ID, "spec.md", {
    latest_version: 2,
    state: "changes_requested",
    version: 2,
  });
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    stubs.getArtifact.mockResolvedValue(awaitingSpec());
    const written = Promise.withResolvers<ArtifactReview>();
    const createArtifactReview = spyOn(api, "createArtifactReview").mockImplementation(
      () => written.promise
    );
    const view = draw(
      <KeymapProvider>
        <ReadWaitingApprovalChip id={SPEC_ID} />
      </KeymapProvider>,
      surface
    );
    try {
      const scope = within(view.container);
      const { requestChanges } = await sendChangeRequest(user, view, createArtifactReview, reason);
      await user.keyboard("{Escape}");
      await waitFor(() => expect(scope.queryByRole("dialog")).toBeNull());
      await settleReads();
      expect(document.activeElement).toBe(requestChanges);

      const answerReread = await landHeld(stubs.getArtifact, () =>
        written.resolve(specReview("changes_requested", reason))
      );
      answerReread(changesRequested);
      const chip = await scope.findByRole("button", { name: "Changes requested" });
      await waitFor(() => expect(requestChanges.isConnected).toBe(false));
      await settleReads();
      expect(document.activeElement).toBe(surface === "inside" ? chip : document.body);
      expect(createArtifactReview).toHaveBeenCalledTimes(1);
    } finally {
      written.reject(new Error("the connection dropped"));
      createArtifactReview.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

/** Runs `run` with `copyText`'s fallback as a browser runs it: `document.execCommand` copies, and
 *  each command it is given is recorded in `copied`; the temporary text box's `select()` takes the
 *  focus. The clipboard a case sets (`setClipboard`) is put back after. */
async function withCopyFallback(run: (copied: string[]) => Promise<void>): Promise<void> {
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const copied: string[] = [];
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (command: string) => {
      copied.push(command);
      return true;
    },
  });
  // A browser's `select()` focuses the text box; happy-dom's only selects its text.
  const select = spyOn(HTMLTextAreaElement.prototype, "select").mockImplementation(function (
    this: HTMLTextAreaElement
  ) {
    this.focus();
  });
  try {
    await run(copied);
  } finally {
    select.mockRestore();
    setClipboard(originalClipboard);
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
  }
}

/** Gives the page `clipboard`: `undefined`, as on an origin that is not secure, or a clipboard
 *  whose `writeText` answers as the case says. */
function setClipboard(clipboard: Pick<Clipboard, "writeText"> | undefined): void {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
}

// Where the clipboard is missing (an origin that is not secure) or refuses, `copyText` copies
// through a temporary text box that takes the focus and leaves the page, which drops the focus to
// the body. Inside `NoTooltips` the focus returns to the copy button after each copy: at once
// where the clipboard is missing, and once the copy settles where the clipboard refused, on a
// second press made while the first copy's "Copied" still shows too. Outside `NoTooltips` the
// focus stays where the copy left it, the body.
test("inside NoTooltips a copy pressed by Enter keeps the focus on its button", async () => {
  const user = userEvent.setup();
  await withCopyFallback(async (copied) => {
    for (const surface of ["inside", "outside"] as const) {
      for (const clipboard of ["missing", "refusing"] as const) {
        setClipboard(
          clipboard === "missing"
            ? undefined
            : { writeText: async () => Promise.reject(new Error("denied")) }
        );
        copied.length = 0;
        const view = draw(<CopyButton value="CORE-1" what="issue key" />, surface);
        try {
          const scope = within(view.container);
          const button = scope.getByRole("button", { name: "Copy issue key CORE-1" });
          for (let press = 0; press < 2; press += 1) {
            // The second press is made within the first copy's 1.5 s, while its "Copied" shows.
            if (press > 0) expect(scope.queryByText("Copied")).not.toBeNull();
            button.focus();
            await user.keyboard("{Enter}");
            await scope.findByText("Copied");
            await settleReads();
            expect(copied).toHaveLength(press + 1);
            const focus =
              document.activeElement === button
                ? "the copy button"
                : document.activeElement === document.body
                  ? "the body"
                  : document.activeElement?.outerHTML;
            expect({ clipboard, focus, press }).toEqual({
              clipboard,
              focus: surface === "inside" ? "the copy button" : "the body",
              press,
            });
          }
        } finally {
          undraw(view);
        }
      }
    }
  });
});

// A reader who moves the focus to another control while a copy is out keeps it there: the
// clipboard refuses after the move, and the fallback's text box takes the focus and leaves it on
// the body, so inside `NoTooltips` the focus goes back to where the reader put it, not to the
// copy button. Outside `NoTooltips` the focus stays where the copy left it, the body.
test("inside NoTooltips a copy leaves a focus the reader moved elsewhere where it is", async () => {
  const user = userEvent.setup();
  await withCopyFallback(async (copied) => {
    for (const surface of ["inside", "outside"] as const) {
      const refused = Promise.withResolvers<void>();
      setClipboard({ writeText: () => refused.promise });
      copied.length = 0;
      const view = draw(
        <>
          <CopyButton value="CORE-1" what="issue key" />
          <button type="button">Elsewhere</button>
        </>,
        surface
      );
      try {
        const scope = within(view.container);
        const button = scope.getByRole("button", { name: "Copy issue key CORE-1" });
        const elsewhere = scope.getByRole("button", { name: "Elsewhere" });
        button.focus();
        await user.keyboard("{Enter}");
        await settleReads();
        // The copy is out: the clipboard has not answered, and the fallback has not run.
        expect(copied).toEqual([]);
        expect(document.activeElement).toBe(button);

        elsewhere.focus();
        refused.reject(new Error("denied"));
        await scope.findByText("Copied");
        await settleReads();
        expect(copied).toEqual(["copy"]);
        expect(document.activeElement).toBe(surface === "inside" ? elsewhere : document.body);
      } finally {
        undraw(view);
      }
    }
  });
});

// A copy's write to the clipboard is the button's write: inside `NoTooltips` a press while it is
// out does nothing, so a second Enter makes no second write and draws no "Copied" before the first
// write lands, and the focus stays on the button, which says it is refused (`aria-disabled`) until
// the write lands and no longer after. Once the write lands, a press made while "Copied" shows
// copies again. Outside `NoTooltips` each press writes and the button is never marked refused, as
// on `main`.
test("inside NoTooltips a copy pressed again by Enter while its write is out makes no second write", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const writes: { resolve: () => void }[] = [];
  setClipboard({
    writeText: () => {
      const write = Promise.withResolvers<void>();
      writes.push(write);
      return write.promise;
    },
  });
  try {
    for (const surface of ["inside", "outside"] as const) {
      writes.length = 0;
      const view = draw(<CopyButton value="CORE-1" what="issue key" />, surface);
      try {
        const scope = within(view.container);
        const button = scope.getByRole("button", { name: "Copy issue key CORE-1" });
        button.focus();
        await user.keyboard("{Enter}");
        await waitFor(() => expect(writes).toHaveLength(1));
        await user.keyboard("{Enter}");
        await settleReads();
        expect(writes).toHaveLength(surface === "inside" ? 1 : 2);
        if (surface === "inside") {
          expect(scope.queryByText("Copied")).toBeNull();
          expect(document.activeElement).toBe(button);
          expect(button.getAttribute("aria-disabled")).toBe("true");
        } else {
          expect(button.hasAttribute("aria-disabled")).toBe(false);
        }
        for (const write of writes) write.resolve();
        await scope.findByText("Copied");
        // The mark goes in the render after the one that draws Copied, so the case waits for it.
        await waitFor(() => expect(button.hasAttribute("aria-disabled")).toBe(false));
        if (surface === "inside") {
          await user.keyboard("{Enter}");
          await waitFor(() => expect(writes).toHaveLength(2));
          expect(document.activeElement).toBe(button);
          expect(button.getAttribute("aria-disabled")).toBe("true");
        }
      } finally {
        for (const write of writes) write.resolve();
        undraw(view);
      }
    }
  } finally {
    setClipboard(originalClipboard);
  }
});

/** CORE-1's priority line on the issue as `read` answers it: a write of what the issue advances
 *  invalidates that read, and the line draws what the read answers next. `inRow` is the line's. */
function ReadPriorityLine({
  inRow = false,
  read,
}: {
  inRow?: boolean;
  read: () => Promise<Pick<Issue, "advances" | "advances_revision" | "key">>;
}): ReactNode {
  const issue = useQuery({ queryFn: read, queryKey: ["issue", "CORE-1"] });
  return issue.data === undefined ? null : (
    <IssuePriorityLine disabled={false} inRow={inRow} issue={issue.data} />
  );
}

/** CORE-1's reads, as `ReadPriorityLine` makes them (`read`). Until `landHeld` holds `read`, each
 *  answers `answer()`: the issue advancing, as its own, what `advances` holds as it answers. */
interface IssueReads {
  advances: readonly string[];
  readonly answer: () => Pick<Issue, "advances" | "advances_revision" | "key">;
  readonly read: Mock<() => Promise<Pick<Issue, "advances" | "advances_revision" | "key">>>;
}

function issueReads(advances: readonly string[]): IssueReads {
  const reads: IssueReads = {
    advances,
    answer: () => ({
      advances: reads.advances.map((id) => ({ id, inherited: false })),
      advances_revision: 1,
      key: "CORE-1",
    }),
    read: mock(async () => reads.answer()),
  };
  return reads;
}

/** Has the priorities read name a third priority, `P3 Lean proofs checked`, after the
 *  fixture's two. */
function listThreePriorities(): void {
  const rows = priorities.priorities ?? [];
  const first = rows[0];
  if (first === undefined) throw new Error("the fixture lists no priority");
  stubs.listPriorities.mockResolvedValue({
    ...priorities,
    priorities: [
      ...rows,
      { ...first, id: "lean-proofs", rank: "lean-proofs", title: "Lean proofs checked" },
    ],
  });
}

// A priority's Remove is refused while its write is out, and inside `NoTooltips` until the read
// of the issue after it is drawn as well: there the write returns its reads, and it stops pending
// only once they land. Inside, each Remove is `aria-disabled` in place of `disabled`, which drops
// the focus to the body in a browser, so the pressed Remove keeps the focus, and a press sends no
// second write, while the write is out and while its read is. Once that read draws the issue
// without the entry, the focus that fell with its Remove goes to the Remove of the entry after
// it, else of the entry before it, else to the add control; once the write failed, the entry
// stays, and its Remove keeps the focus it held all along. Outside `NoTooltips` each Remove is
// `disabled` while the write alone is out, as on `main`: the focus falls to the body, and the
// line moves none.
test("inside NoTooltips a priority's Remove pressed by Enter keeps the focus until the issue is read, then hands it to the next priority's Remove", async () => {
  const user = userEvent.setup();
  listThreePriorities();
  const evals = "Remove P1 Evals collecting data now from CORE-1";
  const rocq = "Remove P2 Rocq and Lean speed work restarted from CORE-1";
  const lean = "Remove P3 Lean proofs checked from CORE-1";
  for (const surface of ["inside", "outside"] as const) {
    for (const write of ["lands", "fails"] as const) {
      const unemulate = emulateChromiumFocusFixup();
      const reads = issueReads(["evals-now", "rocq-lean", "lean-proofs"]);
      const patchIssue = answerIssuePatches();
      const view = draw(<ReadPriorityLine read={reads.read} />, surface);
      try {
        const scope = within(view.container);
        // Presses Enter on the Remove named `name`, holds its write and then the read after it,
        // and lets both land; answers the Remove.
        const press = async (name: string) => {
          const remove = await scope.findByRole("button", { name });
          const calls = patchIssue.mock.calls.length;
          const written = Promise.withResolvers<Issue>();
          patchIssue.mockImplementation(() => written.promise);
          remove.focus();
          await user.keyboard("{Enter}");
          await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(calls + 1));
          await settleReads();
          if (surface === "inside") {
            expectRefusedHoldingFocus(remove);
            await user.keyboard("{Enter}");
            await settleReads();
            expect(patchIssue).toHaveBeenCalledTimes(calls + 1);
          } else {
            expectDisabledAsOnMain(remove);
            expect(document.activeElement).toBe(document.body);
          }

          // The write lands, or fails, and the read after it is held.
          const sent = patchIssue.mock.calls[calls]?.[1].advances ?? [];
          const answerReread = await landHeld(reads.read, () => {
            if (write === "fails") {
              written.reject(new Error("the connection dropped"));
              return;
            }
            reads.advances = sent;
            written.resolve({} as Issue);
          });
          expect(remove.isConnected).toBe(true);
          if (surface === "inside") {
            // A press while the read is out sends no second write, and the Remove is still
            // refused.
            await user.keyboard("{Enter}");
            await settleReads();
            expect(patchIssue).toHaveBeenCalledTimes(calls + 1);
            expectRefusedHoldingFocus(remove);
          } else {
            // The write stopped pending as it landed, before its read.
            expect(remove.hasAttribute("disabled")).toBe(false);
            expect(document.activeElement).toBe(document.body);
          }

          answerReread(reads.answer());
          if (write === "lands") {
            await waitFor(() => expect(remove.isConnected).toBe(false));
          } else {
            await scope.findByRole("alert");
          }
          await settleReads();
          expect(patchIssue).toHaveBeenCalledTimes(calls + 1);
          return remove;
        };
        if (write === "lands") {
          // The first of three: the focus goes to the Remove of the entry after it.
          await press(evals);
          expect(document.activeElement).toBe(
            surface === "inside" ? scope.getByRole("button", { name: rocq }) : document.body
          );
          // The last of two: the focus goes to the Remove of the entry before it.
          await press(lean);
          expect(document.activeElement).toBe(
            surface === "inside" ? scope.getByRole("button", { name: rocq }) : document.body
          );
          // The only one left: the focus goes to the add control.
          await press(rocq);
          expect(document.activeElement).toBe(
            surface === "inside"
              ? scope.getByRole("button", { name: addPriorityControlName("CORE-1") })
              : document.body
          );
        } else {
          // The write failed and the entry stays: its Remove takes a press again, and inside it
          // kept the focus.
          const remove = await press(rocq);
          expect(remove.isConnected).toBe(true);
          expect(remove.hasAttribute("aria-disabled")).toBe(false);
          expect(remove.hasAttribute("disabled")).toBe(false);
          expect(document.activeElement).toBe(surface === "inside" ? remove : document.body);
        }
      } finally {
        patchIssue.mockRestore();
        undraw(view);
        unemulate();
      }
    }
  }
});

// The line takes the focus from the body alone. A reader who moves on while a Remove's write is
// out keeps the focus where they put it once the write and the read after it land and the entry
// leaves the page, on both sides of `NoTooltips`.
test("inside NoTooltips a priority's Remove leaves a focus the reader moved elsewhere where it is", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const reads = issueReads(["evals-now", "rocq-lean"]);
    const written = Promise.withResolvers<Issue>();
    const patchIssue = answerIssuePatches().mockImplementation(() => written.promise);
    const view = draw(
      <>
        <ReadPriorityLine read={reads.read} />
        <button type="button">Elsewhere</button>
      </>,
      surface
    );
    try {
      const scope = within(view.container);
      const remove = await scope.findByRole("button", {
        name: "Remove P1 Evals collecting data now from CORE-1",
      });
      remove.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
      const elsewhere = scope.getByRole("button", { name: "Elsewhere" });
      elsewhere.focus();

      const answerReread = await landHeld(reads.read, () => {
        reads.advances = ["rocq-lean"];
        written.resolve({} as Issue);
      });
      expect(document.activeElement).toBe(elsewhere);
      answerReread(reads.answer());
      await waitFor(() => expect(remove.isConnected).toBe(false));
      await settleReads();
      expect(document.activeElement).toBe(elsewhere);
      expect(patchIssue).toHaveBeenCalledTimes(1);
    } finally {
      written.resolve({} as Issue);
      patchIssue.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

// The Remove of an entry with entries on both sides hands its focus to the Remove of the entry
// after it, not of the entry before it. The case above presses a first and a last entry only,
// where the two orders agree. Outside `NoTooltips` the focus stays on the body.
test("inside NoTooltips the Remove of a middle priority hands its focus to the entry after it", async () => {
  const user = userEvent.setup();
  listThreePriorities();
  const rocq = "Remove P2 Rocq and Lean speed work restarted from CORE-1";
  const lean = "Remove P3 Lean proofs checked from CORE-1";
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const reads = issueReads(["evals-now", "rocq-lean", "lean-proofs"]);
    const patchIssue = answerIssuePatches().mockImplementation(async (_key, input) => {
      reads.advances = input.advances ?? reads.advances;
      return {} as Issue;
    });
    const view = draw(<ReadPriorityLine read={reads.read} />, surface);
    try {
      const scope = within(view.container);
      const remove = await scope.findByRole("button", { name: rocq });
      remove.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
      await waitFor(() => expect(scope.queryByRole("button", { name: rocq })).toBeNull());
      await settleReads();
      expect(patchIssue.mock.calls[0]?.[1].advances).toEqual(["evals-now", "lean-proofs"]);
      expect(document.activeElement).toBe(
        surface === "inside" ? scope.getByRole("button", { name: lean }) : document.body
      );
    } finally {
      patchIssue.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

/** What Dispatch answers a rebuild that restored the document's latest saved version. */
const rebuiltReport: ArtifactRebuildReport = {
  head: 1,
  removed_checkpoints: 0,
  removed_snapshots: 0,
  removed_updates: 3,
  source_version: 1,
  validation_error: "decode live document: unexpected end of input",
};

// Rebuild is refused while its write is out, and inside `NoTooltips` until the read of the
// document after it is drawn as well: there the rebuild returns its reads, and it stops pending
// only once they land. Inside, it says so with `aria-disabled` in place of `disabled`, which drops
// the focus in a browser, so it keeps its focus, and a press then asks nothing and sends no second
// rebuild, while the rebuild is out and while its read is. Once the text reads, the offer leaves
// the page with Rebuild, which hands its focus to nothing: that is the page's own to place. A
// focus the reader moved elsewhere while the rebuild was out stays where they put it. Outside
// `NoTooltips` Rebuild is `disabled` while the rebuild alone is out, as on `main`.
test("inside NoTooltips Rebuild keeps its focus until the rebuilt document is read", async () => {
  const user = userEvent.setup();
  const confirm = spyOn(window, "confirm").mockReturnValue(true);
  try {
    for (const surface of ["inside", "outside"] as const) {
      for (const reader of ["waits", "moves on"] as const) {
        confirm.mockClear();
        const unemulate = emulateChromiumFocusFixup();
        // Every text read is refused, however many the load makes.
        stubs.getArtifactText.mockRejectedValue(
          new ApiError(409, {
            code: "DOCUMENT_UNLOADABLE",
            error: "The stored history cannot load.",
          })
        );
        const rebuilt = Promise.withResolvers<ArtifactRebuildReport>();
        const rebuildArtifact = spyOn(api, "rebuildArtifact").mockImplementation(
          () => rebuilt.promise
        );
        const view = draw(
          <>
            <button type="button">Elsewhere</button>
            {proofDocument()}
          </>,
          surface
        );
        try {
          const scope = within(view.container);
          const rebuild = await scope.findByRole("button", {
            name: "Rebuild from the latest version",
          });
          rebuild.focus();
          await user.keyboard("{Enter}");
          await waitFor(() => expect(rebuildArtifact).toHaveBeenCalledTimes(1));
          await waitFor(() => expect(rebuild.textContent).toBe("Rebuilding…"));
          await settleReads();
          if (surface === "inside") {
            expectRefusedHoldingFocus(rebuild);
            await user.keyboard("{Enter}");
            await settleReads();
            expectRefusedHoldingFocus(rebuild);
          } else {
            expectDisabledAsOnMain(rebuild);
            expect(document.activeElement).toBe(document.body);
          }
          expect(confirm).toHaveBeenCalledTimes(1);
          expect(rebuildArtifact).toHaveBeenCalledTimes(1);
          const elsewhere = scope.getByRole("button", { name: "Elsewhere" });
          if (reader === "moves on") elsewhere.focus();

          // The rebuild lands, and the read of the document's text after it is held.
          const answerReread = await landHeld(stubs.getArtifactText, () =>
            rebuilt.resolve(rebuiltReport)
          );
          expect(rebuild.isConnected).toBe(true);
          if (surface === "inside" && reader === "waits") {
            // A press while the read is out asks nothing and sends no second rebuild, and
            // Rebuild is still refused.
            await user.keyboard("{Enter}");
            await settleReads();
            expect(confirm).toHaveBeenCalledTimes(1);
            expect(rebuildArtifact).toHaveBeenCalledTimes(1);
            expectRefusedHoldingFocus(rebuild);
          }
          if (surface === "inside") {
            expect(rebuild.textContent).toBe("Rebuilding…");
          } else {
            // The rebuild stopped pending as it landed, before its read.
            expect(rebuild.hasAttribute("disabled")).toBe(false);
            expect(rebuild.textContent).toBe("Rebuild from the latest version");
          }

          answerReread({
            markdown: "The live document",
            token: "sha256:spec-text",
            version: null,
          });
          await waitFor(() => expect(rebuild.isConnected).toBe(false));
          await settleReads();
          expect(document.activeElement).toBe(reader === "waits" ? document.body : elsewhere);
          expect(confirm).toHaveBeenCalledTimes(1);
          expect(rebuildArtifact).toHaveBeenCalledTimes(1);
        } finally {
          rebuilt.reject(
            new ApiError(422, {
              code: "REBUILD_REFUSED",
              error: "The history cannot be rebuilt now.",
            })
          );
          undraw(view);
          rebuildArtifact.mockRestore();
          unemulate();
        }
      }
    }
  } finally {
    confirm.mockRestore();
  }
});

/** An issue the identity block's typed editors edit: the margin fixture's issue, with no
 *  documents, as `shared-writes.test.ts` builds its own. */
const fieldIssue: IssueDetails = {
  ...marginIssue,
  artifacts: [],
  last_seq: 250,
  primary_artifact_id: "",
  status: "todo",
};

const fieldState: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

/** The identity block's editor of `field`, open as the page opens it: on the drafts of the
 *  page's `useIssueHeaderWrites`, and closed by its `onClose`, which `onClose` here is told of. */
function OpenFieldForm({ field, onClose }: { field: IssueField; onClose: () => void }): ReactNode {
  const writes = useIssueHeaderWrites(fieldIssue, fieldState);
  const [open, setOpen] = useState<IssueField | null>(field);
  return (
    <IssueFieldForms
      field={open}
      isClosed={false}
      issue={fieldIssue}
      onClose={() => {
        onClose();
        setOpen(null);
      }}
      writes={writes}
    />
  );
}

/** Presses Escape in the `field` box, open in Details on a phone, and checks that the press
 *  closed the box alone: the box is gone, its `onClose` ran, and the press, marked
 *  `defaultPrevented`, passed the sheet's dialog binding by. A second Escape, with the box gone,
 *  closes the sheet, so the binding the first press passed is one that would have closed it. */
async function escapeInFieldBox(field: IssueField, label: string): Promise<void> {
  const user = userEvent.setup();
  // What each Escape closed, in order.
  const closed: string[] = [];
  const escapes = recordEscapes();
  const view = draw(
    <KeymapProvider>
      <EscapeSheet onClose={() => closed.push("sheet")}>
        <OpenFieldForm field={field} onClose={() => closed.push("box")} />
      </EscapeSheet>
    </KeymapProvider>,
    "inside"
  );
  try {
    const box = within(view.container).getByRole("textbox", { name: label });
    await waitFor(() => expect(document.activeElement).toBe(box));

    await user.keyboard("{Escape}");
    expect(within(view.container).queryByRole("textbox", { name: label })).toBeNull();
    expect(closed).toEqual(["box"]);
    expect(escapes.presses.map((event) => event.defaultPrevented)).toEqual([true]);

    await user.keyboard("{Escape}");
    expect(closed).toEqual(["box", "sheet"]);
  } finally {
    escapes.stop();
    undraw(view);
  }
}

// An editor's Escape is the editor's: it closes the box, and Details, a sheet on a phone that
// closes on Escape too, stays open around it.
test("Escape in the route box closes the box and leaves Details open", async () => {
  await escapeInFieldBox("route", PROPERTY_LABELS.route);
});

test("Escape in the parent box closes the box and leaves Details open", async () => {
  await escapeInFieldBox("parent", PROPERTY_LABELS.parent);
});

/** An editor's two buttons in a form of its own, whose save is out from a submit until `saved`
 *  settles: `saving` and `waiting` hold then, as an editor that draws its refusal in itself holds
 *  them. `onSubmit` and `onCancel` hear each submit of the form and each cancel. */
function HeldSaveForm({
  onCancel,
  onSubmit,
  saved,
}: {
  onCancel: () => void;
  onSubmit: () => void;
  saved: Promise<void>;
}): ReactNode {
  const [out, setOut] = useState(false);
  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        onSubmit();
        setOut(true);
        void saved.then(() => setOut(false));
      }}
    >
      <input aria-label={PROPERTY_LABELS.route} />
      <FormActions
        cancel={onCancel}
        saveLabel={CONTROL_LABELS.save_route}
        saving={out}
        waiting={out}
      />
    </form>
  );
}

// An editor's Save and Cancel refuse a press with `aria-disabled` while its save is out, never
// `disabled`, which drops the focus to the body in a browser: Save, pressed by Enter, keeps the
// focus, and neither Enter on it again nor Enter in the box, which clicks it too, submits again;
// Cancel, while the editor waits for that save, keeps the focus as well and cancels nothing. Once
// the save lands both take a press again. The editors are drawn only on the new issue page, so
// the case is drawn inside `NoTooltips` alone.
test("inside NoTooltips an editor's Save and Cancel keep the focus while its save is out", async () => {
  const user = userEvent.setup();
  const unemulate = emulateChromiumFocusFixup();
  const saved = Promise.withResolvers<void>();
  let submits = 0;
  let cancels = 0;
  const view = draw(
    <HeldSaveForm
      onCancel={() => {
        cancels += 1;
      }}
      onSubmit={() => {
        submits += 1;
      }}
      saved={saved.promise}
    />,
    "inside"
  );
  try {
    const scope = within(view.container);
    const save = scope.getByRole("button", { name: CONTROL_LABELS.save_route });
    const cancel = scope.getByRole("button", { name: CONTROL_LABELS.cancel });
    const box = scope.getByRole("textbox", { name: PROPERTY_LABELS.route });
    save.focus();
    await user.keyboard("{Enter}");
    await settleReads();
    expect(submits).toBe(1);
    expectRefusedHoldingFocus(save);
    await user.keyboard("{Enter}");
    await settleReads();
    expect(submits).toBe(1);
    expectRefusedHoldingFocus(save);

    box.focus();
    await user.keyboard("{Enter}");
    await settleReads();
    expect(submits).toBe(1);
    expect(document.activeElement).toBe(box);

    cancel.focus();
    expectRefusedHoldingFocus(cancel);
    await user.keyboard("{Enter}");
    await settleReads();
    expect(cancels).toBe(0);
    expectRefusedHoldingFocus(cancel);

    saved.resolve();
    await waitFor(() => expect(cancel.hasAttribute("aria-disabled")).toBe(false));
    expect(save.hasAttribute("aria-disabled")).toBe(false);
    await user.keyboard("{Enter}");
    expect(cancels).toBe(1);
  } finally {
    saved.resolve();
    undraw(view);
    unemulate();
  }
});

/** An AskCard's queries, as a case reads them (`within`'s). */
type CardScope = BoundFunctions<typeof queries>;

/** The reader's answer to `ask`, as Dispatch answers the write that `sent` it. */
function answeredWith(ask: Ask, sent: AnswerAskInput | undefined): Ask {
  return {
    ...ask,
    answer: {
      at: "2026-10-08T02:00:00Z",
      selected: sent?.selected ?? null,
      text: sent?.text ?? null,
      user: "alice",
    },
    state: "answered",
  };
}

/** The reader's answer to the case's question, recorded, which a card drawn to change it seeds
 *  its form with. */
const RECORDED_ANSWER: Partial<Ask> = {
  answer: { at: "2026-10-08T01:00:00Z", selected: ["Ship"], text: "First note", user: "alice" },
  state: "answered",
};

/** A control of an open AskCard that its answer or its clarification refuses while that write is
 *  out, and how a reader reaches it by the keyboard. */
interface AskCardControl {
  /** The control, as its case names it. */
  readonly name: string;
  /** What the card's question holds besides `openAsk`'s. */
  readonly ask?: Partial<Ask>;
  /** Whether the card is drawn changing the reader's recorded answer (`mode="change"`). */
  readonly changing?: boolean;
  /** The write the press sends. */
  readonly write: "answer" | "clarification";
  /** Brings the card to the press, sends the write by the keyboard, and answers the control,
   *  which inside `NoTooltips` holds the focus then. */
  readonly press: (scope: CardScope, user: UserEvent, surface: Surface) => Promise<HTMLElement>;
  /** Presses the control again, as a reader would while its write is out. */
  readonly again: (scope: CardScope, user: UserEvent, control: HTMLElement) => Promise<void>;
}

function answerBoxOf(scope: CardScope): HTMLTextAreaElement {
  return scope.getByRole("textbox", { name: "Your answer" }) as HTMLTextAreaElement;
}

function radioOf(scope: CardScope, name: string): HTMLInputElement {
  return scope.getByRole("radio", { name }) as HTMLInputElement;
}

/** Types a question-shaped answer and sends it from the box, which offers to ask it back
 *  instead; answers the choice's two actions once drawn. */
async function offerToAskBack(
  scope: CardScope,
  user: UserEvent
): Promise<{ anyway: HTMLElement; instead: HTMLElement }> {
  await user.type(answerBoxOf(scope), "Which path is safer?");
  await user.keyboard("{Control>}{Enter}{/Control}");
  const instead = await scope.findByRole("button", { name: "Ask back instead" });
  // Ask back instead takes the focus as it is drawn, on both sides of `NoTooltips`.
  expect(document.activeElement).toBe(instead);
  return { anyway: scope.getByRole("button", { name: "Answer with it anyway" }), instead };
}

async function pressEnter(_scope: CardScope, user: UserEvent): Promise<void> {
  await user.keyboard("{Enter}");
}

/** Presses Enter in an option, which submits the form as a browser's implicit submission does,
 *  and picks the option `other` with a click that moves no focus: neither changes the pick. */
async function pressOptionAgain(
  scope: CardScope,
  user: UserEvent,
  control: HTMLElement,
  other: string
): Promise<void> {
  await user.keyboard("{Enter}");
  const picked = control as HTMLInputElement;
  radioOf(scope, other).click();
  await settleReads();
  expect(picked.checked).toBe(true);
  expect(radioOf(scope, other).checked).toBe(false);
}

const ASK_CARD_CONTROLS: readonly AskCardControl[] = [
  {
    again: async (scope, user, control) => {
      await user.keyboard("more");
      expect(answerBoxOf(scope).value).toBe("Ship it after the freeze");
      await user.keyboard("{Control>}{Enter}{/Control}");
      expect(document.activeElement).toBe(control);
    },
    ask: { options: [] },
    name: "answer box",
    press: async (scope, user) => {
      const box = answerBoxOf(scope);
      await user.type(box, "Ship it after the freeze");
      await user.keyboard("{Control>}{Enter}{/Control}");
      return box;
    },
    write: "answer",
  },
  {
    again: pressEnter,
    name: "Answer",
    press: async (scope, user) => {
      await user.click(radioOf(scope, "Ship"));
      const answer = scope.getByRole("button", { name: "Answer" });
      answer.focus();
      await user.keyboard("{Enter}");
      return answer;
    },
    write: "answer",
  },
  {
    again: pressEnter,
    ask: RECORDED_ANSWER,
    changing: true,
    name: "Save answer",
    press: async (scope, user) => {
      const save = await scope.findByRole("button", { name: "Save answer" });
      save.focus();
      await user.keyboard("{Enter}");
      return save;
    },
    write: "answer",
  },
  {
    again: pressEnter,
    ask: RECORDED_ANSWER,
    changing: true,
    name: "Cancel of a change",
    // Save answer sends the change, and the reader moves to Cancel while it is out: outside
    // `NoTooltips` Cancel is `disabled` then, and takes no focus.
    press: async (scope, user, surface) => {
      const save = await scope.findByRole("button", { name: "Save answer" });
      const cancel = scope.getByRole("button", { name: "Cancel" });
      save.focus();
      await user.keyboard("{Enter}");
      if (surface === "inside") cancel.focus();
      return cancel;
    },
    write: "answer",
  },
  {
    again: pressEnter,
    name: "Ask back",
    press: async (scope, user) => {
      await user.type(answerBoxOf(scope), "Stage it before the release");
      const askBack = scope.getByRole("button", { name: "Ask back" });
      askBack.focus();
      await user.keyboard("{Enter}");
      return askBack;
    },
    write: "clarification",
  },
  {
    again: pressEnter,
    name: "Ask back instead",
    press: async (scope, user) => {
      const { instead } = await offerToAskBack(scope, user);
      await user.keyboard("{Enter}");
      return instead;
    },
    write: "clarification",
  },
  {
    again: pressEnter,
    name: "Answer with it anyway",
    press: async (scope, user) => {
      const { anyway } = await offerToAskBack(scope, user);
      anyway.focus();
      await user.keyboard("{Enter}");
      return anyway;
    },
    write: "answer",
  },
  {
    again: (scope, user, control) => pressOptionAgain(scope, user, control, "Hold"),
    name: "option",
    press: async (scope, user) => {
      const ship = radioOf(scope, "Ship");
      await user.click(ship);
      await user.keyboard("{Enter}");
      return ship;
    },
    write: "answer",
  },
  {
    again: (scope, user, control) => pressOptionAgain(scope, user, control, "Ship"),
    name: "Other",
    press: async (scope, user) => {
      const other = radioOf(scope, "Other");
      await user.click(other);
      await user.type(answerBoxOf(scope), "Stage it first");
      other.focus();
      await user.keyboard("{Enter}");
      return other;
    },
    write: "answer",
  },
];

/** Inside `NoTooltips`, the AskCard control holds the focus and is refused without `disabled`:
 *  the answer box is `readOnly`, an option says it by its group's `aria-disabled`, and every
 *  other control by its own. */
function expectAskControlRefusedHoldingFocus(control: HTMLElement): void {
  if (control instanceof HTMLTextAreaElement) {
    expect(document.activeElement).toBe(control);
    expect(control.hasAttribute("readonly")).toBe(true);
    expect(control.hasAttribute("disabled")).toBe(false);
    return;
  }
  if (control instanceof HTMLInputElement) {
    expect(document.activeElement).toBe(control);
    expect(control.closest("fieldset")?.getAttribute("aria-disabled")).toBe("true");
    expect(control.hasAttribute("disabled")).toBe(false);
    return;
  }
  expectRefusedHoldingFocus(control);
}

/** Outside `NoTooltips`, the AskCard control is `disabled` alone, as on `main`. */
function expectAskControlDisabledAsOnMain(control: HTMLElement): void {
  expectDisabledAsOnMain(control);
  expect(control.hasAttribute("readonly")).toBe(false);
  expect(control.closest("fieldset")?.hasAttribute("aria-disabled") ?? false).toBe(false);
}

/** Once its write has failed, the AskCard control is still drawn and takes a press again. */
function expectAskControlFree(control: HTMLElement): void {
  expect(control.isConnected).toBe(true);
  expect(control.hasAttribute("disabled")).toBe(false);
  expect(control.hasAttribute("aria-disabled")).toBe(false);
  expect(control.hasAttribute("readonly")).toBe(false);
  expect(control.closest("fieldset")?.hasAttribute("aria-disabled") ?? false).toBe(false);
}

/** `ask`'s card in a row the focus can land on, as the Inbox draws its row: the element the
 *  card's own hand-off gives the focus to when a control holding it leaves the page. Each read of
 *  the ask's thread is answered at once with the open ask, unless the case reads it itself. */
function AskRow({
  ask,
  changing = false,
  getAskThread = async () => threadOf(ask),
}: {
  ask: Ask;
  changing?: boolean;
  getAskThread?: (id: string) => Promise<AskRead>;
}): ReactNode {
  return (
    <div data-testid="ask-row" tabIndex={-1}>
      <AskCard
        ask={ask}
        getAskThread={getAskThread}
        initialThread={threadOf(ask)}
        mode={changing ? "change" : "answer"}
      />
    </div>
  );
}

// While the card's answer or its clarification is out, every control of its form is refused.
// Inside `NoTooltips` each is `aria-disabled` in place of `disabled`, which drops the focus to the
// body in a browser (the answer box is `readOnly`, an option is refused by its group): the one
// the reader pressed keeps the focus, and a press of it sends no second write and changes
// nothing. Once the write lands, and inside `NoTooltips` once the read of the ask that a
// clarification starts is drawn as well, what it takes off the page - the form, which the answer
// replaces with its record, or the choice a question-shaped answer offers - hands the focus on as
// it goes, to the row the card is drawn in, and a clarification that empties the box leaves Ask
// back `disabled`, which hands it on too; once the write fails, the pressed control is still drawn
// and kept the focus all along. Outside `NoTooltips` each is `disabled` while the write is out, as
// on `main`, and the card hands the focus to the row as it disables the control.
for (const control of ASK_CARD_CONTROLS) {
  test(`inside NoTooltips AskCard's ${control.name} keeps the focus while its write is out, then the card hands it to the row`, async () => {
    const user = userEvent.setup();
    for (const surface of ["inside", "outside"] as const) {
      for (const write of ["lands", "fails"] as const) {
        const unemulate = emulateChromiumFocusFixup();
        const ask = openAsk({ ...control.ask, id: `ask-${surface}-${write}` });
        const answered = Promise.withResolvers<Ask>();
        const answerAsk = spyOn(api, "answerAsk").mockImplementation(() => answered.promise);
        const said = Promise.withResolvers<Comment>();
        const createComment = spyOn(api, "createComment").mockImplementation(() => said.promise);
        const sent = control.write === "answer" ? answerAsk : createComment;
        const view = draw(<AskRow ask={ask} changing={control.changing} />, surface);
        try {
          const scope = within(view.container);
          const row = scope.getByTestId("ask-row");
          await settleReads();
          const pressed = await control.press(scope, user, surface);
          await waitFor(() => expect(sent).toHaveBeenCalledTimes(1));
          await settleReads();
          if (surface === "inside") {
            expectAskControlRefusedHoldingFocus(pressed);
            await control.again(scope, user, pressed);
            await settleReads();
            expect(sent).toHaveBeenCalledTimes(1);
            expectAskControlRefusedHoldingFocus(pressed);
          } else {
            expectAskControlDisabledAsOnMain(pressed);
            expect(document.activeElement).toBe(row);
          }

          if (write === "fails") {
            const failure = new Error("the connection dropped");
            if (control.write === "answer") answered.reject(failure);
            else said.reject(failure);
            await scope.findByRole("alert");
            await settleReads();
            expectAskControlFree(pressed);
            if (surface === "inside") expect(document.activeElement).toBe(pressed);
            continue;
          }
          if (control.write === "answer") {
            answered.resolve(answeredWith(ask, answerAsk.mock.calls[0]?.[1]));
            await waitFor(() =>
              expect(view.container.querySelector("[data-ask-answer]")).toBeNull()
            );
          } else {
            said.resolve({} as Comment);
            await waitFor(() => expect(answerBoxOf(scope).value).toBe(""));
          }
          await settleReads();
          expect(document.activeElement).toBe(row);
          expect(sent).toHaveBeenCalledTimes(1);
        } finally {
          answerAsk.mockRestore();
          createComment.mockRestore();
          undraw(view);
          unemulate();
        }
      }
    }
  });
}

// The card hands off a focus its form holds alone. A reader who moves on while Answer's write is
// out keeps the focus where they put it once the answer lands and the form leaves the page, on
// both sides of `NoTooltips`.
test("inside NoTooltips AskCard's Answer leaves a focus the reader moved elsewhere where it is", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const ask = openAsk({ id: `ask-${surface}` });
    const answered = Promise.withResolvers<Ask>();
    const answerAsk = spyOn(api, "answerAsk").mockImplementation(() => answered.promise);
    const view = draw(
      <>
        <AskRow ask={ask} />
        <button type="button">Elsewhere</button>
      </>,
      surface
    );
    try {
      const scope = within(view.container);
      await settleReads();
      await user.click(radioOf(scope, "Ship"));
      const answer = scope.getByRole("button", { name: "Answer" });
      answer.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
      const elsewhere = scope.getByRole("button", { name: "Elsewhere" });
      elsewhere.focus();

      answered.resolve(answeredWith(ask, answerAsk.mock.calls[0]?.[1]));
      await waitFor(() => expect(view.container.querySelector("[data-ask-answer]")).toBeNull());
      await settleReads();
      expect(answer.isConnected).toBe(false);
      expect(document.activeElement).toBe(elsewhere);
      expect(answerAsk).toHaveBeenCalledTimes(1);
    } finally {
      answerAsk.mockRestore();
      undraw(view);
      unemulate();
    }
  }
});

/** The reads of an ask's thread a case answers itself (`read`): until `landHeld` holds `read`,
 *  each answers the open ask's read at once. */
interface AskReads {
  readonly read: Mock<() => Promise<AskRead>>;
}

function askReads(ask: Ask): AskReads {
  return { read: mock(async () => threadOf(ask)) };
}

/** Each element the focus lands on from `start` until `stop`. */
interface FocusMoves {
  readonly moved: EventTarget[];
  readonly start: () => void;
  readonly stop: () => void;
}

function recordFocusMoves(): FocusMoves {
  const moved: EventTarget[] = [];
  const record = (event: FocusEvent) => {
    if (event.target !== null) moved.push(event.target);
  };
  return {
    moved,
    start: () => document.addEventListener("focusin", record),
    stop: () => document.removeEventListener("focusin", record),
  };
}

/** Once the ask is read again, the focus moved once, to `row`, inside `NoTooltips`, and not at
 *  all outside it, where it was on the row already. */
function expectOneMoveToRow(focus: FocusMoves, row: HTMLElement, surface: Surface): void {
  expect(document.activeElement).toBe(row);
  expect(focus.moved).toHaveLength(surface === "inside" ? 1 : 0);
  if (surface === "inside") expect(focus.moved[0]).toBe(row);
}

// An answer Dispatch made and lost its answer to (`409 KEY_COMMITTED`) lands with no body: the
// card has no record to draw, so it reads the ask again, and until that read is drawn the form
// stays on the page with its option still picked. Inside `NoTooltips` the card stays refused until
// then: Answer keeps the focus and is `aria-disabled`, and a press of it sends no second answer,
// which Dispatch would refuse, the ask being closed. Once the read draws the recorded answer, the
// form leaves the page and hands the focus to the row, the one move of the focus. Outside
// `NoTooltips` Answer is `disabled` while the write is out, and the focus goes to the row then,
// as on `main`.
test("inside NoTooltips an answer that lands with no answer body keeps the ask card refused until the ask is read again", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const focus = recordFocusMoves();
    const ask = openAsk({ id: `ask-${surface}` });
    const reads = askReads(ask);
    const answered = Promise.withResolvers<Ask>();
    const answerAsk = spyOn(api, "answerAsk").mockImplementation(() => answered.promise);
    const view = draw(<AskRow ask={ask} getAskThread={reads.read} />, surface);
    try {
      const scope = within(view.container);
      const row = scope.getByTestId("ask-row");
      await settleReads();
      await user.click(radioOf(scope, "Ship"));
      const answer = scope.getByRole("button", { name: "Answer" });
      answer.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(1));
      await settleReads();
      if (surface === "inside") {
        expectRefusedHoldingFocus(answer);
      } else {
        expectAskControlDisabledAsOnMain(answer);
        expect(document.activeElement).toBe(row);
      }

      // The answer lands with no body, and the read of the ask after it is held.
      focus.start();
      const answerReread = await landHeld(reads.read, () =>
        answered.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "made" }))
      );
      expect(answer.isConnected).toBe(true);
      expect(radioOf(scope, "Ship").checked).toBe(true);
      if (surface === "inside") {
        // A press while the read is out sends no second answer, and Answer is still refused.
        await user.keyboard("{Enter}");
        await settleReads();
        expect(answerAsk).toHaveBeenCalledTimes(1);
        expectRefusedHoldingFocus(answer);
      } else {
        // The write stopped pending as it landed, before its read.
        expect(answer.hasAttribute("disabled")).toBe(false);
        expect(answer.hasAttribute("aria-disabled")).toBe(false);
        expect(document.activeElement).toBe(row);
      }
      expect(focus.moved).toHaveLength(0);

      answerReread(threadOf(answeredWith(ask, answerAsk.mock.calls[0]?.[1])));
      await scope.findByText(/Answered by/);
      await settleReads();
      expect(answer.isConnected).toBe(false);
      expectOneMoveToRow(focus, row, surface);
      expect(answerAsk).toHaveBeenCalledTimes(1);
    } finally {
      answerAsk.mockRestore();
      focus.stop();
      undraw(view);
      unemulate();
    }
  }
});

// A clarification empties the box as it lands and reads the ask again. Inside `NoTooltips`, until
// that read is drawn, Ask back is drawn as it stood at the press: refused with `aria-disabled`, and
// not `disabled` for the text the landing took, which would drop the focus in a browser; it keeps
// the focus, and a press of it sends nothing. Once the read is drawn Ask back, with no text to
// send, is `disabled`, and the card hands the focus to the row, the one move of the focus. Outside
// `NoTooltips` Ask back is `disabled` while the write is out, and the focus goes to the row then,
// as on `main`.
test("inside NoTooltips Ask back keeps the focus until the ask is read again", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const focus = recordFocusMoves();
    const ask = openAsk({ id: `ask-${surface}` });
    const reads = askReads(ask);
    const said = Promise.withResolvers<Comment>();
    const createComment = spyOn(api, "createComment").mockImplementation(() => said.promise);
    const view = draw(<AskRow ask={ask} getAskThread={reads.read} />, surface);
    try {
      const scope = within(view.container);
      const row = scope.getByTestId("ask-row");
      await settleReads();
      await user.type(answerBoxOf(scope), "Stage it before the release");
      const askBack = scope.getByRole("button", { name: "Ask back" });
      askBack.focus();
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
      await settleReads();
      if (surface === "inside") {
        expectRefusedHoldingFocus(askBack);
      } else {
        expectAskControlDisabledAsOnMain(askBack);
        expect(document.activeElement).toBe(row);
      }

      // The clarification lands and empties the box, and the read of the ask after it is held.
      focus.start();
      const answerReread = await landHeld(reads.read, () => said.resolve({} as Comment));
      expect(answerBoxOf(scope).value).toBe("");
      if (surface === "inside") {
        expectRefusedHoldingFocus(askBack);
        await user.keyboard("{Enter}");
        await settleReads();
        expect(createComment).toHaveBeenCalledTimes(1);
        expectRefusedHoldingFocus(askBack);
      } else {
        expectAskControlDisabledAsOnMain(askBack);
        expect(document.activeElement).toBe(row);
      }
      expect(focus.moved).toHaveLength(0);

      answerReread(threadOf(ask));
      await waitFor(() => expect(askBack.hasAttribute("disabled")).toBe(true));
      await settleReads();
      expect(askBack.hasAttribute("aria-disabled")).toBe(false);
      expectOneMoveToRow(focus, row, surface);
      expect(createComment).toHaveBeenCalledTimes(1);
    } finally {
      createComment.mockRestore();
      focus.stop();
      undraw(view);
      unemulate();
    }
  }
});

// A clarification sent by Ask back instead closes the choice a question-shaped answer offers as
// it lands, and reads the ask again. Inside `NoTooltips`, until that read is drawn, the choice
// stays on the page with both its actions refused: Ask back instead keeps the focus and a press of
// it sends nothing. Once the read is drawn the choice leaves the page and the card hands the focus
// to the row, the one move of the focus. Outside `NoTooltips` Ask back instead is `disabled` while
// the write is out, and the focus goes to the row then; the choice leaves as the clarification
// lands, as on `main`.
test("inside NoTooltips Ask back instead keeps the focus until the ask is read again", async () => {
  const user = userEvent.setup();
  for (const surface of ["inside", "outside"] as const) {
    const unemulate = emulateChromiumFocusFixup();
    const focus = recordFocusMoves();
    const ask = openAsk({ id: `ask-${surface}` });
    const reads = askReads(ask);
    const said = Promise.withResolvers<Comment>();
    const createComment = spyOn(api, "createComment").mockImplementation(() => said.promise);
    const view = draw(<AskRow ask={ask} getAskThread={reads.read} />, surface);
    try {
      const scope = within(view.container);
      const row = scope.getByTestId("ask-row");
      await settleReads();
      const { anyway, instead } = await offerToAskBack(scope, user);
      await user.keyboard("{Enter}");
      await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
      await settleReads();
      if (surface === "inside") {
        expectRefusedHoldingFocus(instead);
      } else {
        expectAskControlDisabledAsOnMain(instead);
        expect(document.activeElement).toBe(row);
      }

      // The clarification lands and closes the choice, and the read of the ask after it is held.
      focus.start();
      const answerReread = await landHeld(reads.read, () => said.resolve({} as Comment));
      expect(answerBoxOf(scope).value).toBe("");
      if (surface === "inside") {
        expect(anyway.isConnected).toBe(true);
        expect(anyway.getAttribute("aria-disabled")).toBe("true");
        expect(anyway.hasAttribute("disabled")).toBe(false);
        expectRefusedHoldingFocus(instead);
        await user.keyboard("{Enter}");
        await settleReads();
        expect(createComment).toHaveBeenCalledTimes(1);
        expectRefusedHoldingFocus(instead);
      } else {
        expect(instead.isConnected).toBe(false);
        expect(document.activeElement).toBe(row);
      }
      expect(focus.moved).toHaveLength(0);

      answerReread(threadOf(ask));
      await waitFor(() => expect(instead.isConnected).toBe(false));
      await settleReads();
      expect(anyway.isConnected).toBe(false);
      expectOneMoveToRow(focus, row, surface);
      expect(createComment).toHaveBeenCalledTimes(1);
    } finally {
      createComment.mockRestore();
      focus.stop();
      undraw(view);
      unemulate();
    }
  }
});
