#!/usr/bin/env bun
/**
 * The one reader of the private render's export, and its comparison with the rows of the public
 * prompt-inputs proof (scripts/e2e/prompt-inputs.sh). The private render composes each launch of
 * each deployment from the public prompt parts at one public commit, and its export.json holds
 * outcomes, byte counts and sha256 digests alone. This reads only those.
 *
 *   bun teardown-inputs.ts compare --export <export.json> --evidence <dir> --head <commit>
 *       --branch <branch> --projects <project,...> --launches <launch,...>
 *   bun teardown-inputs.ts sizes --before <export.json> --before-private <label>
 *       --after <export.json> --head <commit> --branch <branch> --projects <project,...>
 *       --launches <launch,...>
 *   bun teardown-inputs.ts rows --before <rows.jsonl> --after <rows.jsonl>
 *
 * --export, and the --before and --after of `sizes`, are export.json files of the schema
 * prompt-render/2, and of no other version. The keys read are the ones that version keeps from
 * prompt-render/1: `public` (each read's `requested`, `commit`, `branch`, `branchHead`,
 * `isBranchHead` and `problems`),
 * `renders` (each render's `private` `label` and `commit`, `project`, `publicCommit`, `gate`,
 * `problems`, `deployment`, `memory`, `requiredReadBytes` and `launches`: each launch's `launch`,
 * `role`, `backend`, `parts` with each part's `path`, `bytes` and `sha256`, `roleText` and
 * `assembled`), `controls` (each `name` and `outcome`), `outcome` and `problems`. Every other key
 * is left unread, so the keys version 2 adds do not stop it. A list written as null reads as empty.
 *
 * --evidence is the directory a prompt-inputs.sh run printed. Its rows.jsonl holds one row for
 * each launch, as the Go type promptInputsRow of
 * packages/daemon/internal/daemon/prompt_inputs_launches_test.go writes it (every key of that type
 * and no other), and its composition.json names in `subject_head` the public commit those rows
 * were measured at. A row's first request counts the text of the other messages as a part of its
 * own, `other_messages_bytes`, and its total includes it. A row written before that part was
 * counted has no such key. It still reads, as history, but no command takes it for a
 * measurement: `compare` and `rows` each refuse it, naming its launch.
 *
 * --head is the exact public commit, 40 lower-case hex. --branch is the public branch the render
 * was asked to compare that commit with (the run's `legion_branch`): for a pull request's head,
 * the pull request's branch, not `main`. --projects and --launches are the projects and launches
 * the caller expects, comma-separated, each launch by the name the rows give it
 * (`implementer/claude`). The export is held to these lists, and is never taken as complete by
 * itself.
 *
 * Two names are normalized, and nothing else. A render's launch takes the rows' name:
 * `controller-daemon` is `controller-headless/<backend>`, `controller-operator` is
 * `controller-interactive/<backend>`, `architect-root` and `architect-child` are
 * `<name>/<backend>`, and `<role>-<backend>` is `<role>/<backend>`, each only where the launch's
 * own role agrees. A part's path loses `packages/daemon/internal/prompts/`, so
 * `packages/daemon/internal/prompts/roles/merger.md` is `roles/merger.md`.
 *
 * `compare` holds the export to the head: one read of the public commit, the head requested and
 * resolved, and the head of --branch when it was read; an outcome of `pass` with no problem; the
 * wrong-project and missing-reference controls, each `detected`; one gate render of each expected
 * project and none other, at the head and with no problem; and one launch of each expected kind.
 * It holds the rows to the head (composition.json's `subject_head`) and to the launches: one row
 * of each, none other, none without a part, and none without other_messages_bytes. Then, launch by
 * launch, the export's parts and the row's agree in order, path, bytes and sha256. It prints a
 * line for each project and `teardown-inputs: pass`, or each problem and `teardown-inputs: fail`.
 *
 * `sizes` compares no part. It prints, before and after, the bytes of each project's deployment,
 * memory and required reads, and of each launch's role parts and assembled prompt (addressing left
 * out). --after is held as `compare` holds an export, and its gate renders are read. --before is
 * held to its outcome and controls and to the lists, and its renders are those of the private
 * checkout --before-private names (`base` in a pull request's run: the old deployment text at
 * each project's released Legion); a before render's own problems are counted, not failed.
 *
 * `rows` compares two runs of the prompt-inputs proof, the base's rows.jsonl (--before) and the
 * head's (--after). It prints one table with a line for each launch: the bytes of its role parts,
 * of each part of its first request (the system text, the tool definitions, the skill list, the
 * dispatch-first skill, the first message and the other messages), of its required reads (marked
 * `not observed` where the run did not observe them) and its total, before and after. It refuses,
 * naming the launch and the arm, a row measured without the other messages, a launch one arm has
 * twice, and a launch that only one arm has; it drops no row.
 *
 * Exit 0 when every check holds, 1 when one does not or an input does not read, 2 when the
 * arguments are refused.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { type CliIo, processIo } from "./production-dispatch";

/** The one export version this reads. */
export const EXPORT_SCHEMA = "prompt-render/2";
/** The directory the export names each part under; a row names it from there. */
export const PROMPTS_DIR = "packages/daemon/internal/prompts/";

