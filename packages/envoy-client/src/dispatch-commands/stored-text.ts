// The stored-text check of an upload Dispatch stores as a Markdown document: whether the call
// refuses its text for its size, and, for inline text, whether Dispatch would store the text under
// another spelling, read from the store's own parse and render. `dispatch artifact` and its dry run
// both make it (`artifactCommand` and `artifactDryRun` in documents.ts), so the two answer alike.
import {
  MARKDOWN_DOCUMENT_MAX_BYTES,
  MARKDOWN_DOCUMENT_TOO_LARGE,
  PATH_KEEPS_BYTES,
  pathKeepsBytesOf,
  TEXT_REFUSAL_NEW_DOCUMENT,
  utf8Bytes,
} from "@legion/contracts";
import { goBlankPattern } from "@legion/contracts/go-blank";
import type { BunFile } from "bun";
import { type DispatchClient, DispatchServiceError } from "../dispatch-http";
import { messageFor } from "../errors";
import { ToolInputError } from "../tool-input-errors";
import { dispatchAnswered } from "./failures";

/**
 * How much of one line a refusal quotes whole. A line of a document is bounded by the document
 * itself, so one differing line can be as long as the whole text, and a quote nobody can read
 * to the end is one nobody acts on — the same reason STORED_TEXT_PRINTED bounds the stored
 * text below, and the cut is named the same way.
 */
const STORED_LINE_QUOTED = 200;

/** `line` as a refusal quotes it: its JSON spelling whole while the line is short, and
 *  otherwise that of its first STORED_LINE_QUOTED characters, with the cut named after it. */
function quotedStoredLine(line: string): string {
  if (line.length <= STORED_LINE_QUOTED) return JSON.stringify(line);
  const head = JSON.stringify(line.slice(0, STORED_LINE_QUOTED));
  return `${head} (cut after ${STORED_LINE_QUOTED} of its ${line.length} characters)`;
}

/** The first line the store would write otherwise, which is the line a reader fixes first. The
 *  two texts differ, so one of the three arms answers. Each quote is bounded, since the line it
 *  names can be as long as the document itself. */
function firstStoredDifference(given: string, stored: string): string {
  const written = given.split("\n");
  const holds = stored.split("\n");
  // Each side is read as `string | undefined`, since a line past the end of its own text is
  // undefined and the strict packages that compile this file say so. The two are not equal
  // here, so exactly one arm below answers and the pair can never both be undefined.
  for (let line = 0; line < Math.max(written.length, holds.length); line += 1) {
    const wrote = written[line];
    const held = holds[line];
    if (wrote === held) continue;
    if (wrote === undefined && held !== undefined) {
      return `the store writes a line ${line + 1}, ${quotedStoredLine(held)}`;
    }
    if (held === undefined && wrote !== undefined) {
      return `the store drops line ${line + 1}, ${quotedStoredLine(wrote)}`;
    }
    if (wrote !== undefined && held !== undefined) {
      return `line ${line + 1} reads ${quotedStoredLine(wrote)} and is stored as ${quotedStoredLine(held)}`;
    }
  }
  return "the stored text differs from the text given";
}

/**
 * How much of the stored text a refusal prints whole. Past it the refusal prints the first whole
 * lines that fit and says how much it cut: a document is up to 1 MiB, and a refusal nobody can
 * read to the end is one nobody acts on. The line a reader fixes first is named before the text
 * either way, so a cut refusal still says what to change.
 */
const STORED_TEXT_PRINTED = 2000;

/** The stored text as a refusal prints it, and what the refusal says of the cut: whole while it
 *  is short, and otherwise its first whole lines up to STORED_TEXT_PRINTED characters. */
function storedTextPrinted(stored: string): { readonly text: string; readonly cut: string } {
  if (stored.length <= STORED_TEXT_PRINTED) return { text: stored, cut: "" };
  const head = stored.slice(0, STORED_TEXT_PRINTED);
  const lastLineFeed = head.lastIndexOf("\n");
  const text = lastLineFeed === -1 ? head : head.slice(0, lastLineFeed + 1);
  return {
    text,
    cut: ` The text below is cut after ${text.length} of its ${stored.length} characters: change the line named above and send the text again.`,
  };
}

/**
 * The refusal of inline text the store would change: where the two first differ, the way out
 * this name has, and the text the store writes, so the writer can send exactly that text. The
 * markers hold the stored text apart from the prose around it. A name `--path` would store as
 * bytes has a second way out, which keeps the text as it was written; a Markdown name has none,
 * since `--path` parses and renders it the same way (`pathKeepsBytesOf`).
 */
