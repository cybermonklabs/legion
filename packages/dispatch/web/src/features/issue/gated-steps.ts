import { DESIGN_GATE_STEPS } from "@legion/contracts";

import type { ArtifactWithTrees, IssueDetails } from "../../api/types";

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
 *  the step nobody has written yet. The document is the issue details' own row, so its approval
 *  carries the tree of its review reason. */
export interface StepEntry {
  step: DesignGateStep;
  label: string;
  document: ArtifactWithTrees | undefined;
}

/**
 * The design-gate steps the issue's header draws: one for each line of the issue read's approval
 * gate (`approval_gate`) that names a step, in the gate's order, which is the order the
 * deployment published the steps in. The server decides the lines: a root of its project - an
 * issue with no parent, or whose parent is an issue of another project - in a project that gates
 * steps has one line for each step, and every other issue one line with no step, for its primary
 * document. Each step's document is the issue details' own row the line names by its id.
 *
 * It is empty - which leaves the header its one chip for the issue's primary document - for an
 * issue whose gate names no step, and for an issue read with no gate at all, which a Dispatch
 * older than the field answers.
 *
 * A step word this build does not draw is left out rather than drawn without a name: a Dispatch
 * newer than this build may gate another step.
 */
export function approvalGateSteps(issue: IssueDetails | undefined): StepEntry[] {
  return (issue?.approval_gate ?? []).flatMap((line) => {
    const step = line.step;
    if (step === undefined || !isDesignGateStep(step)) {
      return [];
    }
    const document =
      line.artifact_id === null
        ? undefined
        : issue?.artifacts?.find((artifact) => artifact.id === line.artifact_id);
    return [{ document, label: STEP_LABELS[step], step }];
  });
}

/** Whether this build draws a step word. A gate line's `step` is a plain string in the generated
 *  document, because a Dispatch newer than this build may gate another step, so this is where one
 *  it does not draw is told apart. */
export function isDesignGateStep(step: string): step is DesignGateStep {
  return (DESIGN_GATE_STEPS as readonly string[]).includes(step);
}