const COMMIT = /^[0-9a-f]{40}$/;
const SHA256 = /^[0-9a-f]{64}$/;

const Bytes = z.number().int().nonnegative();
const Sha256 = z.string().regex(SHA256, "a lower-case sha256");
/** A Go slice: nil encodes as null, which reads as empty. */
const list = <T extends z.ZodType>(item: T) =>
  z
    .array(item)
    .nullable()
    .transform((items) => items ?? []);

// The private render's export: only the keys this reads, every other key left unread.
const Digest = z.looseObject({ bytes: Bytes, sha256: z.string() });
const PartRow = z.looseObject({ path: z.string().min(1), bytes: Bytes, sha256: Sha256 });
const LaunchRow = z.looseObject({
  launch: z.string().min(1),
  role: z.string().min(1),
  backend: z.string().min(1),
  parts: list(PartRow),
  roleText: Digest,
  assembled: Digest,
});
const Render = z.looseObject({
  private: z.looseObject({ label: z.string().min(1), commit: z.string() }),
  project: z.string().min(1),
  publicCommit: z.string(),
  gate: z.boolean(),
  problems: list(z.string()),
  deployment: Digest,
  memory: Digest.optional(),
  requiredReadBytes: Bytes,
  launches: list(LaunchRow),
});
const PublicRef = z.looseObject({
  requested: z.string(),
  commit: z.string(),
  branch: z.string(),
  branchHead: z.string(),
  isBranchHead: z.boolean(),
  problems: list(z.string()),
});
const RenderExport = z.looseObject({
  schema: z.literal(EXPORT_SCHEMA),
  public: list(PublicRef),
  renders: list(Render),
  controls: list(z.looseObject({ name: z.string().min(1), outcome: z.string() })),
  outcome: z.string(),
  problems: list(z.string()),
});
export type RenderExport = z.infer<typeof RenderExport>;
export type Render = z.infer<typeof Render>;
export type LaunchRow = z.infer<typeof LaunchRow>;

// The prompt-inputs proof's row: promptInputsRow and the types under it, key for key.
const RowPart = z.strictObject({ path: z.string().min(1), bytes: Bytes, sha256: Sha256 });
const PromptInputsRow = z.strictObject({
  launch: z.string().min(1),
  parts: list(RowPart),
  addressing_bytes: Bytes,
  task_bytes: Bytes,
  first_request: z.strictObject({
    system_text_bytes: Bytes,
    tool_definitions_bytes: Bytes,
    skill_list_bytes: Bytes,
    dispatch_first_bytes: Bytes,
    first_message_bytes: Bytes,
    // Absent from a row written before the driver counted the other messages: such a row reads
    // as history, and `measured` refuses it.
    other_messages_bytes: Bytes.optional(),
  }),
  required_reads: list(z.strictObject({ path: z.string(), bytes: Bytes })),
  reads_observed: z.string().optional(),
  references: z
    .array(
      z.strictObject({
        kind: z.string(),
        path: z.string(),
        resolved: z.boolean(),
        bytes: Bytes,
        detail: z.string().optional(),
      })
    )
    .optional(),
  total_bytes: Bytes,
});
export type PromptInputsRow = z.infer<typeof PromptInputsRow>;

