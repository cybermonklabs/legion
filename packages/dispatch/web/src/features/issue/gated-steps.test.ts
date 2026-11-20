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

/** A root issue on a Dispatch older than document roles: it answers no Verification document id
 *  at all, the way an older server leaves the field off the wire. */
const { verification_artifact_id: _unroled, ...unroledFixture } = fixture;
const olderRoot = {
  ...unroledFixture,
  artifacts: [specArtifact, verificationDoc],
} as IssueDetails;

const root: IssueDetails = { ...olderRoot, verification_artifact_id: "artifact-3" };

const ungated: Project = {
  key: "CORE",
  name: "Core",
  created_at: "2026-09-09T00:00:00Z",
  open_asks: 0,
  gated_steps: [],
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

test("a step the issue holds no document for is drawn with no document", () => {
  const none: IssueDetails = { ...root, verification_artifact_id: "" };

  expect(gatedStepEntries(gating("criteria"), none)).toEqual([
    { step: "criteria", label: "Verification", document: undefined },
  ]);
});

test("a step an older server answers no id for at all is drawn with no document", () => {
  expect(gatedStepEntries(gating("criteria"), olderRoot)).toEqual([
    { step: "criteria", label: "Verification", document: undefined },
  ]);
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
