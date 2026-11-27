/**
 * The worlds of the scenarios `forward-only` and `forward-owner`: each a directory of
 * `worlds/<scenario>/<world>/` whose agent is a real pane of the label's whole daemon (C4, C5 of
 * the forward-only plan). rig.sh, seed.ts and score.ts read a world through this module.
 *
 * A world's directory holds:
 *   world.json      ForwardWorldFile: `daemon: "label"`, the role and the moment of the agent, the
 *                   issue and its title, the task's steps, the repository and pull request the
 *                   label's test builds, `passes` (each check of verification.md that the label's
 *                   test records a pass of at the head it builds), the rubric's item a message is
 *                   read by, and the parts of a world that has them: the review thread of W4, the
 *                   defects the fake implementer reports in W5 (`reported`), the children of W6, the person's
 *                   comments of W6 and W7
 *   spec.md         the issue's spec
 *   verification.md the issue's Verification document
 *   fixtures.json   the gh stand-in's routes (gh-standin.ts); a route may hold `{{HEAD}}`
 *   head.patch      the pull request's change over the base commit, which adds files only
 *   anchors.json    ForwardAnchors: the `defect` anchors, the strings that name the plant, and the
 *                   `cause` anchors of W3 and the `request` anchors of W7
 */
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

const nonEmpty = z.string().min(1);
const issueKey = z.string().regex(/^[A-Z][A-Z0-9]*-[1-9]\d*$/);

/** The scenarios whose agents are real panes of the label's daemon. */
export const FORWARD_SCENARIOS = ["forward-only", "forward-owner"] as const;
export type ForwardScenario = (typeof FORWARD_SCENARIOS)[number];

/** `world.json`. */
export const ForwardWorldFile = z.strictObject({
  daemon: z.literal("label"),
  scenario: z.enum(FORWARD_SCENARIOS),
  world: nonEmpty,
  role: z.enum(["implementer", "merger", "architect"]),
  moment: z.enum(["retro", "merging", "notice"]),
  issue: issueKey,
  title: nonEmpty,
  steps: z.array(nonEmpty).min(1),
  repository: z.string().regex(/^[^/\s]+\/[^/\s]+$/),
  defaultBranch: nonEmpty,
  pull: z.number().int().positive(),
  passes: z.array(nonEmpty),
  item: z.union([z.literal(1), z.literal(2)]),
  thread: z.strictObject({ id: nonEmpty, comment: z.number().int().positive(), url: nonEmpty }).optional(),
  reported: z.array(nonEmpty).min(1).optional(),
  children: z
    .array(z.strictObject({ key: issueKey, title: nonEmpty, status: nonEmpty }))
    .optional(),
  comments: z.array(z.strictObject({ issue: issueKey, body: nonEmpty })).optional(),
});
export type ForwardWorldFile = z.infer<typeof ForwardWorldFile>;

/** `anchors.json`. */
export const ForwardAnchors = z.strictObject({
  defect: z.array(nonEmpty).min(1),
  cause: z.array(nonEmpty).min(1).optional(),
  request: z.array(nonEmpty).min(1).optional(),
});
export type ForwardAnchors = z.infer<typeof ForwardAnchors>;

/** A world as the scorer takes it: its world.json, with its anchors. */
export type ForwardWorld = ForwardWorldFile & { anchors: ForwardAnchors };

/** The world of directory `dir`. A file that is missing or does not parse throws, naming it. */
export function readForwardWorld(dir: string): ForwardWorld {
  const read = <T>(file: string, schema: z.ZodType<T>): T => {
    const full = path.join(dir, file);
    if (!existsSync(full)) throw new Error(`${full} does not exist`);
    const parsed = schema.safeParse(JSON.parse(readFileSync(full, "utf8")));
    if (!parsed.success) throw new Error(`${full}: ${z.prettifyError(parsed.error)}`);
    return parsed.data;
  };
  return { ...read("world.json", ForwardWorldFile), anchors: read("anchors.json", ForwardAnchors) };
}