const Composition = z.looseObject({ subject_head: z.string().regex(COMMIT, "a 40-hex commit") });

function parsed<T extends z.ZodType>(schema: T, raw: unknown): z.infer<T> {
  const result = schema.safeParse(raw);
  if (!result.success) throw new Error(z.prettifyError(result.error).replaceAll("\n", " "));
  return result.data;
}

/** An export.json's text, read; throws where it is not of the schema prompt-render/2. */
export function parseExport(text: string): RenderExport {
  const raw: unknown = JSON.parse(text);
  const schema =
    typeof raw === "object" && raw !== null && "schema" in raw ? raw.schema : undefined;
  if (schema !== EXPORT_SCHEMA)
    throw new Error(`its schema is ${JSON.stringify(schema)}; this reads ${EXPORT_SCHEMA} alone`);
  return parsed(RenderExport, raw);
}

/** A rows.jsonl's text, one row a line; throws, naming the line, where one is not a row. */
export function parseRows(text: string): PromptInputsRow[] {
  return text.split("\n").flatMap((line, index) => {
    if (line === "") return [];
    try {
      return [parsed(PromptInputsRow, JSON.parse(line))];
    } catch (error) {
      throw new Error(`line ${index + 1}: ${messageFor(error)}`);
    }
  });
}

/** A composition.json's text: the public commit its rows were measured at. */
export function parseComposition(text: string): string {
  return parsed(Composition, JSON.parse(text)).subject_head;
}

/** What the caller expects: the exact public head, its branch, and the projects and launches. */
export interface Expected {
  readonly head: string;
  readonly branch: string;
  readonly projects: readonly string[];
  readonly launches: readonly string[];
}

/** A prompt-inputs run's evidence: its rows and the public commit they were measured at. */
export interface Evidence {
  readonly rows: readonly PromptInputsRow[];
  readonly subjectHead: string;
}

/** The render's launches whose rows' name is not `<role>/<backend>`: the role each must have,
 * and the rows' name before its backend. */
const SHAPES: Readonly<Record<string, { role: string; shape: string }>> = {
  "controller-daemon": { role: "controller", shape: "controller-headless" },
  "controller-operator": { role: "controller", shape: "controller-interactive" },
  "architect-root": { role: "architect", shape: "architect-root" },
  "architect-child": { role: "architect", shape: "architect-child" },
};

/** The rows' name of a render's launch, or undefined where it is no name this reader knows. */
export function rowsLaunchName(launch: Pick<LaunchRow, "launch" | "role" | "backend">) {
  const { launch: name, role, backend } = launch;
  const known = Object.hasOwn(SHAPES, name) ? SHAPES[name] : undefined;
  if (known !== undefined) return known.role === role ? `${known.shape}/${backend}` : undefined;
  return name === `${role}-${backend}` ? `${role}/${backend}` : undefined;
}

/** The rows' path of a render's part, or undefined where it is not under PROMPTS_DIR. */
export function rowsPartPath(exported: string): string | undefined {
  return exported.startsWith(PROMPTS_DIR) ? exported.slice(PROMPTS_DIR.length) : undefined;
}

function grouped<T>(items: readonly T[], key: (item: T) => string): Map<string, T[]> {
  const groups = new Map<string, T[]>();
  for (const item of items) {
    const group = groups.get(key(item));
    if (group === undefined) groups.set(key(item), [item]);
    else group.push(item);
  }
  return groups;
}

/** The export's own verdict: its outcome, its problems and its controls. */
function verdictProblems(exported: RenderExport, where: string): string[] {
  const problems: string[] = [];
  if (exported.outcome !== "pass")
    problems.push(`${where}the export's outcome is ${JSON.stringify(exported.outcome)}`);
  if (exported.problems.length > 0)
    problems.push(`${where}the export reports ${exported.problems.length} problems`);
  if (exported.controls.length === 0) problems.push(`${where}the export carries no control`);
  else
    for (const name of ["wrong-project-override", "missing-reference"]) {
      const count = exported.controls.filter((control) => control.name === name).length;
      if (count !== 1) problems.push(`${where}the export carries ${count} ${name} controls`);
    }
  for (const control of exported.controls)
    if (control.outcome !== "detected")
      problems.push(`${where}control ${control.name}: ${control.outcome}`);
  return problems;
}