function storedTextProblem(given: string, stored: string, name: string): string {
  const asBytes = pathKeepsBytesOf(name)
    ? ` To keep the text as you wrote it, send it as a file with ${PATH_KEEPS_BYTES}.`
    : "";
  const printed = storedTextPrinted(stored);
  return [
    `Dispatch stores this text under another spelling: ${firstStoredDifference(given, stored)}.`,
    ` Send the text Dispatch stores, which is between the two marker lines below.${asBytes}`,
    `${printed.cut}\n--- the text Dispatch stores ---\n${printed.text}`,
    "--- end of the text Dispatch stores ---",
  ].join("");
}

/** What a refusal or a warning says of a round trip that failed with `error`. */
export function roundTripFailure(error: unknown): string {
  return `POST /api/v1/markdown/round-trip failed with ${messageFor(error)}`;
}

/**
 * The refusal of an upload whose stored-text check could not run. The check is the one thing that
 * says whether the store would write this text under another spelling, so a failure that is not
 * the route's absence leaves that question open, and an upload sent anyway would store the
 * changed text this check exists to stop. A text Dispatch refused, its answer marked as a refusal
 * of the text (`TEXT_REFUSAL_HEADER`), is no such failure: the check answers it with Dispatch's own
 * refusal (`askRoundTrip`). Blank text has no other spelling to stop, so its failure refuses
 * nothing (`artifactCommand`).
 */
export function storedTextCheckFailed(error: unknown): Error {
  return new Error(
    `Dispatch could not say how it would store this text, so nothing was uploaded: ${roundTripFailure(error)}. Send the upload again once that call is answered.`,
    { cause: error }
  );
}

/**
 * Whether the round trip failed because Dispatch itself holds no such route: the 404 or 405 a
 * Dispatch older than POST /api/v1/markdown/round-trip answers, as its own JSON. Such a server
 * cannot say what it would store, and refused the upload itself before this check existed, so
 * the upload goes on and that server makes its own answer.
 *
 * A 404 or 405 page from an ingress, a proxy or a load balancer is `DispatchGatewayError`,
 * which says nothing about the route, so `dispatchAnswered` is what this asks: reading such a
 * page as an absent route would skip the check and store the respelled text it exists to stop.
 */
function roundTripRouteAbsent(error: unknown): error is DispatchServiceError {
  return dispatchAnswered(error, 404) || dispatchAnswered(error, 405);
}

/** `text` without a final line feed, which is what the stored-text check compares. The store
 *  ends every document it holds with one, so text a writer gave without one comes back with one
 *  and nothing else changed (`markdown_round_trip_test.go` pins "one" stored as "one\n"). That
 *  one line feed is the only difference the check forgives. */
function withoutFinalLineFeed(text: string): string {
  return text.endsWith("\n") ? text.slice(0, -1) : text;
}

/**
 * What Dispatch says of the text an upload would store as a Markdown document (`storedTextCheck`
 * for inline text, `storedFileCheck` for a `--path` file):
 * - `kept`: the store takes the text: inline, as given but for one final line feed, or blank,
 *   which is held to no spelling; from a file, however the store spells it;
 * - `refused`: the call refuses the text, and `refusal` is what it throws for it: a
 *   `ToolInputError` for inline text the store writes under another spelling
 *   (`storedTextProblem`), an `Error` in Dispatch's words for text past the bytes one document
 *   may hold (`documentTooLarge`), and Dispatch's own refusal of the round trip, which is its
 *   upload's refusal of the same text (`askRoundTrip`);
 * - `new-document`: Dispatch refuses the text as a new document's seed does, and the upload asks
 *   `--force`, which may replace a document Dispatch judges the text against, by looser rules
 *   (`askRoundTrip`): the call uploads the text, and Dispatch answers;
 * - `no-route`: this Dispatch holds no round-trip route (`roundTripRouteAbsent`), so the upload
 *   goes on unchecked and the server makes its own answer;
 * - `failed`: any other failure, which refuses an inline upload whose text is not blank
 *   (`storedTextCheckFailed`).
 */
export type StoredTextCheck =
  | { readonly kind: "kept" }
  | { readonly kind: "refused"; readonly refusal: Error }
  | { readonly kind: "new-document"; readonly refusal: DispatchServiceError }
  | { readonly kind: "no-route"; readonly error: DispatchServiceError }
  | { readonly kind: "failed"; readonly error: unknown };

