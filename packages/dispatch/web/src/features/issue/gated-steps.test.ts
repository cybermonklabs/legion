import { beforeAll, expect, test } from "bun:test";

import type { Artifact, GatedStep, IssueDetails, Project } from "../../api/types";
import { issue as fixture, specArtifact } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { gatedStepEntries } from "./gated-steps";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const verificationDoc: Artifact = {
  ...specArtifact,
  id: "artifact-3",
  name: "verification.md",
  slug: "verification",
  primary: false,
  role: "verification",
};

/** A root issue as a server of the release before answers it: no `queries`, and the Verification
 *  document named in `verification_artifact_id`. */
const before: IssueDetails = {
  ...fixture,
  artifacts: [specArtifact, verificationDoc],
  verification_artifact_id: verificationDoc.id,
};

/** The same issue as this build answers it: its verification query names the document. */
const root: IssueDetails = {
  ...before,
  queries: [
    {
      query: "verification",
      artifact_id: verificationDoc.id,
      takes: ["check", "result"],
      subtree: `issue:${fixture.key}`,
    },
  ],
};

const ungated: Project = {
  key: "CORE",
  name: "Core",
  created_at: "2026-09-09T00:00:00Z",
  open_asks: 0,
  gated_steps: [],
  proven_sessions: false,
};

function gating(...gated_steps: GatedStep[]): Project {
  return { ...ungated, gated_steps };
}

test("a project whose deployment published no gated steps draws none", () => {
  expect(gatedStepEntries(ungated, root)).toEqual([]);
});

test("a project that gates nothing draws none", () => {
  expect(gatedStepEntries(gating(), root)).toEqual([]);
});

test("an issue that is not its tree's root draws none, whatever its project gates", () => {
  const child: IssueDetails = { ...root, parent: "CORE-9" };

  expect(gatedStepEntries(gating("criteria", "spec"), child)).toEqual([]);
});

test("an issue whose parent is in another project is a root of its own project and draws its steps", () => {
  // A parent of another project passes nothing on, so the issue stands in its own project's
  // gate as a root does; only a parent of the issue's own project makes it a child.
  const rootUnderAnotherProject: IssueDetails = { ...root, parent: "OTHR-6" };

  expect(
    gatedStepEntries(gating("criteria", "spec"), rootUnderAnotherProject).map((entry) => entry.step)
  ).toEqual(["criteria", "spec"]);
  expect(gatedStepEntries(gating("criteria", "spec"), { ...root, parent: "CORE-9" })).toEqual([]);
});

test("neither the unread project nor the unread issue draws a step", () => {
  expect(gatedStepEntries(undefined, root)).toEqual([]);
  expect(gatedStepEntries(gating("spec"), undefined)).toEqual([]);
});

test("the steps are drawn in the order the deployment published them, not the enumeration's", () => {
  expect(gatedStepEntries(gating("spec", "criteria"), root)).toEqual([
    { step: "spec", label: "Spec", document: specArtifact },
    { step: "criteria", label: "Verification", document: verificationDoc },
  ]);
});

test("each step names its own document, the Verification document among them", () => {
  expect(gatedStepEntries(gating("criteria", "spec"), root)).toEqual([
    { step: "criteria", label: "Verification", document: verificationDoc },
    { step: "spec", label: "Spec", document: specArtifact },
  ]);
});

// The queries decide alone where the issue read carries them: an issue with no verification
// query has no Verification document, whatever `verification_artifact_id` names.
test("a step the issue holds no document for is drawn with no document", () => {
  const none: IssueDetails = { ...root, queries: [] };

  expect(gatedStepEntries(gating("criteria"), none)).toEqual([
    { step: "criteria", label: "Verification", document: undefined },
  ]);
});

// During a roll or after a rollback a server of the release before answers the issue read: it
// carries no `queries`, and the step still names the document `verification_artifact_id` names.
test("a step of an issue read with no queries names the document its kept id names", () => {
  expect(gatedStepEntries(gating("criteria"), before)).toEqual([
    { step: "criteria", label: "Verification", document: verificationDoc },
  ]);
  expect(gatedStepEntries(gating("criteria"), { ...before, verification_artifact_id: "" })).toEqual(
    [{ step: "criteria", label: "Verification", document: undefined }]
  );
});

test("a step word this build has no name for is left out and the rest keep their order", () => {
  // A Dispatch newer than this build may gate another step; the header draws what it knows.
  const newer: Project = {
    ...ungated,
    gated_steps: ["spec", "thresholds", "criteria"] as unknown as GatedStep[],
  };

  expect(gatedStepEntries(newer, root)).toEqual([
    { step: "spec", label: "Spec", document: specArtifact },
    { step: "criteria", label: "Verification", document: verificationDoc },
  ]);
});

// A Dispatch of the release before may still answer the retired `decisions` step in a project's
// list; the header draws no step for it and asks for no document by that role.
test("the retired decisions step is left out and the rest keep their order", () => {
  expect(gatedStepEntries(gating("decisions", "criteria", "spec"), root)).toEqual([
    { step: "criteria", label: "Verification", document: verificationDoc },
    { step: "spec", label: "Spec", document: specArtifact },
  ]);
});