/** The export's reads of the public repository: the head alone, the head of the branch. */
function headProblems(exported: RenderExport, expected: Expected, where: string): string[] {
  const { head, branch } = expected;
  const problems: string[] = [];
  if (exported.public.length === 0) problems.push(`${where}the export read no public commit`);
  for (const read of exported.public) {
    if (read.requested !== head) {
      problems.push(`${where}the export read the public commit ${read.requested}, not ${head}`);
      continue;
    }
    if (read.commit !== head)
      problems.push(`${where}the export resolved ${head} to ${read.commit || "nothing"}`);
    if (read.branch !== branch)
      problems.push(`${where}the export compared ${head} with ${read.branch}, not ${branch}`);
    else if (!read.isBranchHead || read.branchHead !== head)
      problems.push(
        `${where}${head} was not the head of ${branch} when the export read it (the head was ${read.branchHead || "not read"})`
      );
    if (read.problems.length > 0)
      problems.push(`${where}the export's read of ${head} has ${read.problems.length} problems`);
  }
  const reads = exported.public.filter((read) => read.requested === head).length;
  if (reads > 1) problems.push(`${where}the export read ${head} ${reads} times`);
  return problems;
}

/** One render of an expected project, its launches by the rows' names. */
interface Held {
  readonly render: Render;
  readonly launches: ReadonlyMap<string, LaunchRow>;
}

interface HeldRenders {
  readonly held: ReadonlyMap<string, Held>;
  readonly problems: readonly string[];
}

/**
 * The renders held to the expected projects and launches: one render of each project and none
 * other, and in each one launch of each launch and none other. `head`, where given, is the public
 * commit each render must be at, and a render's own problems then fail it.
 */
function holdRenders(
  renders: readonly Render[],
  expected: Expected,
  where: string,
  which: string,
  head?: string
): HeldRenders {
  const problems: string[] = [];
  const held = new Map<string, Held>();
  if (renders.length === 0) problems.push(`${where}the export holds no ${which} render`);
  const byProject = grouped(renders, (render) => render.project);
  for (const [project, group] of byProject) {
    if (!expected.projects.includes(project))
      problems.push(`${where}the export renders ${project}, which is not an expected project`);
    else if (group.length > 1)
      problems.push(`${where}the export renders ${project} ${group.length} times`);
  }
  for (const project of expected.projects) {
    const group = byProject.get(project) ?? [];
    const [render] = group;
    if (render === undefined)
      problems.push(`${where}the export has no ${which} render of ${project}`);
    if (render === undefined || group.length > 1) continue;
    if (head !== undefined) {
      if (render.publicCommit !== head)
        problems.push(`${where}${project} is rendered at ${render.publicCommit}, not ${head}`);
      if (render.problems.length > 0)
        problems.push(`${where}${project}'s render has ${render.problems.length} problems`);
    }
    if (render.launches.length === 0) problems.push(`${where}${project}'s render holds no launch`);
    const launches = new Map<string, LaunchRow>();
    const byName = grouped(render.launches, (launch) => rowsLaunchName(launch) ?? "");
    for (const [name, same] of byName) {
      if (name === "") {
        for (const launch of same)
          problems.push(
            `${where}${project}: the launch ${launch.launch} (${launch.role} on ${launch.backend}) is no launch this reader knows`
          );
      } else if (!expected.launches.includes(name))
        problems.push(`${where}${project}: the launch ${name} is not an expected launch`);
      else if (same.length > 1)
        problems.push(
          `${where}${project}: the export holds the launch ${name} ${same.length} times`
        );
      else if (same[0] !== undefined) launches.set(name, same[0]);
    }
    for (const name of expected.launches)
      if (!byName.has(name)) problems.push(`${where}${project}: the export has no launch ${name}`);
    held.set(project, { render, launches });
  }
  return { held, problems };
}

