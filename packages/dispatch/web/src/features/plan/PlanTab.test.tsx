import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { Artifact, ArtifactApproval, ArtifactBlock, GraphReferences } from "../../api/types";
import { PlanTab } from "./PlanTab";

/** The words a block reference resolves to, which is what a pointer's link shows. */
const BLOCK_TEXT: Record<string, string> = {
  "dispatch://CORE-1/artifact/plan-md#b-t0": "Add the routes.",
  "dispatch://CORE-1/artifact/plan-md#b-t1": "Draw the panel.",
  "dispatch://CORE-1/spec#b-r1": "Every issue page shows a Plan tab.",
};

function references(reference: string): GraphReferences {
  return {
    edges: [],
    node: graphNode({
      block_id: reference.split("#b-")[1] ?? "",
      id: reference.slice("dispatch://".length),
      issue_key: "CORE-1",
      kind: "block",
      ref: reference,
      text: BLOCK_TEXT[reference] ?? "",
    }),
  };
}

function plan(approval?: ArtifactApproval): Artifact {
  return {
    created_at: "2026-10-01T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-2",
    issue_key: "CORE-1",
    kind: "doc",
    name: "plan.md",
    primary: false,
    project: "CORE",
    role: "plan",
    ref_key: "CORE-1/plan-md",
    session_id: null,
    slug: "plan-md",
    versions: [],
    ...(approval === undefined ? {} : { approval }),
  };
}

const blocks: ArtifactBlock[] = [
  {
    from: 0,
    id: "p1",
    references: { asks: 0, comments: 0 },
    to: 4,
    token: "sha256:p1",
    type: "paragraph",
  },
  {
    // `serves` and `needs` are `block[]` attributes in the one block schema
    // (`pmdoc/schema/blocks.json`), each value a canonical block reference.
    attributes: [
      { kind: "block[]", name: "serves", value: "dispatch://CORE-1/spec#b-r1" },
      { kind: "block[]", name: "needs", value: "dispatch://CORE-1/artifact/plan-md#b-t0" },
    ],
    from: 5,
    id: "t1",
    references: { asks: 0, comments: 0 },
    to: 9,
    token: "sha256:t1",
    type: "plan_task",
  },
];

function renderTab(artifact: Artifact | undefined, artifactBlocks: ArtifactBlock[] = blocks) {
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue(artifactBlocks);
  const getReferences = spyOn(api, "getReferences").mockImplementation(async (reference: string) =>
    references(reference)
  );
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <PlanTab artifact={artifact} issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getArtifactBlocks, getReferences, view };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

test("a plan task shows the requirements it serves and the tasks it needs", async () => {
  const { getArtifactBlocks, getReferences, view } = renderTab(plan());
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  // Only the `plan_task` blocks are tasks; the plan's prose is the document's to draw.
  const tasks = await screen.findAllByTestId("plan-task");
  expect(tasks).toHaveLength(1);
  const task = within(tasks[0] as HTMLElement);
  await task.findByText("Draw the panel.");
  const serves = within(task.getByTestId("plan-task-serves"));
  expect(serves.getByRole("link", { name: "Every issue page shows a Plan tab." })).toBeTruthy();
  const needs = within(task.getByTestId("plan-task-needs"));
  expect(needs.getByRole("link", { name: "Add the routes." }).getAttribute("href")).toBe(
    "/issues/CORE-1/artifacts/plan-md#b-t0"
  );
});

test("a task that serves and needs nothing says so", async () => {
  const { getArtifactBlocks, getReferences, view } = renderTab(plan(), [
    {
      from: 0,
      id: "t9",
      references: { asks: 0, comments: 0 },
      to: 4,
      token: "sha256:t9",
      type: "plan_task",
    },
  ]);
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  const task = within(await screen.findByTestId("plan-task"));
  expect(task.getByTestId("plan-task-serves").textContent).toBe("Servesnone");
  expect(task.getByTestId("plan-task-needs").textContent).toBe("Needsnone");
});

