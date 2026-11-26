import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  documentBase,
  getArtifactText,
  listIssueArtifacts,
  markdownRoundTrip,
} from "./api";
import { type CliRun, runDispatch } from "./dispatch-cli";
import { expect, test } from "./test";

/**
 * dispatch://INFRA-4224 on the surface an agent runs: `dispatch artifact --dry-run` answers a text
 * an upload refuses for the text alone as the call answers it. Each row of the one list of such
 * texts, `packages/envoy/internal/dispatch/api/testdata/text-refusals.json` (the Go comment above
 * `textRefusalsFile` in `markdown_round_trip_test.go` says what each member and each part is), is
 * run with the real command, its own process, against the harness's real Dispatch: once with
 * `--dry-run` and once without, in every combination of the way the text is given (inline from a
 * file, inline on stdin, and as a `--path` Markdown file, as the row's `sent` and `command` allow),
 * `--force` or not, and a new name or a name the issue already holds.
 *
 * A text every upload refuses alike, a replacement included (`scope` upload), is refused by the
 * call in every combination: the dry run and the call exit 1 and print the same stdout, byte for
 * byte, naming the row's code and words, and nothing is stored. A text a replacement need not
 * refuse alike (`scope` new-document) is refused the same way without `--force`. With `--force`
 * the command's check asks for none of a new document's refusals, and Dispatch answers it as the
 * most permissive replacement takes the text, which the row's `replaced` says. Where that
 * replacement refuses the text too, the dry run and the call are refused alike, in its code and
 * words; where it stores an inline text under another spelling, they are refused alike for that
 * spelling, exit 2, as the call refuses any such text, and print the same stdout but for the id
 * the parse mints, anew on each run, for a block whose id another
 * block names. Everywhere else the dry run passes and the call is refused: the one gap the dry run
 * documents ("What the dry run cannot give" in the pull request, and the `--dry-run` help of
 * `dispatch artifact`).
 *
 * A way the row's `command` names is one the command refuses the text itself in, before it asks
 * Dispatch anything: given inline, a file's or stdin's bytes that are not UTF-8, which it reads as
 * bytes, and which the route tests cannot send inline, since no JSON string carries them. It
 * refuses them as a Markdown `--path` file of the same bytes is refused, in that upload's order:
 * the size first, then the first U+0000 or byte that is not UTF-8. The dry run and the call are
 * refused alike in every combination of it, exit 1, the same stdout naming the row's code and
 * words, and nothing is stored. The file and stdin each hand the command the row's bytes, so a
 * refusal naming the byte (0xFF) is one the command made of that byte, and not of the U+FFFD a
 * lenient read would have stored in its place. The command refuses itself, the same way, a text
 * from a file or stdin past the bytes one document holds or holding U+0000, which the route tests
 * send inline too: its refusal holds the code and words Dispatch gives the same text, so those
 * rows are expected as Dispatch's refusal of the upload.
 */

/** One part of a row's text, as the Go comment above `textRefusalsFile` names the four. */
type TextPart =
  | { readonly text: string }
  | { readonly base64: string }
  | { readonly repeat: string; readonly times: number }
  | { readonly repeat: string; readonly bytes: number };

/** A row of the list, its members named as JSON writes them. */
interface TextRefusalRow {
  readonly name: string;
  readonly text: readonly TextPart[];
  readonly sent: readonly ("inline" | "file")[];
  /** The ways the command refuses the text itself in, before it asks Dispatch: absent on most
   *  rows. */
  readonly command?: readonly "inline"[];
  readonly status: number;
  readonly code: string;
  readonly words: string;
  readonly scope: "upload" | "new-document";
  /** What the most permissive replacement answers, absent where it stores the rendering of the
   *  text: a refusal of its own, or a rendering the parse minted an id in. */
  readonly replaced?: {
    readonly status?: number;
    readonly code?: string;
    readonly words?: string;
    readonly rendering?: string;
  };
}

