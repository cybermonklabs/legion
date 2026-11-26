import { createIssue, createIssueArtifact, createProject } from "./api";
import { specBlockRef } from "./issue-layout-seed-text";

/**
 * The seed of the Plan view's row of sizes (`e2e/issue-page-t-plan.e2e.ts`): an issue whose plan
 * writes three tasks a, b and c, in that order, where c needs a, a is 45 minutes long, b an hour
 * and a half and c holds no size, each serving a statement of its own; and one issue under it
 * whose own plan writes two tasks, the second needing the first, two hours and half an hour
 * long, serving nothing: the server refuses a statement of an issue under another, whose spec
 * is the one above it. Every write goes through `e2e/api.ts`, as a person uploads a plan, and
 * each `needs` names its task in the same write that makes it, which the server reads from that
 * write's own blocks.
 */

/** The project the seed makes. */
const SIZES_PROJECT = "SIZES";

/** The block ids of the issue's own three tasks, in the order its plan writes them. */
export const OWN_TASKS = { a: "ta", b: "tb", c: "tc" } as const;

/** The block ids of the two tasks of the issue under it, in the order its plan writes them. */
export const CHILD_TASKS = { first: "xa", second: "xb" } as const;

/** The words of each task and statement, one sentence each and no longer than a chip draws
 *  whole (`node-row-chip.ts`' `CHIP_LIMIT`, 32 characters), so a chip naming one, a need's or a
 *  statement's, names it by the whole of its words. */
export const PLAN_SIZES_WORDS = {
  a: "Lay the schema down.",
  b: "Write the reader.",
  c: "Check the reader on the schema.",
  childFirst: "Build the child's store.",
  childSecond: "Fill the child's store.",
  statements: [
    "A bar is as long as its size.",
    "A step says how long it takes.",
    "A step starts after its needs.",
  ],
} as const;

/** Each task's size as the plan writes it, and in the whole minutes the Plan read answers; c
 *  holds none. */
export const PLAN_SIZES = {
  a: { minutes: 45, written: "45m" },
  b: { minutes: 90, written: "1h30m" },
  childFirst: { minutes: 120, written: "2h" },
  childSecond: { minutes: 30, written: "30m" },
} as const;

export interface PlanSizesSeed {
  /** The key of the issue under `key`. */
  readonly child: string;
  readonly childPlanArtifactId: string;
  readonly key: string;
  readonly planArtifactId: string;
}

/** One plan task block: its id, its attributes in the order the server's own tests write them,
 *  and its words. */
function planTask(id: string, attributes: readonly string[], words: string): string {
  return `:::plan_task{#${id}${attributes.map((one) => ` ${one}`).join("")}}\n${words}\n:::\n`;
}

export async function seedPlanSizes(): Promise<PlanSizesSeed> {
  const W = PLAN_SIZES_WORDS;
  await createProject({ key: SIZES_PROJECT, name: "Plan sizes" });
  const issue = await createIssue({
    force: true,
    project: SIZES_PROJECT,
    spec: W.statements
      .map((words, index) => `:::requirement{#r${index + 1}}\n${words}\n:::\n`)
      .join("\n"),
    title: "A plan whose tasks hold sizes",
  });
  const serves = (id: string) => `serves="${specBlockRef(issue.key, id)}"`;
  const plan = await createIssueArtifact(issue.key, {
    content: [
      "How the sizes are read.\n",
      planTask(OWN_TASKS.a, [serves("r1"), `size="${PLAN_SIZES.a.written}"`], W.a),
      planTask(OWN_TASKS.b, [serves("r2"), `size="${PLAN_SIZES.b.written}"`], W.b),
      planTask(
        OWN_TASKS.c,
        [serves("r3"), `needs="dispatch://${issue.key}/artifact/plan-md#b-${OWN_TASKS.a}"`],
        W.c
      ),
    ].join("\n"),
    name: "plan.md",
  });
  const child = await createIssue({
    force: true,
    parent: issue.key,
    project: SIZES_PROJECT,
    title: "The issue under the plan",
  });
  const childPlan = await createIssueArtifact(child.key, {
    content: [
      "How the child's sizes are read.\n",
      planTask(CHILD_TASKS.first, [`size="${PLAN_SIZES.childFirst.written}"`], W.childFirst),
      planTask(
        CHILD_TASKS.second,
        [
          `size="${PLAN_SIZES.childSecond.written}"`,
          `needs="dispatch://${child.key}/artifact/plan-md#b-${CHILD_TASKS.first}"`,
        ],
        W.childSecond
      ),
    ].join("\n"),
    name: "plan.md",
  });
  return {
    child: child.key,
    childPlanArtifactId: childPlan.artifact.id,
    key: issue.key,
    planArtifactId: plan.artifact.id,
  };
}