// A pointer's link reads as the block's words through the label every reference link reads its
// text through (`useReferenceLabel`), and a block whose text is empty has no words: its link
// reads as the reference's short form, where an empty headline drew it as a link with no text
// and no size, which a reader could neither see nor click.
test("a pointer to a block whose text is empty reads as the reference's short form", async () => {
  const empty = "dispatch://CORE-1/artifact/plan-md#b-t8";
  const { getArtifactBlocks, getReferences, view } = renderTab(plan(), [
    {
      attributes: [
        { kind: "block[]", name: "serves", value: "dispatch://CORE-1/spec#b-r1" },
        { kind: "block[]", name: "needs", value: empty },
      ],
      from: 0,
      id: "t1",
      references: { asks: 0, comments: 0 },
      to: 4,
      token: "sha256:t1",
      type: "plan_task",
    },
  ]);
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  const task = within(await screen.findByTestId("plan-task"));
  // The serving block's words are drawn once its read lands, and the empty block's read lands
  // beside it.
  await within(task.getByTestId("plan-task-serves")).findByRole("link", {
    name: "Every issue page shows a Plan tab.",
  });
  expect(getReferences).toHaveBeenCalledWith(empty);
  const needs = within(task.getByTestId("plan-task-needs"));
  expect(needs.getByRole("link").textContent).toBe("CORE-1 plan-md block");
});

// A pointer names a block of a document, never a block of a comment or an issue message
// (`parseBlockPointerValue`, after Go's `pmdoc.validateBlockPointerValues`): a value naming one is
// no pointer at all, so the tab reads no post's block and prints none of its words. An edited or
// deleted post has nothing here to mark or to withhold.
test("a value naming a block of a comment or a message is no pointer, and the tab reads and prints no post", async () => {
  const comment = "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b1";
  const message = "dispatch://CORE-1/message/22222222-2222-4222-8222-222222222222#b-b1";
  const { getArtifactBlocks, getReferences, view } = renderTab(plan(), [
    {
      attributes: [
        { kind: "block[]", name: "serves", value: comment },
        { kind: "block[]", name: "needs", value: message },
      ],
      from: 0,
      id: "t1",
      references: { asks: 0, comments: 0 },
      to: 4,
      token: "sha256:t1",
      type: "plan_task",
    },
  ]);
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  const task = within(await screen.findByTestId("plan-task"));
  // The task's own block is read and drawn; the two values beside it are not.
  await task.findByRole("link", { name: "Draw the panel." });
  expect(task.getByTestId("plan-task-serves").textContent).toBe("Servesnone");
  expect(task.getByTestId("plan-task-needs").textContent).toBe("Needsnone");
  expect(getReferences).not.toHaveBeenCalledWith(comment);
  expect(getReferences).not.toHaveBeenCalledWith(message);
});

// A plan is approved by its plan review, never by a person, and the chip beside it says so.
test("the plan review's approval shows on the tab", async () => {
  const { getArtifactBlocks, getReferences, view } = renderTab(
    plan({ kind: "plan_review", latest_version: 2, state: "approved", version: 2 })
  );
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  expect(screen.getByTestId("plan-approval").textContent).toContain("Approved by plan review v2");
  await screen.findAllByTestId("plan-task");
});

test("a plan that marks no tasks says so", async () => {
  const { getArtifactBlocks, getReferences, view } = renderTab(plan(), [
    {
      from: 0,
      id: "p1",
      references: { asks: 0, comments: 0 },
      to: 4,
      token: "sha256:p1",
      type: "paragraph",
    },
  ]);
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  expect((await screen.findByTestId("plan-no-tasks")).textContent).toContain(
    "This plan marks no tasks."
  );
});

test("an issue with no plan document shows the command that makes one", () => {
  const { getArtifactBlocks, getReferences, view } = renderTab(undefined);
  restore = () => {
    getArtifactBlocks.mockRestore();
    getReferences.mockRestore();
    view.unmount();
  };

  const empty = screen.getByTestId("plan-empty");
  expect(empty.textContent).toContain("CORE-1 has no plan document.");
  expect(empty.textContent).toContain("dispatch artifact --issue CORE-1 --name plan.md");
  // The note's words are asserted once, in `ArtifactUpload.test.tsx`, where they live: this
  // case asks only that the tab shows the note.
  expect(within(empty).getByTestId("replacing-upload-note")).not.toBeNull();
  expect(getArtifactBlocks).not.toHaveBeenCalled();
});