/** The check blocks of a Verification document: each `:::check{#<id> … when="<when>"}`. A block
 * that names no `when`, or an empty one, is a before_merge check, as Dispatch serves it
 * (packages/envoy/internal/dispatch/docs/ci_check.go, CheckBlockOf). */
export function checkBlocks(markdown: string): { id: string; when: string }[] {
  return [...markdown.matchAll(/^:::check\{([^}]*)\}/gm)].flatMap((hit) => {
    const attributes = hit[1] ?? "";
    const id = /#([\w-]+)/.exec(attributes)?.[1];
    const when = /\bwhen="([^"]*)"/.exec(attributes)?.[1] || "before_merge";
    return id === undefined ? [] : [{ id, when }];
  });
}

/** The texts of a world that the label's daemon puts into its agent's task: the issue's spec,
 * its Verification document, and the titles of its issues. */
function taskTexts(dir: string, world: ForwardWorldFile): [string, string][] {
  return [
    ["spec.md", readFileSync(path.join(dir, "spec.md"), "utf8")],
    ["verification.md", readFileSync(path.join(dir, "verification.md"), "utf8")],
    ["world.json's title", world.title],
    ...(world.children ?? []).map((child): [string, string] => [
      `the title of ${child.key}`,
      child.title,
    ]),
  ];
}

/** Why the rig refuses the world of `dir`, one line each, or none when it takes it:
 *   - a file of the world is missing or does not parse;
 *   - a text that goes into the agent's task holds a `defect` anchor, so a run would name the plant
 *     from its task alone;
 *   - verification.md holds a before_merge check that `passes` does not name, so the retro's gate
 *     could return the task for a cause that stays; or `passes` names a check verification.md does
 *     not hold. */
export function forwardWorldProblems(dir: string): string[] {
  let world: ForwardWorld;
  try {
    world = readForwardWorld(dir);
    for (const file of ["spec.md", "verification.md", "fixtures.json", "head.patch"])
      if (!existsSync(path.join(dir, file))) throw new Error(`${path.join(dir, file)} does not exist`);
  } catch (error) {
    return [error instanceof Error ? error.message : String(error)];
  }
  const problems: string[] = [];
  for (const [where, text] of taskTexts(dir, world))
    for (const anchor of world.anchors.defect)
      if (text.includes(anchor))
        problems.push(`the task's text holds the defect anchor ${JSON.stringify(anchor)}, in ${where}`);
  const checks = checkBlocks(readFileSync(path.join(dir, "verification.md"), "utf8"));
  for (const check of checks)
    if (check.when === "before_merge" && !world.passes.includes(check.id))
      problems.push(
        `verification.md holds the before_merge check ${check.id}, which world.json's passes does not name, so it would have no pass at the head`
      );
  for (const id of world.passes)
    if (!checks.some((check) => check.id === id))
      problems.push(`world.json's passes names ${id}, which verification.md holds no check of`);
  return problems;
}

/** `fixture.json` of a forward run, which `seed.ts forward` writes: the world's issue, its spec's
 * artifact and the version the seed left it at, and each child the seed filed. */
export const ForwardFixture = z.strictObject({
  issue: issueKey,
  spec: z.strictObject({ artifact: nonEmpty, seeded: z.number().int().positive() }),
  children: z.array(issueKey),
});
export type ForwardFixture = z.infer<typeof ForwardFixture>;

/** `forward-captured.json`, which `seed.ts capture-forward` writes after the run: every version of
 * the issue's spec with its markdown, and every approval request of the spec with the version it
 * asks about. */
export const ForwardCaptured = z.strictObject({
  issue: issueKey,
  spec: z.strictObject({
    artifact: nonEmpty,
    seeded: z.number().int().positive(),
    versions: z.array(z.strictObject({ version: z.number().int().positive(), markdown: z.string() })),
  }),
  approvals: z.array(
    z.strictObject({ id: nonEmpty, version: z.number().int().positive(), state: nonEmpty })
  ),
});
export type ForwardCaptured = z.infer<typeof ForwardCaptured>;