/**
 * The refusal of a row written before the driver counted the other messages, or undefined for a
 * row that counts them. Such a row's total leaves those bytes out, so it is history, never a
 * measurement. `which` names the row: `row`, or the arm's row.
 */
function unmeasured(which: string, row: PromptInputsRow): string | undefined {
  if (row.first_request.other_messages_bytes !== undefined) return undefined;
  return `the ${which} of ${row.launch} has no other_messages_bytes: it was measured without the other messages, and its total leaves them out`;
}

/** The rows held to the head and the expected launches: one row of each, each with a part and
 * each measured with the other messages. */
function rowProblems(evidence: Evidence, expected: Expected): string[] {
  const problems: string[] = [];
  if (evidence.subjectHead !== expected.head)
    problems.push(
      `the rows were measured at ${evidence.subjectHead} (composition.json), not ${expected.head}`
    );
  if (evidence.rows.length === 0) problems.push("the rows hold no launch");
  const byLaunch = grouped(evidence.rows, (row) => row.launch);
  for (const [launch, same] of byLaunch) {
    const [row] = same;
    if (!expected.launches.includes(launch))
      problems.push(`the rows hold ${launch}, which is not an expected launch`);
    else if (same.length > 1) problems.push(`the rows hold ${launch} ${same.length} times`);
    else if (row !== undefined) {
      if (row.parts.length === 0) problems.push(`the row of ${launch} has no part`);
      const refusal = unmeasured("row", row);
      if (refusal !== undefined) problems.push(refusal);
    }
  }
  for (const launch of expected.launches)
    if (!byLaunch.has(launch)) problems.push(`the rows hold no row of ${launch}`);
  return problems;
}

/** Where one launch's parts and its row's differ: in order and path, then in bytes or sha256. */
function partProblems(
  where: string,
  exported: readonly z.infer<typeof PartRow>[],
  row: readonly z.infer<typeof RowPart>[]
): string[] {
  if (exported.length === 0) return [`${where}: the export holds no part`];
  const outside = exported.filter((part) => rowsPartPath(part.path) === undefined);
  if (outside.length > 0)
    return outside.map((part) => `${where}: the part ${part.path} is not under ${PROMPTS_DIR}`);
  const paths = exported.map((part) => rowsPartPath(part.path));
  if (paths.length !== row.length || paths.some((each, index) => each !== row[index]?.path))
    return [
      `${where}: the export's parts are ${paths.join(", ")}, and the row's are ${row.map((part) => part.path).join(", ")}`,
    ];
  return exported.flatMap((part, index) => {
    const theirs = row[index];
    if (theirs === undefined) return [];
    const problems: string[] = [];
    if (part.bytes !== theirs.bytes)
      problems.push(
        `${where} ${theirs.path}: ${part.bytes} bytes in the export, ${theirs.bytes} in the row`
      );
    if (part.sha256 !== theirs.sha256)
      problems.push(
        `${where} ${theirs.path}: sha256 ${part.sha256} in the export, ${theirs.sha256} in the row`
      );
    return problems;
  });
}

/** What `compare` found: every problem, for each project the launches and parts that agree, and
 * the private checkout of the gate renders. */
export interface Comparison {
  readonly problems: readonly string[];
  readonly projects: readonly { project: string; launches: number; parts: number }[];
  readonly checkout?: { readonly label: string; readonly commit: string };
}

/** The export's public parts, launch by launch, against the prompt-inputs rows at the head. */
export function compare(
  exported: RenderExport,
  evidence: Evidence,
  expected: Expected
): Comparison {
  const gates = exported.renders.filter((render) => render.gate);
  const renders = holdRenders(gates, expected, "", "gate", expected.head);
  const problems = [
    ...verdictProblems(exported, ""),
    ...headProblems(exported, expected, ""),
    ...renders.problems,
    ...rowProblems(evidence, expected),
  ];
  const rows = grouped(evidence.rows, (row) => row.launch);
  const projects = [...renders.held].map(([project, { launches }]) => {
    let agree = 0;
    let parts = 0;
    for (const [name, launch] of launches) {
      const [row, ...more] = rows.get(name) ?? [];
      if (row === undefined || more.length > 0 || row.parts.length === 0) continue;
      const found = partProblems(`${project} ${name}`, launch.parts, row.parts);
      problems.push(...found);
      if (found.length === 0) {
        agree += 1;
        parts += row.parts.length;
      }
    }
    return { project, launches: agree, parts };
  });
  return { problems, projects, checkout: gates[0]?.private };
}

