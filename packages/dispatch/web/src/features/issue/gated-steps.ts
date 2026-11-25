import { GATED_STEPS } from "@legion/contracts/legion-roles";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import type { ArtifactWithTrees, GatedStep, IssueDetails, Project } from "../../api/types";
import { issueProject } from "../../lib/issue-key";

/** What the page calls the document each design-gate step reviews: Dispatch's own name for that
 *  document, so a reader is told which document waits rather than the enumeration's word for the
 *  step (`criteria` names the Verification document). */
export const STEP_LABELS: Record<GatedStep, string> = {
  decisions: "Decisions",
  criteria: "Verification",
  spec: "Spec",
};

/** One design-gate step of a root issue: the step, the name the page draws for it, and the
 *  document it reviews - undefined for an issue that holds no document for the step, which is
 *  the step nobody has written yet. The document is the issue details' own row, so its approval
 *  carries the tree of its review reason. */
export interface StepEntry {
  step: GatedStep;
  label: string;
  document: ArtifactWithTrees | undefined;
}

/** The document the issue holds for `step`, by the id the issue names for it, never by the
 *  document's role or name among `issue.artifacts`. */
function stepDocument(issue: IssueDetails, step: GatedStep): ArtifactWithTrees | undefined {
  switch (step) {
    case "spec":
      return primarySpec(issue);
    case "criteria":
      return roleDocument(issue, "verification");
    case "decisions":
      return roleDocument(issue, "decisions");
  }
}

/** Whether the issue is a root of its own project, the issue that stands in the design gate: it
 *  has no parent, or its parent is an issue of another project (`issueProject`, the one
 *  key-to-project rule), which passes nothing on, so the issue is its own project's root. */
export function isProjectRoot(issue: { parent: string | null; project: string }): boolean {
  return issue.parent === null || issueProject(issue.parent) !== issue.project;
}

/**
 * The design-gate steps the issue's header draws, one for each step the project's deployment
 * gates, in the order the deployment published them. It is empty - which leaves the header its
 * one chip for the issue's primary document - until the project and the issue are both read, for
 * a project whose deployment gates nothing, and for an issue that is not a root of its project
 * (`isProjectRoot`), since only a root issue stands in the gate.
 *
 * A step word this build does not know is left out rather than drawn without a name: a Dispatch
 * newer than this build may gate a fourth step, and the header draws the steps it knows.
 */
export function gatedStepEntries(
  project: Project | undefined,
  issue: IssueDetails | undefined
): StepEntry[] {
  const steps = project?.gated_steps;
  if (steps === undefined || steps === null || steps.length === 0) {
    return [];
  }
  if (issue === undefined || !isProjectRoot(issue)) {
    return [];
  }
  return steps
    .filter(isGatedStep)
    .map((step) => ({ step, label: STEP_LABELS[step], document: stepDocument(issue, step) }));
}

/** Whether this build knows a step word. `Project.gated_steps` is a list of plain strings in the
 *  generated document, because a Dispatch newer than this build may gate a fourth step, so this
 *  is where one it does not know is left out rather than drawn without a name. */
function isGatedStep(step: string): step is GatedStep {
  return (GATED_STEPS as readonly string[]).includes(step);
}
