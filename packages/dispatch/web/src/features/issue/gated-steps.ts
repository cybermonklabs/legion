import { DESIGN_GATE_STEPS } from "@legion/contracts";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import type { Artifact, IssueDetails, Project } from "../../api/types";

/** A design-gate step this build draws: the steps a deployment gates. */
type DesignGateStep = (typeof DESIGN_GATE_STEPS)[number];

/** What the page calls the document each design-gate step reviews: Dispatch's own name for that
 *  document, so a reader is told which document waits rather than the enumeration's word for the
 *  step (`criteria` names the Verification document). */
export const STEP_LABELS: Record<DesignGateStep, string> = {
  criteria: "Verification",
  spec: "Spec",
};

/** One design-gate step of a root issue: the step, the name the page draws for it, and the
 *  document it reviews - undefined for an issue that holds no document for the step, which is
 *  the step nobody has written yet. */
export interface StepEntry {
  step: DesignGateStep;
  label: string;
  document: Artifact | undefined;
}

/** The document the issue holds for `step`, by the id the issue names for it, never by the
 *  document's role or name among `issue.artifacts`. */
function stepDocument(issue: IssueDetails, step: DesignGateStep): Artifact | undefined {
  switch (step) {
    case "spec":
      return primarySpec(issue);
    case "criteria":
      return roleDocument(issue, "verification");
  }
}

/**
 * The design-gate steps the issue's header draws, one for each step the project's deployment
 * gates, in the order the deployment published them. It is empty - which leaves the header its
 * one chip for the issue's primary document - until the project and the issue are both read, for
 * a project whose deployment gates nothing, and for an issue that is not its tree's root, since
 * only a root issue stands in the gate.
 *
 * A step word this build does not draw is left out rather than drawn without a name: a Dispatch
 * newer than this build may gate another step, and a Dispatch of the release before may still
 * answer `decisions`, a step that is gone (dispatch://INFRA-1150 R11).
 */
export function gatedStepEntries(
  project: Project | undefined,
  issue: IssueDetails | undefined
): StepEntry[] {
  const steps = project?.gated_steps;
  if (steps === undefined || steps === null || steps.length === 0) {
    return [];
  }
  if (issue === undefined || issue.parent !== null) {
    return [];
  }
  return steps
    .filter(isDesignGateStep)
    .map((step) => ({ step, label: STEP_LABELS[step], document: stepDocument(issue, step) }));
}

/** Whether this build draws a step word. `Project.gated_steps` is a list of plain strings in the
 *  generated document, because a Dispatch newer than this build may gate another step, so this is
 *  where one it does not draw is left out rather than drawn without a name. */
function isDesignGateStep(step: string): step is DesignGateStep {
  return (DESIGN_GATE_STEPS as readonly string[]).includes(step);
}
