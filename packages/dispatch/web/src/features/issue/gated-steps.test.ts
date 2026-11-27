import { beforeAll, expect, test } from "bun:test";
import type { ApprovalGateLine } from "@legion/contracts";

import type { Artifact, IssueDetails } from "../../api/types";
import { issue as fixture, specArtifact } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { approvalGateSteps } from "./gated-steps";

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

/** The issue with both documents and the gate the server answered for it. */
function gated(...approval_gate: ApprovalGateLine[]): IssueDetails {
  return { ...fixture, approval_gate, artifacts: [specArtifact, verificationDoc] };
}

const spec: ApprovalGateLine = {
  approval: { latest_version: 1, state: "awaiting" },
  artifact_id: "artifact-1",
  step: "spec",
};
const criteria: ApprovalGateLine = {
  approval: { latest_version: 1, state: "draft" },
  artifact_id: "artifact-3",
  step: "criteria",
};

test("an issue read with no gate draws no step", () => {
  expect(approvalGateSteps({ ...fixture, artifacts: [specArtifact, verificationDoc] })).toEqual([]);
  expect(approvalGateSteps(undefined)).toEqual([]);
});

test("a gate of one line with no step - every issue but a gated root - draws no step", () => {
  expect(approvalGateSteps(gated({ approval: spec.approval, artifact_id: "artifact-1" }))).toEqual(
    []
  );
});

test("the steps are drawn in the gate's order, each with the document its line names", () => {
  expect(approvalGateSteps(gated(spec, criteria))).toEqual([
    { document: specArtifact, label: "Spec", step: "spec" },
    { document: verificationDoc, label: "Verification", step: "criteria" },
  ]);
  expect(approvalGateSteps(gated(criteria, spec))).toEqual([
    { document: verificationDoc, label: "Verification", step: "criteria" },
    { document: specArtifact, label: "Spec", step: "spec" },
  ]);
});

test("a step whose line names no document is drawn with no document", () => {
  expect(
    approvalGateSteps(gated({ approval: null, artifact_id: null, step: "criteria" }, spec))
  ).toEqual([
    { document: undefined, label: "Verification", step: "criteria" },
    { document: specArtifact, label: "Spec", step: "spec" },
  ]);
});

test("a step word this build has no name for is left out and the rest keep their order", () => {
  expect(approvalGateSteps(gated(spec, { ...criteria, step: "thresholds" }, criteria))).toEqual([
    { document: specArtifact, label: "Spec", step: "spec" },
    { document: verificationDoc, label: "Verification", step: "criteria" },
  ]);
});