/** The list, by the relative path the Go route tests read it by from their own package. */
const textRefusalsFile = new URL(
  "../../envoy/internal/dispatch/api/testdata/text-refusals.json",
  import.meta.url
);
const rows = JSON.parse(readFileSync(textRefusalsFile, "utf8")) as readonly TextRefusalRow[];

const utf8 = new TextEncoder();

/** The bytes a row's parts stand for, joined as bytes: a `base64` part is raw bytes, which a
 *  string could not carry, and `{"repeat", "bytes"}` cuts by bytes rather than by characters. A
 *  part that is none of the four fails the row by its index, as `textRefusalText` does. */
function textOf(row: TextRefusalRow): Uint8Array {
  const parts = row.text.map((part, index): Uint8Array => {
    const members = Object.keys(part).sort().join(",");
    if (members === "text" && "text" in part) return utf8.encode(part.text);
    if (members === "base64" && "base64" in part) return Buffer.from(part.base64, "base64");
    if (members === "repeat,times" && "times" in part && part.repeat !== "" && part.times > 0) {
      const unit = utf8.encode(part.repeat);
      return Buffer.alloc(unit.length * part.times, unit);
    }
    if (members === "bytes,repeat" && "bytes" in part && part.repeat !== "" && part.bytes > 0) {
      return Buffer.alloc(part.bytes, utf8.encode(part.repeat));
    }
    throw new Error(
      `${textRefusalsFile.pathname}: part ${index} of ${JSON.stringify(row.name)} is none of ` +
        '{"text": s}, {"base64": s}, {"repeat": s, "times": n} and {"repeat": s, "bytes": n}'
    );
  });
  return Buffer.concat(parts);
}

/** The ways a text is given, each with the member of `sent`, or of `command`, that admits it. */
const ways = [
  { key: "content-file", sent: "inline", label: "--content-file <file>" },
  { key: "stdin", sent: "inline", label: "--content-file - (stdin)" },
  { key: "path", sent: "file", label: "--path <file>.md" },
] as const;

/** The held document's text, which no row's refusal reaches. */
const heldText = "Held.\n";

/** What the issue holds: the name of each document, and the held one's text and token. A refused
 *  call leaves all three as they were, so a document made under the new name, or a version
 *  written over the held one, changes this. */
async function storedOf(
  issue: string,
  held: string | undefined
): Promise<{ names: string[]; held?: { markdown: string; token: string | undefined } }> {
  const names = (await listIssueArtifacts(issue)).map((artifact) => artifact.name).sort();
  if (held === undefined) return { names };
  const { markdown, token } = await getArtifactText(held);
  return { names, held: { markdown, token } };
}

/** A long string as a failure prints it: its length and digest, so a 1 MiB text is no diff. */
function digested(text: string): string {
  return `${Buffer.byteLength(text, "utf8")} bytes, sha256 ${createHash("sha256").update(text).digest("hex")}`;
}

/** The arguments a dry run that passed printed, with the inline text digested. */
function printedArguments(stdout: string): Record<string, unknown> {
  const printed = JSON.parse(stdout) as Record<string, unknown>;
  const content = printed.content;
  return typeof content === "string" ? { ...printed, content: digested(content) } : printed;
}

/** One run as a failure names it, its stdout cut where a 1 MiB text would flood the report. */
function shown(label: string, run: CliRun): string {
  const cut = (text: string) => (text.length > 2_000 ? `${text.slice(0, 2_000)}…` : text);
  return `${label} exited ${run.code}\nstdout: ${cut(run.stdout)}\nstderr: ${cut(run.stderr)}`;
}

/** The markdown the most permissive replacement stores for `markdown`: the round trip's answer
 *  without `new_document`, which is what `--force` asks (requireReplacementAnswer,
 *  markdown_round_trip_test.go). A refusal fails the row, since `markdownRoundTrip` throws on any
 *  answer but a rendering, so a replacement that came to refuse what the row's `replaced` says it
 *  takes is no pass here. */
