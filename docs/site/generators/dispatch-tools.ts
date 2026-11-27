#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/tools.md: one section per Dispatch spec
// (packages/contracts/src/dispatch-tools.ts), each the command's description, its flags and its
// example as a command line, all from packages/envoy-client/src/dispatch-command.ts, the code
// `dispatch <command> --help` prints them with, so the page and the help cannot drift. Then one
// section for each command no spec defines (`COMMANDS_OUTSIDE_THE_SPECS` in dispatch-command.ts:
// `dispatch proxy` and `dispatch login`): its help, as `dispatch <command> --help` prints it.
// Contract: scripts/generate.ts.
import { dispatchToolSpecs } from "../../../packages/contracts/src/dispatch-tools.ts";
import {
  COMMANDS_OUTSIDE_THE_SPECS,
  commandFlagTable,
  commandLine,
  commandName,
} from "../../../packages/envoy-client/src/dispatch-command.ts";
import { fence, inline, writePage } from "./lib/markdown.ts";

function render(): string {
  const outside = COMMANDS_OUTSIDE_THE_SPECS.map(({ name }) => `\`dispatch ${name}\``).join(", ");
  const lines = [
    `Agents work in Dispatch through the \`dispatch\` command in their shell. It has ${dispatchToolSpecs.length + COMMANDS_OUTSIDE_THE_SPECS.length} commands, each with the flags and example \`dispatch <command> --help\` prints. A flag \`--<name>-file <path>\` reads the value from a file, and \`-\` reads stdin. Only one flag of a call may read stdin. The command refuses \`--<name> -\` for each text flag except \`dispatch artifact --path\`, whose value names a local file. A flag the command does not list is refused. A command that a spec defines exits 0 when it wrote or read. It exits 1 when Dispatch or AskGate refused it, Dispatch failed, the command could not reach Dispatch, its trade for a token at the secrets broker failed, or Dispatch kept no answer for a call that writes nothing. It exits 2 on a usage error, such as flags it refused before any request, and on a session or configuration it cannot use. Each command that no spec defines (${outside}) gives its exit statuses in its help, below.`,
    "",
  ];
  for (const spec of dispatchToolSpecs) {
    lines.push(`## dispatch ${commandName(spec.name)}`, "", inline(spec.description), "");
    lines.push("| Flag | What it sends |", "| --- | --- |");
    for (const row of commandFlagTable(spec.name)) {
      const text = inline(row.text, true).replace(/\s*\n\s*/g, " ");
      lines.push(`| ${inline(`\`${row.flag}\``, true)} | ${text} |`);
    }
    lines.push("");
    if ("validation" in spec && spec.validation) {
      lines.push(`**Rule:** ${inline(spec.validation.message)}`, "");
    }
    const example = commandLine(spec.name, spec.example as Record<string, unknown>);
    lines.push("Example:", "", "```sh", example, "```", "");
  }
  for (const { name, help } of COMMANDS_OUTSIDE_THE_SPECS) {
    const text = help().trimEnd();
    const helpFence = fence(text);
    lines.push(
      `## dispatch ${name}`,
      "",
      `No Dispatch spec defines \`dispatch ${name}\`. Its flags, its example and what it does are in its help, which \`dispatch ${name} --help\` prints:`,
      "",
      `${helpFence}text`,
      text,
      helpFence,
      ""
    );
  }
  return lines.join("\n");
}

writePage({
  path: "dispatch/reference/tools.md",
  title: "The dispatch command",
  description:
    "Every dispatch command an agent runs: what it does, its flags, and an example command line.",
  sources: [
    "packages/contracts/src/dispatch-tools.ts",
    "packages/envoy-client/src/dispatch-command.ts",
    "packages/envoy-client/src/dispatch-proxy-command.ts",
    "packages/envoy-client/src/dispatch-login-command.ts",
  ],
  body: render,
});