/** What `sizes` found: every problem, or the before and after table. */
export interface Sizes {
  readonly problems: readonly string[];
  readonly table: string;
}

/** Each held launch that was not composed, so that it has no assembled prompt to measure. */
function uncomposed(where: string, renders: HeldRenders): string[] {
  const problems: string[] = [];
  for (const [project, { launches }] of renders.held)
    for (const [name, launch] of launches)
      if (launch.parts.length === 0 || !SHA256.test(launch.assembled.sha256))
        problems.push(`${where}${project} ${name}: the launch was not composed`);
  return problems;
}

/** The before and after bytes of each project and launch, from the two exports' renders. */
export function sizes(
  before: RenderExport,
  beforePrivate: string,
  after: RenderExport,
  expected: Expected
): Sizes {
  const old = holdRenders(
    before.renders.filter((render) => render.private.label === beforePrivate),
    expected,
    "before: ",
    `${beforePrivate}-labelled`
  );
  const now = holdRenders(
    after.renders.filter((render) => render.gate),
    expected,
    "after: ",
    "gate",
    expected.head
  );
  const problems = [
    ...verdictProblems(before, "before: "),
    ...old.problems,
    ...uncomposed("before: ", old),
    ...verdictProblems(after, "after: "),
    ...headProblems(after, expected, "after: "),
    ...now.problems,
    ...uncomposed("after: ", now),
  ];
  if (problems.length > 0) return { problems, table: "" };

  const pairs = expected.projects.flatMap((project) => {
    const was = old.held.get(project);
    const is = now.held.get(project);
    return was === undefined || is === undefined ? [] : [{ project, was, is }];
  });
  const [first] = pairs;
  const lines = [
    `Before: the private ${beforePrivate} at ${first?.was.render.private.commit}. After: the private ${first?.is.render.private.label} at ${first?.is.render.private.commit}, the public ${expected.head} on ${expected.branch}.`,
    "",
    "| Project | Public before | Deployment bytes before | after | Memory bytes before | after | Required-read bytes before | after | Problems before |",
    "| --- | --- | --- | --- | --- | --- | --- | --- | --- |",
  ];
  for (const { project, was, is } of pairs)
    lines.push(
      `| ${project} | ${was.render.publicCommit.slice(0, 12)} | ${was.render.deployment.bytes} | ${is.render.deployment.bytes} | ${was.render.memory?.bytes ?? "-"} | ${is.render.memory?.bytes ?? "-"} | ${was.render.requiredReadBytes} | ${is.render.requiredReadBytes} | ${was.render.problems.length} |`
    );
  lines.push(
    "",
    "| Project | Launch | Role-part bytes before | after | Assembled bytes before, addressing left out | after |",
    "| --- | --- | --- | --- | --- | --- |"
  );
  for (const { project, was, is } of pairs)
    for (const name of expected.launches) {
      const a = was.launches.get(name);
      const b = is.launches.get(name);
      if (a !== undefined && b !== undefined)
        lines.push(
          `| ${project} | ${name} | ${a.roleText.bytes} | ${b.roleText.bytes} | ${a.assembled.bytes} | ${b.assembled.bytes} |`
        );
    }
  return { problems, table: `${lines.join("\n")}\n` };
}

