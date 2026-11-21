import type { HeldEditInput, HeldRead, HeldState } from "@legion/contracts";
import type { DispatchWriteKept } from "../dispatch-http";
import { type CommandContext, type DispatchToolResult, optionalString, stringArg } from "./context";

/**
 * The commands of a text Dispatch's reviewer holds (INFRA-3231's R5): `dispatch held` reads it,
 * `dispatch held-edit` changes it, which the reviewer reads again, and `dispatch held-post` posts
 * it as written with a reason. The frame that brings the reviewer's findings names the last two.
 */

/** The wording of a held text as its reader would see it. */
function wordingLines(wording: HeldRead["wording"]): string[] {
  const lines: string[] = [];
  if (wording.title !== undefined && wording.title !== "") lines.push(`Title: ${wording.title}`);
  lines.push(wording.body);
  for (const option of wording.options ?? []) {
    lines.push(
      `- ${option.label}${option.description === undefined || option.description === "" ? "" : `: ${option.description}`}`
    );
  }
  return lines;
}

/** Until when the writer of a held text has to act on it. */
function actBy(deadline: string): string {
  return `You have until ${deadline} to edit it or post it.`;
}

/** The held text a held text posts after. */
function postsAfter(waitsOn: string): string {
  return `It posts after held text ${waitsOn}.`;
}

/** What a held text's lines say of when it posts: until when its writer has to act on it, while
 *  it waits on the writer, and the held text it posts after, where it waits on one. */
function heldTiming(held: HeldState): string[] {
  return [
    ...(held.deadline !== null && held.state === "awaiting_writer" ? [actBy(held.deadline)] : []),
    ...(held.waits_on === undefined || held.waits_on === "" ? [] : [postsAfter(held.waits_on)]),
  ];
}

/** The held text a write waits on when the reviewer does not read it (the server answers
 *  `posting` with `waits_on`): Dispatch holds such a write only until that text posts, and posts
 *  it right after. Undefined for a write the reviewer reads. */
function waitsOnlyOn(held: HeldState): string | undefined {
  return held.state === "posting" && held.waits_on !== undefined && held.waits_on !== ""
    ? held.waits_on
    : undefined;
}

/** What holds a write, in the words that follow `held`: the review, with its state, or only the
 *  held text the write waits on. */
export function heldFor(held: HeldState): string {
  const parent = waitsOnlyOn(held);
  return parent === undefined ? `for review (${held.state})` : `until held text ${parent} posts`;
}

/** What opens each line that reports a write Dispatch holds, where a write that posted at once
 *  says `Posted`: the first line of the write's result, and its cadence line. */
export function heldOpening(held: HeldState): string {
  return `Held ${heldFor(held)}`;
}

/**
 * The first line of a write Dispatch holds, which the server answers with the id it reserved and
 * `held`: no person sees the text until it posts, so the line never says it posted, was sent, was
 * asked or was created. It names what holds the text, its kind and its id; `names`, what the
 * command's own line names beside the id, with its own leading separator ("" for nothing); when it
 * posts (heldTiming); and `dispatch held <id>`, with the reviewer's findings where the reviewer
 * reads the text. A text that waits only on another held text gets no findings.
 */
export function heldWriteLine(held: HeldState, kind: string, id: string, names: string): string {
  return [
    `${heldOpening(held)}: ${kind} ${id}${names}. No person sees it until it posts.`,
    ...heldTiming(held),
    waitsOnlyOn(held) === undefined
      ? `\`dispatch held ${id}\` reads it and what the reviewer found; a finding also reaches you as a message of its own.`
      : `\`dispatch held ${id}\` reads it.`,
  ].join(" ");
}

/**
 * What a command prints for its write Dispatch kept beside a held text its path names
 * (`DispatchWriteKept`): Dispatch accepted the write and runs it right after that text posts, so
 * the command did what it was asked.
 */
export function keptWriteResult(kept: DispatchWriteKept): DispatchToolResult {
  return {
    text: `Kept ${heldFor(kept.held)}: Dispatch keeps this write as ${kept.id} and runs it right after that text posts. \`dispatch held ${kept.id}\` reads it.`,
    details: { kept: kept.id, held: kept.held },
  };
}

/** What a held text's read prints: its state, its wording, the findings and the rewrite. */
function heldText(held: HeldRead): string {
  const lines = [`Held ${held.kind} ${held.id}: ${held.state}.`, ...heldTiming(held)];
  if (held.outcome !== undefined && held.outcome !== "") lines.push(`Outcome: ${held.outcome}.`);
  lines.push("", "The text:", ...wordingLines(held.wording));
  const findings = held.findings ?? [];
  if (findings.length > 0) {
    lines.push("", "The reviewer's findings:");
    findings.forEach((finding, index) => {
      lines.push(
        `${index + 1}. "${finding.quote}" (${finding.rule_set}): ${finding.why} Fix: ${finding.fix}`
      );
    });
  }
  if (held.rewrite !== null) {
    lines.push("", "The reviewer's rewrite:", ...wordingLines(held.rewrite));
    if (held.not_applied !== undefined && held.not_applied !== "") {
      lines.push(`Dispatch cannot post this rewrite as it is: ${held.not_applied}.`);
    }
  }
  return lines.join("\n");
}

/** `dispatch held`: read a text the reviewer holds. */
export async function heldCommand(context: CommandContext): Promise<DispatchToolResult> {
  const held = await context.client.getHeld(stringArg(context.args, "held"));
  return { text: heldText(held), details: { held } };
}

/** `dispatch held-edit`: change a held text, which the reviewer reads again. */
export async function heldEditCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor } = context;
  const options = args.options;
  // Each read once, so the check below narrows what the spread writes (a field set to undefined
  // is not a field left out, under exactOptionalPropertyTypes).
  const question = optionalString(args, "question");
  const title = optionalString(args, "title");
  const spec = optionalString(args, "spec");
  const body = optionalString(args, "body");
  const input: HeldEditInput = {
    ...(question === undefined ? {} : { question }),
    ...(Array.isArray(options)
      ? { options: options as NonNullable<HeldEditInput["options"]> }
      : {}),
    ...(title === undefined ? {} : { title }),
    ...(spec === undefined ? {} : { spec }),
    ...(body === undefined ? {} : { body }),
    actor,
  };
  const held = await client.editHeld(stringArg(args, "held"), input);
  return {
    text: `Edited held ${held.kind} ${held.id}; the reviewer reads it again.\n\n${heldText(held)}`,
    details: { held },
  };
}

/** `dispatch held-post`: post a held text as written, with the reason it is right as written. */
export async function heldPostCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor } = context;
  const held = await client.postHeld(stringArg(args, "held"), {
    reason: stringArg(args, "reason"),
    actor,
  });
  return {
    text:
      held.state === "posted"
        ? `Posted ${held.kind} ${held.id} as written.`
        : `Held ${held.kind} ${held.id} is ${held.state}${held.waits_on ? `: it posts after held text ${held.waits_on}` : ""}.`,
    details: { held },
  };
}