async function replacementRendering(markdown: string): Promise<string> {
  return (await markdownRoundTrip({ markdown })).markdown;
}

/** An id the parse mints for a block whose id another block already names: a random v4 uuid,
 *  which the list writes "minted" (`mintedBlockIDs`, markdown_round_trip_test.go). A refusal that
 *  prints such a rendering differs in it from one run to the next, so the dry run's stdout and the
 *  call's are compared with each one written "minted", and every other byte as printed. */
const mintedID = /[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}/g;

/** What the dry run and the call of one combination answer, by the row: refused alike, each run
 *  exiting `exit` with stdout holding each of `names`, or the documented gap. */
type Expected =
  | { readonly kind: "refused alike"; readonly exit: number; readonly names: readonly string[] }
  | { readonly kind: "gap" };

async function expectedOf(
  row: TextRefusalRow,
  sent: "inline" | "file",
  byCommand: boolean,
  force: boolean,
  text: Uint8Array,
  rendering: () => Promise<string>
): Promise<Expected> {
  // A way the row's `command` names: the command refuses the text itself, `--force` or not,
  // before it asks Dispatch anything, in Dispatch's code and words. Words that name a character
  // (`holds …`) follow the field the command read, `content`; the size's words name no field.
  if (byCommand) {
    const field = row.words.startsWith("holds ") ? "content " : "";
    return { kind: "refused alike", exit: 1, names: [`${row.code}: ${field}${row.words}`] };
  }
  if (row.scope === "upload" || !force) {
    return { kind: "refused alike", exit: 1, names: [`${row.code}: `, row.words] };
  }
  // --force over a new document's refusal: Dispatch answers as the most permissive replacement
  // takes the text, which refuses it in the row's `replaced` words where those name a refusal,
  // and otherwise stores a rendering the command's inline check compares with the text.
  const replaced = row.replaced;
  if (replaced?.status !== undefined) {
    return { kind: "refused alike", exit: 1, names: [`${replaced.code}: `, replaced.words ?? ""] };
  }
  if (sent === "inline") {
    const given = new TextDecoder().decode(text);
    const stored = await rendering();
    // The store ends every document with a line feed, and that one line feed is the only
    // difference the command's check forgives (`withoutFinalLineFeed`,
    // packages/envoy-client/src/dispatch-commands/stored-text.ts).
    if (stored.replace(/\n$/, "") !== given.replace(/\n$/, "")) {
      // The refusal prints the text Dispatch stores between two marker lines, which is the row's
      // `replaced` rendering where the row names one.
      const rendering =
        replaced?.rendering === undefined
          ? []
          : [
              `--- the text Dispatch stores ---\n${replaced.rendering}--- end of the text Dispatch stores ---`,
            ];
      return {
        kind: "refused alike",
        exit: 2,
        names: ["Dispatch stores this text under another spelling: ", ...rendering],
      };
    }
  }
  return { kind: "gap" };
}

// Each row runs the command with a HOME of its own, where the command keeps its session state, so
// the row removes it afterwards: a run leaves each row's text and none of that state. Bun keeps
// its transpiler cache in the run's own directory instead, which the run removes when it ends
// (`e2e/cli-transpiler-cache.ts`).
test.afterEach(() => {
  rmSync(join(test.info().outputPath(), "home"), { force: true, recursive: true });
});