/**
 * The refusal of a Markdown document's text past MARKDOWN_DOCUMENT_MAX_BYTES, or undefined for
 * text within it. `measured` is the text, inline text counted in UTF-8 as the server counts the
 * text it is sent (`utf8Bytes`), never in the UTF-16 units of its `length`; a `--path` file's
 * bytes as they are sent; or a file's size in bytes before it is read. The refusal is the server's
 * own `413 CAP_EXCEEDED` in its own words (MARKDOWN_DOCUMENT_TOO_LARGE), which `messageFor` prints
 * as it prints that answer, for an upload within the server's 25 MiB file cap; past that cap the
 * server answers `artifact blob exceeds 25 MB` instead (errUploadTooLarge). The server refuses
 * such an upload whatever name it carries, so `dispatch artifact` refuses such text first: the
 * call before it reads whether the owner holds the name (`artifactCommand`), and the call and its
 * dry run before they ask the round trip (`storedTextCheck`, `storedFileCheck`).
 */
export function documentTooLarge(measured: string | Uint8Array | number): Error | undefined {
  const bytes =
    typeof measured === "number"
      ? measured
      : typeof measured === "string"
        ? utf8Bytes(measured)
        : measured.byteLength;
  return bytes > MARKDOWN_DOCUMENT_MAX_BYTES
    ? new Error(`CAP_EXCEEDED: ${MARKDOWN_DOCUMENT_TOO_LARGE}`)
    : undefined;
}

/**
 * The bytes of an upload's `--path` file that Dispatch stores as a Markdown document, or undefined
 * for a file it stores as its bytes, which is not read here. The call sends `file` under the type
 * Bun gives its path (`file.type`), `text/markdown` for a path ending in .md or .markdown
 * (PATH_KEEPS_BYTES), and Dispatch stores an upload as a document exactly when its file part's
 * media type is `text/markdown`, its parameters and its case aside (`artifactKind`,
 * packages/envoy/internal/dispatch/api/artifacts.go). The bytes are the ones the call checks and
 * sends: `held`, those the CLI read for AskGate's check where it read them, or else the file's own,
 * read once. A file whose size is past the bytes one document holds is refused before it is read,
 * with `documentTooLarge`'s refusal, which this throws; a size that cannot be read leaves the
 * answer to the read, which throws what it throws, and to the caller's check of what it read.
 */
export async function documentFileBytes(
  file: BunFile,
  held?: Uint8Array<ArrayBuffer>
): Promise<Uint8Array<ArrayBuffer> | undefined> {
  if (file.type.split(";", 1)[0]?.trim().toLowerCase() !== "text/markdown") return undefined;
  const size =
    held?.byteLength ??
    (await file.stat().then(
      (stats) => stats.size,
      () => undefined
    ));
  const tooLarge = size === undefined ? undefined : documentTooLarge(size);
  if (tooLarge !== undefined) throw tooLarge;
  return held ?? (await file.bytes());
}

/** What the round trip answers for a text: the markdown Dispatch would store for it, or what the
 *  check makes of its failure. */
type RoundTripAnswer =
  | { readonly kind: "stored"; readonly stored: string }
  | Exclude<StoredTextCheck, { readonly kind: "kept" }>;

/**
 * Asks the round trip (POST /api/v1/markdown/round-trip, which writes nothing) through `ask`: the
 * text inline (`markdownRoundTrip`) or a file's bytes (`fileRoundTrip`). `client` is called once,
 * for the client the round trip goes through, and what it throws is thrown; every failure of the
 * round trip is returned.
 *
 * An answer Dispatch marks as its refusal of the text (`TEXT_REFUSAL_HEADER`, read into
 * `DispatchServiceError.textRefusal`), whatever its status, is the call's refusal where its scope
 * holds for the call, as Dispatch worded it, printed as the call prints the upload's refusal
 * (`CAP_EXCEEDED: <message>`, `INVALID_MARKDOWN: <message>`). The route marks every refusal of the
 * checks an upload of a new document makes of the text alone, which it makes in that upload's own
 * order, status, code and words, so the call and its dry run refuse such a text as the upload
 * would, and a check Dispatch adds to that upload, at any status, reaches them with no change here.
 * Its scope says which uploads it holds for:
 * - TEXT_REFUSAL_UPLOAD, a check every upload of a markdown document makes before its write (the
 *   text's encoding, its bytes): the call's refusal whatever it asks;
 * - TEXT_REFUSAL_NEW_DOCUMENT, a check a new document's seed makes (the parse, the asks' content
 *   rule, the rendering's bytes and elements, any failure of the seed on the text): the call's
 *   refusal unless `force`, the upload's `--force`, which may replace a document. A replacement
 *   is judged against the document it replaces, by looser rules (an ask the write leaves unchanged
 *   is not held to its content rule, a rendering no heavier than the one it replaces passes), so
 *   under `--force` such a refusal is `new-document`: the call uploads the text, and Dispatch
 *   answers.
 *
 * Every unmarked failure says nothing of the text: a 404 or 405 is a Dispatch without the route
 * (`roundTripRouteAbsent`), and anything else - the route's own refusal of its body, a 401, a 403,
 * a 429, a 5xx of Dispatch's own, a gateway's page or no answer at all - is `failed`.
 */