/** The columns of `rowSizes`' table after the launch: each heading, and its bytes in a row. */
const ROW_COLUMNS: readonly (readonly [string, (row: PromptInputsRow) => number | string])[] = [
  ["Role-part bytes", (row) => row.parts.reduce((sum, part) => sum + part.bytes, 0)],
  ["System text", (row) => row.first_request.system_text_bytes],
  ["Tool definitions", (row) => row.first_request.tool_definitions_bytes],
  ["Skill list", (row) => row.first_request.skill_list_bytes],
  ["Dispatch-first", (row) => row.first_request.dispatch_first_bytes],
  ["First message", (row) => row.first_request.first_message_bytes],
  ["Other messages", (row) => row.first_request.other_messages_bytes ?? "-"],
  [
    "Required reads",
    (row) => {
      const bytes = row.required_reads.reduce((sum, read) => sum + read.bytes, 0);
      return row.reads_observed === "observed" ? bytes : `${bytes}, not observed`;
    },
  ],
  ["Total", (row) => row.total_bytes],
];

/**
 * The before and after bytes of each launch, from two runs' rows: the base's (`before`) and the
 * head's (`after`). Every launch of either run is a line, in the order the base's rows give them,
 * and a launch that only one run has, a launch one run has twice, or a row measured without the
 * other messages is a problem, which leaves no table.
 */
export function rowSizes(
  before: readonly PromptInputsRow[],
  after: readonly PromptInputsRow[]
): Sizes {
  const problems: string[] = [];
  const arms = (
    [
      ["before", before],
      ["after", after],
    ] as const
  ).map(([arm, rows]) => {
    if (rows.length === 0) problems.push(`the ${arm} rows hold no launch`);
    const byLaunch = grouped(rows, (row) => row.launch);
    for (const [launch, same] of byLaunch)
      if (same.length > 1) problems.push(`the ${arm} rows hold ${launch} ${same.length} times`);
    for (const row of rows) {
      const refusal = unmeasured(`${arm} row`, row);
      if (refusal !== undefined) problems.push(refusal);
    }
    return { arm, byLaunch };
  });
  const [was, is] = arms;
  if (was === undefined || is === undefined) return { problems, table: "" };
  const launches = [...new Set([...was.byLaunch.keys(), ...is.byLaunch.keys()])];
  for (const launch of launches)
    for (const [arm, other] of [
      [was, is],
      [is, was],
    ] as const)
      if (!arm.byLaunch.has(launch))
        problems.push(
          `${launch} has a row in the ${other.arm} rows and none in the ${arm.arm} rows`
        );
  if (problems.length > 0) return { problems, table: "" };

  const lines = [
    `| Launch | ${ROW_COLUMNS.map(([heading]) => `${heading} before | after`).join(" | ")} |`,
    `| --- |${" --- | --- |".repeat(ROW_COLUMNS.length)}`,
  ];
  for (const launch of launches) {
    const [a] = was.byLaunch.get(launch) ?? [];
    const [b] = is.byLaunch.get(launch) ?? [];
    if (a === undefined || b === undefined) continue;
    lines.push(
      `| ${launch} | ${ROW_COLUMNS.map(([, bytes]) => `${bytes(a)} | ${bytes(b)}`).join(" | ")} |`
    );
  }
  return { problems, table: `${lines.join("\n")}\n` };
}

const USAGE = [
  "usage: teardown-inputs.ts compare --export <export.json> --evidence <dir> --head <commit> --branch <branch> --projects <project,...> --launches <launch,...>",
  "   or: teardown-inputs.ts sizes --before <export.json> --before-private <label> --after <export.json> --head <commit> --branch <branch> --projects <project,...> --launches <launch,...>",
  "   or: teardown-inputs.ts rows --before <rows.jsonl> --after <rows.jsonl>",
].join("\n");

const FLAGS = {
  compare: ["--export", "--evidence", "--head", "--branch", "--projects", "--launches"],
  sizes: [
    "--before",
    "--before-private",
    "--after",
    "--head",
    "--branch",
    "--projects",
    "--launches",
  ],
  rows: ["--before", "--after"],
} as const;

type Arguments =
  | { readonly command: "rows"; readonly values: ReadonlyMap<string, string> }
  | {
      readonly command: "compare" | "sizes";
      readonly values: ReadonlyMap<string, string>;
      readonly expected: Expected;
    };