for (const [index, row] of rows.entries()) {
  // The row's text, and the markdown the most permissive replacement stores for it, each made by
  // the first combination that reads it and shared by the rest, since both depend on the row
  // alone: a text of up to 1 MiB is built once a row, and the round trip asked once a row.
  let built: Uint8Array | undefined;
  const textOfRow = (): Uint8Array => {
    built ??= textOf(row);
    return built;
  };
  let rendered: Promise<string> | undefined;
  const renderingOfRow = (): Promise<string> => {
    rendered ??= replacementRendering(new TextDecoder().decode(textOfRow()));
    return rendered;
  };
  for (const way of ways) {
    const byCommand = way.sent === "inline" && row.command?.includes(way.sent) === true;
    if (!row.sent.includes(way.sent) && !byCommand) continue;
    for (const force of [false, true]) {
      for (const target of ["a new name", "a held name"] as const) {
        // The row, the way, --force and the target, so a failure names all four.
        const combination = `${row.name}: ${way.label}, ${force ? "--force" : "no --force"}, over ${target}`;
        // SLOW(>=2s): two `bun` starts of the real `dispatch` command for the row, each a fresh
        // process that reads its compiled modules from the run's transpiler cache, and texts of
        // up to 1 MiB sent through it.
        test(combination, async () => {
          const output = test.info().outputPath();
          const home = join(output, "home");
          mkdirSync(home, { recursive: true });
          await createProject({ key: "DRY", name: "Dry runs" });
          const issue = await createIssue({ project: "DRY", title: "The dry run and the call" });
          const name = `row-${index}-${way.key}-${force ? "force" : "plain"}-${target === "a held name" ? "held" : "new"}.md`;
          const text = textOfRow();

          // The held name: a document the issue holds under it, with a short text. An upload
          // with --force names the token it replaces.
          let held: string | undefined;
          let base: { readonly document: string } | undefined;
          if (target === "a held name") {
            const created = await createIssueArtifact(issue.key, { content: heldText, name });
            held = created.artifact.id;
            if (force) base = await documentBase(held);
          }
          const before = await storedOf(issue.key, held);

          let source: { args: string[]; given: Record<string, string>; stdin?: Uint8Array };
          if (way.key === "path") {
            const file = join(output, name);
            writeFileSync(file, text);
            source = { args: ["--path", file], given: { path: file } };
          } else {
            const content = { content: digested(new TextDecoder().decode(text)) };
            if (way.key === "stdin") {
              source = { args: ["--content-file", "-"], given: content, stdin: text };
            } else {
              const file = join(output, "content");
              writeFileSync(file, text);
              source = { args: ["--content-file", file], given: content };
            }
          }
          const args = [
            "artifact",
            "--issue",
            issue.key,
            "--name",
            name,
            ...source.args,
            ...(force ? ["--force"] : []),
            ...(base === undefined ? [] : ["--precondition-json", JSON.stringify(base)]),
          ];
          const dry = await runDispatch([...args, "--dry-run"], home, { stdin: source.stdin });
          const call = await runDispatch(args, home, { stdin: source.stdin });
          const both = `${shown("the dry run", dry)}\n${shown("the call", call)}`;
          const expected = await expectedOf(row, way.sent, byCommand, force, text, renderingOfRow);

          if (expected.kind === "gap") {
            // The documented limit, "What the dry run cannot give": with --force the dry run
            // asks for none of a new document's refusals, so it passes, printing the arguments,
            // while the call is refused - over a new name because that upload makes a new
            // document, and over a held one because a replacement holds the rule for the
            // blocks it adds or changes, or, for a rendering past the bytes, because the growth
            // check refuses a rendering that grows past them over the version it replaces. A
            // change that closes or widens the gap fails here.
            expect(dry.code, both).toBe(0);
            expect(printedArguments(dry.stdout), both).toEqual({
              issue: issue.key,
              name,
              ...source.given,
              force: true,
              ...(base === undefined ? {} : { precondition: base }),
            });
            expect(call.code, both).toBe(1);
            expect(call.stdout, both).toContain(`${row.code}: `);
            expect(call.stdout, both).toContain(row.words);
          } else {
            expect(dry.code, both).toBe(expected.exit);
            expect(call.code, both).toBe(expected.exit);
            const printed = call.stdout.replaceAll(mintedID, "minted");
            expect(printed, both).toBe(dry.stdout.replaceAll(mintedID, "minted"));
            for (const named of expected.names) expect(printed, both).toContain(named);
          }
          expect(await storedOf(issue.key, held), both).toEqual(before);
        });
      }
    }
  }
}
