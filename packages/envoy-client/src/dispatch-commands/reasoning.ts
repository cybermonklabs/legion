import type {
  ConstraintRead,
  IssueConstraints,
  IssueProposed,
  LensFinding,
  LensInvoked,
  LensRead,
  NodeLenses,
  NodeStateInput,
  NodeStateWritten,
  NodeVisibility,
  PointerInput,
  PointerWritten,
  PremortemConvertInput,
  PremortemReport,
  PremortemRisk,
  ProposedNode,
  ReasoningAuthor,
  RiskWritten,
} from "@legion/contracts";
import {
  type CommandContext,
  type DispatchToolResult,
  defined,
  optionalBoolean,
  optionalString,
  optionalStringArray,
  type SessionContext,
  stringArg,
} from "./context";
import { refusalWithCode } from "./failures";

/**
 * The commands that write and read the nodes a piece of work is reasoned about with, on
 * Dispatch's node and tuple store. `lens` invokes a lens on a node of an issue, `finding` adds a
 * finding to one, `risk` writes a risk under a pre-mortem, `constraint` a constraint under the
 * issue, `pointer` one pointer tuple out of a node, and `state` and `settle` a node's state;
 * `constraints`, `lenses` and `proposed` read the issue's constraints, the lenses on any node and
 * the issue's proposed nodes; `premortem-convert` turns the issue's pre-mortem document into a
 * lens and its risks. Each write names the node it wrote by its dispatch:// address, which is how
 * every later call names it, and says when that node is the caller's alone. Dispatch refuses these
 * calls by code - LENS_KIND_REQUIRED, INVALID_POINTER, POINTER_ALREADY_ASSERTED,
 * AUTHOR_NAMES_NOBODY, CONSTRAINT_INPUT, NODE_STATE_NOT_WRITABLE, NODE_NOT_PROPOSED,
 * SETTLE_NOT_PERMITTED, ARTIFACT_INPUT and NOT_FOUND among them - and each tool's description
 * names its own, so the code reaches the caller.
 */

/** `visibility` as the schema took it, which holds it to NODE_VISIBILITIES. */
function optionalVisibility(args: Record<string, unknown>): NodeVisibility | undefined {
  return optionalString(args, "visibility") as NodeVisibility | undefined;
}

/** What a line about one written node adds when the node is the caller's alone. */
function authorOnlyNote(visibility: string): string {
  return visibility === "author" ? ", for you alone" : "";
}

/** Who wrote a node, as a line about it names them: the person or the session its authored_by
 *  tuple names, and nobody where the write named neither, as a pod's token alone names only its
 *  workload. */
function authorText(author: ReasoningAuthor | null): string {
  return author === null
    ? "nobody (the write named no person or session)"
    : `${author.kind} ${author.id}`;
}