async function askRoundTrip(
  client: () => DispatchClient,
  ask: (dispatch: DispatchClient) => Promise<string>,
  force: boolean
): Promise<RoundTripAnswer> {
  const dispatch = client();
  try {
    return { kind: "stored", stored: await ask(dispatch) };
  } catch (error) {
    if (roundTripRouteAbsent(error)) return { kind: "no-route", error };
    if (error instanceof DispatchServiceError && error.textRefusal !== undefined) {
      return force && error.textRefusal === TEXT_REFUSAL_NEW_DOCUMENT
        ? { kind: "new-document", refusal: error }
        : { kind: "refused", refusal: error };
    }
    return { kind: "failed", error };
  }
}

/**
 * The stored-text check of inline `content` uploaded under `name` by `tool`, `force` its
 * `--force`. `dispatch artifact` and its dry run both make it, so the two never answer
 * differently. Inline text is stored as Markdown: Dispatch parses it and stores its rendering, so
 * a text it would write under another spelling is refused rather than stored changed, and a text
 * Dispatch refuses is refused with its refusal where it holds for the upload (`askRoundTrip`).
 *
 * Text past MARKDOWN_DOCUMENT_MAX_BYTES is `refused` (`documentTooLarge`) without calling
 * `client`, since the server refuses an upload of such text whatever it holds, so nothing is asked
 * for it and a dry run of it builds no client. The empty string is `kept` without calling `client`
 * too: it is a create of an empty document, which no check of the text refuses.
 *
 * Any other blank text (`goBlankPattern`) is asked, and only a refusal counts: the upload refuses
 * blank text as it refuses any, too many elements among it, so `refused` is returned as it came,
 * and every answer is `kept`, never refused for its spelling. The round trip answers "" for every
 * text that pattern calls blank and that it does not refuse, since markdownRoundTrip answers ""
 * for whatever `strings.TrimSpace` empties (that package's markdown_round_trip.go), and that short
 * answer is not what the store writes for each of them: the parse and render a seed runs keep
 * every character of this set but a tab, a line feed, a carriage return and a space, each of the
 * rest rendering to itself.
 */
export async function storedTextCheck(
  client: () => DispatchClient,
  tool: string,
  name: string,
  content: string,
  force: boolean
): Promise<StoredTextCheck> {
  const tooLarge = documentTooLarge(content);
  if (tooLarge !== undefined) return { kind: "refused", refusal: tooLarge };
  if (content === "") return { kind: "kept" };
  const answer = await askRoundTrip(
    client,
    (dispatch) => dispatch.markdownRoundTrip(content),
    force
  );
  if (answer.kind !== "stored") return answer;
  if (goBlankPattern.test(content)) return { kind: "kept" };
  // The store ends a document with a line feed the writer's own text need not carry, and that
  // one line feed is no other spelling: both sides lose at most one before they are compared.
  // Every other difference is refused.
  return withoutFinalLineFeed(answer.stored) === withoutFinalLineFeed(content)
    ? { kind: "kept" }
    : {
        kind: "refused",
        refusal: new ToolInputError(tool, [storedTextProblem(content, answer.stored, name)]),
      };
}

/**
 * The stored-text check of a `--path` file Dispatch stores as a Markdown document, `bytes` its
 * content (`documentFileBytes`) and `force` the upload's `--force`, which `dispatch artifact
 * --dry-run` makes. Bytes past MARKDOWN_DOCUMENT_MAX_BYTES are `refused` without calling `client`
 * (`documentTooLarge`), as the call refuses them before it reads whether the owner holds the name.
 * Otherwise the round trip is asked about the file's bytes as they are (`fileRoundTrip`), and only
 * its refusal is taken, where it holds for the upload (`askRoundTrip`): Dispatch refuses there, in
 * the upload's own words, the bytes the upload of that file refuses for the text alone - a NUL, a
 * byte that is not UTF-8, the parse's refusal, an ask that breaks its content rule, a text or a
 * rendering too large - and the call's upload of the file is refused so. Whatever spelling it
 * answers is `kept`: the call stores a Markdown file as the store spells it, with no refusal for
 * its spelling. The call itself asks the round trip nothing about a file.
 */
export async function storedFileCheck(
  client: () => DispatchClient,
  bytes: Uint8Array,
  force: boolean
): Promise<StoredTextCheck> {
  const tooLarge = documentTooLarge(bytes);
  if (tooLarge !== undefined) return { kind: "refused", refusal: tooLarge };
  const answer = await askRoundTrip(client, (dispatch) => dispatch.fileRoundTrip(bytes), force);
  return answer.kind === "stored" ? { kind: "kept" } : answer;
}