/** A comma-separated list of names, or the refusal that names what is wrong with it. */
function names(flag: string, value: string): string[] | string {
  const items = value.split(",");
  if (items.some((item) => item === "")) return `${flag} ${value} holds an empty name`;
  const twice = items.find((item, index) => items.indexOf(item) !== index);
  if (twice !== undefined) return `${flag} names ${twice} twice`;
  return items;
}

/** The arguments, or the refusal that names the bad one. */
function parseArguments(argv: readonly string[]): Arguments | string {
  const [command, ...rest] = argv;
  if (command !== "compare" && command !== "sizes" && command !== "rows") return USAGE;
  const flags: readonly string[] = FLAGS[command];
  const values = new Map<string, string>();
  for (let index = 0; index < rest.length; index += 2) {
    const flag = rest[index] ?? "";
    const value = rest[index + 1];
    if (!flags.includes(flag)) return `unknown argument ${flag} for ${command}\n${USAGE}`;
    if (value === undefined || value === "") return `${flag} needs a value`;
    if (values.has(flag)) return `${flag} is given twice`;
    values.set(flag, value);
  }
  const missing = flags.filter((flag) => !values.has(flag));
  if (missing.length > 0) return `${command} needs ${missing.join(", ")}\n${USAGE}`;
  if (command === "rows") return { command, values };
  const head = values.get("--head") ?? "";
  if (!COMMIT.test(head)) return `--head ${head} is not a 40-hex lower-case commit`;
  const projects = names("--projects", values.get("--projects") ?? "");
  if (typeof projects === "string") return projects;
  const launches = names("--launches", values.get("--launches") ?? "");
  if (typeof launches === "string") return launches;
  const branch = values.get("--branch") ?? "";
  return { command, values, expected: { head, branch, projects, launches } };
}

function readInput<T>(file: string, parse: (text: string) => T): T {
  try {
    return parse(readFileSync(file, "utf8"));
  } catch (error) {
    throw new Error(`${file}: ${messageFor(error)}`);
  }
}

/** The whole run. Returns the exit status: 0 (every check holds), 1 (one does not) or 2. */
export function main(argv: readonly string[], io: Pick<CliIo, "out" | "err">): number {
  const args = parseArguments(argv);
  if (typeof args === "string") {
    io.err(`teardown-inputs: ${args}`);
    return 2;
  }
  const file = (flag: string) => path.resolve(args.values.get(flag) ?? "");
  let problems: readonly string[];
  const lines: string[] = [];
  try {
    if (args.command === "compare") {
      const exported = readInput(file("--export"), parseExport);
      const rows = readInput(path.join(file("--evidence"), "rows.jsonl"), parseRows);
      const subjectHead = readInput(
        path.join(file("--evidence"), "composition.json"),
        parseComposition
      );
      const found = compare(exported, { rows, subjectHead }, args.expected);
      problems = found.problems;
      for (const { project, launches, parts } of found.projects)
        lines.push(`ok ${project}: ${launches} launches, ${parts} parts agree with the rows`);
      lines.push(
        `teardown-inputs: pass: the private ${found.checkout?.label} at ${found.checkout?.commit}, the public ${args.expected.head} on ${args.expected.branch}`
      );
    } else if (args.command === "sizes") {
      const before = readInput(file("--before"), parseExport);
      const after = readInput(file("--after"), parseExport);
      const found = sizes(before, args.values.get("--before-private") ?? "", after, args.expected);
      problems = found.problems;
      lines.push(found.table.trimEnd());
    } else {
      const found = rowSizes(
        readInput(file("--before"), parseRows),
        readInput(file("--after"), parseRows)
      );
      problems = found.problems;
      lines.push(found.table.trimEnd());
    }
  } catch (error) {
    io.err(`teardown-inputs: ${messageFor(error)}`);
    return 1;
  }
  if (problems.length > 0) {
    for (const problem of problems) io.out(`problem: ${problem}`);
    io.out(`teardown-inputs: fail, ${problems.length} problems`);
    return 1;
  }
  for (const line of lines) io.out(line);
  return 0;
}

if (import.meta.main) {
  process.exit(main(Bun.argv.slice(2), processIo()));
}