/** `dispatch lens`: one lens of one kind invoked on a node of the issue, with its findings. */
export async function lensCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let lens: LensInvoked;
  try {
    lens = await client.invokeLens(issueKey, {
      actor,
      on: stringArg(args, "on"),
      kind: stringArg(args, "kind"),
      ...defined("verdict", optionalString(args, "verdict")),
      ...defined("findings", optionalStringArray(args, "findings")),
      ...defined("triggered_by", optionalString(args, "triggered_by")),
      ...defined("visibility", optionalVisibility(args)),
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  // The document renders a Go list as nullable, and Dispatch answers an empty one.
  const findings = lens.findings ?? [];
  return {
    text: [
      `Invoked a ${lens.kind} lens on ${lens.on}: ${lens.address}${authorOnlyNote(lens.visibility)}, written by ${authorText(lens.author)}`,
      ...(lens.triggered_by === null ? [] : [`Triggered by ${lens.triggered_by}`]),
      ...(lens.verdict === null ? [] : [`Verdict: ${lens.verdict}`]),
      ...findings.map(
        (finding) =>
          `- finding ${finding.address}${authorOnlyNote(finding.visibility)}: ${finding.text}`
      ),
    ].join("\n"),
    details: {
      issue: issueKey,
      lens: lens.address,
      kind: lens.kind,
      on: lens.on,
      visibility: lens.visibility,
      findings: findings.map((finding) => finding.address),
    },
  };
}

/** `dispatch finding`: one finding added to a lens, after every finding it holds. */
export async function findingCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let finding: LensFinding;
  try {
    finding = await client.addLensFinding(issueKey, stringArg(args, "lens"), {
      actor,
      text: stringArg(args, "text"),
      ...defined("visibility", optionalVisibility(args)),
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  return {
    text: `Added a finding to ${finding.lens}: ${finding.address}${authorOnlyNote(finding.visibility)}`,
    details: {
      issue: issueKey,
      lens: finding.lens,
      finding: finding.address,
      visibility: finding.visibility,
    },
  };
}

/** `dispatch risk`: one risk under a pre-mortem lens of the issue. */
export async function riskCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let risk: RiskWritten;
  try {
    risk = await client.writeRisk(issueKey, {
      actor,
      lens: stringArg(args, "lens"),
      heading: stringArg(args, "heading"),
      early_warning: stringArg(args, "early_warning"),
      prevention: stringArg(args, "prevention"),
      ...defined("prevented_by", optionalString(args, "prevented_by")),
      ...defined("visibility", optionalVisibility(args)),
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  const preventedBy = risk.prevented_by ?? [];
  return {
    text: [
      `Wrote a risk under ${risk.lens}: ${risk.address}${authorOnlyNote(risk.visibility)}`,
      preventedBy.length === 0
        ? "No check prevents it yet."
        : `Prevented by ${preventedBy.join(", ")}`,
    ].join("\n"),
    details: {
      issue: issueKey,
      lens: risk.lens,
      risk: risk.address,
      prevented_by: preventedBy,
      visibility: risk.visibility,
    },
  };
}

/** A constraint's fields as one line names them: its standard and reference, or its measurement. */
function constraintFields(constraint: ConstraintRead): string {
  if (constraint.standard !== null) {
    return `standard ${JSON.stringify(constraint.standard)}, reference ${JSON.stringify(constraint.reference ?? "")}`;
  }
  return `measurement ${JSON.stringify(constraint.measurement ?? "")}, value ${JSON.stringify(constraint.value ?? "")}, taken ${JSON.stringify(constraint.taken_where ?? "")}`;
}

/** `dispatch constraint`: one constraint under the issue. */
export async function constraintCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let constraint: ConstraintRead;
  try {
    constraint = await client.writeConstraint(issueKey, {
      actor,
      ...defined("standard", optionalString(args, "standard")),
      ...defined("reference", optionalString(args, "reference")),
      ...defined("measurement", optionalString(args, "measurement")),
      ...defined("value", optionalString(args, "value")),
      ...defined("taken_where", optionalString(args, "taken_where")),
      ...defined("visibility", optionalVisibility(args)),
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  return {
    text: `Wrote a constraint under ${issueKey}: ${constraint.address}${authorOnlyNote(constraint.visibility)} (${constraintFields(constraint)})`,
    details: { issue: issueKey, constraint: constraint.address, visibility: constraint.visibility },
  };
}

/** `dispatch constraints`: the constraints under the issue the caller may see. */
export async function constraintsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client, issue } = context;
  const issueKey = issue();
  let listed: IssueConstraints;
  try {
    listed = await client.issueConstraints(issueKey);
  } catch (error) {
    throw refusalWithCode(error);
  }
  const constraints = listed.constraints ?? [];
  const lines =
    constraints.length === 0
      ? [`${listed.issue} holds no constraint you may see.`]
      : [
          `${listed.issue} holds ${constraints.length} constraint${constraints.length === 1 ? "" : "s"} you may see:`,
          ...constraints.map(
            (constraint) =>
              `- ${constraint.address}${authorOnlyNote(constraint.visibility)}: ${constraintFields(constraint)}; written by ${authorText(constraint.author)}; ${constraint.state}`
          ),
        ];
  return {
    text: lines.join("\n"),
    details: {
      issue: listed.issue,
      constraints: constraints.map((constraint) => constraint.address),
    },
  };
}

/** `dispatch pointer`: one pointer tuple out of a node of the issue, appended, retracted or
 *  retargeted. */
export async function pointerCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let written: PointerWritten;
  try {
    written = await client.writePointer(issueKey, stringArg(args, "node"), {
      actor,
      kind: stringArg(args, "kind") as PointerInput["kind"],
      action: stringArg(args, "action") as PointerInput["action"],
      to: stringArg(args, "to"),
      ...defined("from", optionalString(args, "from")),
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  const done =
    written.action === "retarget"
      ? `Retargeted the ${written.kind} pointer of ${written.node.address} from ${written.from?.address ?? ""} to ${written.to.address}`
      : written.action === "retract"
        ? `Retracted the ${written.kind} pointer of ${written.node.address} at ${written.to.address}`
        : `Appended a ${written.kind} pointer from ${written.node.address} to ${written.to.address}`;
  return {
    text: done,
    details: {
      issue: issueKey,
      node: written.node.address,
      kind: written.kind,
      action: written.action,
      to: written.to.address,
      ...(written.from === null ? {} : { from: written.from.address }),
    },
  };
}

/** One lens as `dispatch lenses` lists it: its line, then its verdict, its triggers, its findings
 *  and its risks, indented beneath it. */
function lensLines(lens: LensRead): string[] {
  const kind = lens.kind === null ? "a lens of no kind" : `${lens.kind.id} (${lens.kind.words})`;
  const lines = [
    `- ${lens.address}${authorOnlyNote(lens.visibility)}: ${kind} on ${lens.on ?? "a node you may not see"}, invoked by ${authorText(lens.author)}; ${lens.state}`,
  ];
  if (lens.verdict !== null) lines.push(`  verdict: ${lens.verdict}`);
  // The document renders a Go list as nullable, and Dispatch answers an empty one.
  const triggeredBy = lens.triggered_by ?? [];
  const triggered = lens.triggered ?? [];
  if (triggeredBy.length > 0) lines.push(`  triggered by ${triggeredBy.join(", ")}`);
  if (triggered.length > 0) lines.push(`  triggered ${triggered.join(", ")}`);
  for (const finding of lens.findings ?? []) {
    lines.push(
      `  - finding ${finding.address}${authorOnlyNote(finding.visibility)}: ${finding.text}`
    );
  }
  for (const risk of lens.risks ?? []) {
    const preventedBy = risk.prevented_by ?? [];
    const prevention =
      preventedBy.length === 0 ? "no check prevents it" : `prevented by ${preventedBy.join(", ")}`;
    lines.push(
      `  - risk ${risk.address}${authorOnlyNote(risk.visibility)}: ${risk.heading} (${prevention})`
    );
  }
  return lines;
}

/** `dispatch lenses`: the lenses invoked on one node, in the order they were invoked, and each
 *  chain of triggered lenses they stand in. It acts for nobody, so it resolves no origin. */
export async function lensesCommand(context: SessionContext): Promise<DispatchToolResult> {
  const on = stringArg(context.args, "on");
  let read: NodeLenses;
  try {
    read = await context.newClient().lenses(on);
  } catch (error) {
    throw refusalWithCode(error);
  }
  const node = read.on.address === "" ? on : read.on.address;
  const lenses = read.lenses ?? [];
  const chains = read.chains ?? [];
  const lines: string[] = read.lens === null ? [] : [`${node} is a lens:`, ...lensLines(read.lens)];
  lines.push(
    lenses.length === 0
      ? `${node} holds no lens you may see.`
      : `${node} holds ${lenses.length} lens${lenses.length === 1 ? "" : "es"} you may see, in the order they were invoked:`,
    ...lenses.flatMap(lensLines)
  );
  for (const chain of chains) {
    const members = (chain.lenses ?? []).map((lens) => lens.address);
    const stopped =
      chain.stopped_at === null
        ? ""
        : `; the walk stopped at ${chain.stopped_at}, past the lenses it lists`;
    lines.push(`Chain of ${members.length} triggered lenses: ${members.join(", ")}${stopped}`);
  }
  return {
    text: lines.join("\n"),
    details: {
      on: node,
      ...(read.lens === null ? {} : { lens: read.lens.address }),
      lenses: lenses.map((lens) => lens.address),
      chains: chains.map((chain) => ({
        lenses: (chain.lenses ?? []).map((lens) => lens.address),
        stopped_at: chain.stopped_at,
      })),
    },
  };
}

/** What a write of a node's state answers: the line naming the node and the state it moved from
 *  and to, `verb` saying how it moved, and the two states. */
function stateResult(
  issueKey: string,
  verb: string,
  written: NodeStateWritten
): DispatchToolResult {
  const { node, previous } = written;
  return {
    text:
      previous === node.state
        ? `${node.kind} ${node.address} is already ${node.state}; nothing was written`
        : `${verb} ${node.kind} ${node.address}: ${previous} -> ${node.state}${authorOnlyNote(node.visibility)}`,
    details: { issue: issueKey, node: node.address, state: node.state, previous },
  };
}

/** `dispatch state`: one write of the state of a node of the issue. */
export async function stateCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let written: NodeStateWritten;
  try {
    written = await client.writeNodeState(issueKey, stringArg(args, "node"), {
      actor,
      state: stringArg(args, "state") as NodeStateInput["state"],
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  return stateResult(issueKey, "Wrote the state of", written);
}

/** `dispatch settle`: the settle of one proposed node of the issue. */
export async function settleCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let written: NodeStateWritten;
  try {
    written = await client.settleNode(issueKey, stringArg(args, "node"), { actor });
  } catch (error) {
    throw refusalWithCode(error);
  }
  return stateResult(issueKey, "Settled", written);
}

/** `dispatch proposed`: the nodes of the issue at state proposed that the caller may see. */
export async function proposedCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client, issue } = context;
  const issueKey = issue();
  let read: IssueProposed;
  try {
    read = await client.issueProposed(issueKey);
  } catch (error) {
    throw refusalWithCode(error);
  }
  // The document renders a Go list as nullable, and Dispatch answers an empty one.
  const nodes = read.nodes ?? [];
  const address = (node: ProposedNode) => node.node.ref ?? `${node.node.kind}/${node.node.id}`;
  const lines =
    nodes.length === 0
      ? [`${read.issue} holds no proposed node you may see.`]
      : [
          `${read.issue} holds ${nodes.length} proposed node${nodes.length === 1 ? "" : "s"} you may see:`,
          ...nodes.map(
            (node) =>
              `- ${node.node.kind} ${address(node)}${node.excerpt === null ? "" : `: ${node.excerpt.text}`}`
          ),
        ];
  return {
    text: lines.join("\n"),
    details: { issue: read.issue, proposed: nodes.map(address) },
  };
}

/** `dispatch premortem-convert`: the conversion of the issue's pre-mortem document into a lens of
 *  kind pre-mortem and its risks, once; with `preview`, what it would write, writing nothing. */
export async function premortemConvertCommand(
  context: CommandContext
): Promise<DispatchToolResult> {
  const { args, client, actor, issue } = context;
  const issueKey = issue();
  let report: PremortemReport;
  try {
    report = await client.convertPremortem(issueKey, stringArg(args, "artifact"), {
      actor,
      // The schema took the entries whole: one { risk, checks, reason } for each risk.
      prevented_by: args["prevented_by"] as PremortemConvertInput["prevented_by"],
      dry_run: optionalBoolean(args, "preview") === true,
    });
  } catch (error) {
    throw refusalWithCode(error);
  }
  // The document renders a Go list as nullable, and Dispatch answers an empty one.
  const risks = report.risks ?? [];
  const riskLine = (risk: PremortemRisk) => {
    const preventedBy = risk.prevented_by ?? [];
    return `- risk ${risk.number}${risk.address === null ? "" : ` ${risk.address}`}: ${risk.heading}; ${
      preventedBy.length === 0 ? "prevented by nothing" : `prevented by ${preventedBy.join(", ")}`
    }`;
  };
  return {
    text: [report.summary, ...risks.map(riskLine)].join("\n"),
    details: {
      issue: issueKey,
      document: report.document.address,
      dry_run: report.dry_run,
      already_converted: report.already_converted,
      lens: report.lens.address,
      risks: risks.map((risk) => ({
        number: risk.number,
        address: risk.address,
        prevented_by: risk.prevented_by ?? [],
      })),
    },
  };
}
