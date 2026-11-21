import type { Artifact } from "@legion/contracts";
import { ARTIFACT_NAME_EXISTS_RULE, PATH_KEEPS_BYTES, pathKeepsBytesOf } from "@legion/contracts";
import { actorText } from "../actor-text";
import { DispatchAnswerMissingError } from "../dispatch-http";
import { messageFor } from "../errors";
import { dispatchAnswered } from "./failures";

/** The document an upload under `name` would write the next version of: the owner's artifact
 *  whose slug or filename that name is, as the upload itself resolves one (`$2 in (name, slug)`,
 *  the slug first). Undefined where the owner holds no such document. */
export function artifactUnderName(
  artifacts: readonly Artifact[],
  name: string
): Artifact | undefined {
  const written = name.trim();
  return (
    artifacts.find((held) => held.slug === written) ??
    artifacts.find((held) => held.name === written)
  );
}

/** The refusal of an upload under a name its owner already holds: the document found, its kind,
 *  its newest version and who wrote that version, then the rule that names --force. */
export function artifactNameExistsProblem(held: Artifact, owner: string): string {
  const versions = held.versions ?? [];
  const newest = versions[versions.length - 1];
  if (newest === undefined) {
    return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}), which has no version yet and which ${actorText(held.created_by)} created. ${ARTIFACT_NAME_EXISTS_RULE}`;
  }
  const authors = (newest.authors ?? []).map(actorText);
  const wroteIt = authors.length === 0 ? actorText(held.created_by) : authors.join(" and ");
  return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}) at version ${newest.number}, which ${wroteIt} wrote. ${ARTIFACT_NAME_EXISTS_RULE}`;
}

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
export function storedTextProblem(given: string, stored: string, name: string): string {
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

/**
 * The refusal of an upload whose stored-text check could not run. The check is the one thing that
 * says whether the store would write this text under another spelling, so a failure that is not
 * the route's absence leaves that question open, and an upload sent anyway would store the
 * changed text this check exists to stop.
 *
 * A body Dispatch itself refused as too large is the one such failure a repeat can never get
 * through, so that refusal names what to do instead of saying to send the upload again: the
 * text is past what the check reads, and a name whose extension keeps the bytes has `--path`,
 * which stores the file and asks this route nothing.
 */
export function storedTextCheckFailed(error: unknown, name: string): string {
  // The round trip stores nothing, so a 2xx with no text in it (`DispatchAnswerMissingError`) says
  // only that. Any other failure is named as it reads, its own final period left to this sentence.
  const failed =
    error instanceof DispatchAnswerMissingError
      ? `answered without the text it would store (${error.code})`
      : `failed with ${messageFor(error).replace(/\.$/, "")}`;
  const failure = `Dispatch could not say how it would store this text, so nothing was uploaded: POST /api/v1/markdown/round-trip ${failed}.`;
  if (!dispatchAnswered(error, 413)) {
    return `${failure} Send the upload again once that call is answered.`;
  }
  // Only a body past markdownRoundTripLimit reaches here, and that is always text past the
  // megabyte a markdown document may hold, so every remedy this names has to be one that
  // takes the text out of one document: a document of its own is no remedy at all, since the
  // upload refused is already one and a second would be refused the same way.
  const asBytes = pathKeepsBytesOf(name)
    ? ` Send it as a file with ${PATH_KEEPS_BYTES}, which asks this check nothing.`
    : "";
  return `${failure} This text is longer than the check reads, and sending the upload again does not change that: a markdown document is at most 1 MiB, so shorten it or split it across documents of at most that.${asBytes}`;
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
export function roundTripRouteAbsent(error: unknown): boolean {
  return dispatchAnswered(error, 404) || dispatchAnswered(error, 405);
}

/** `text` without a final line feed, which is what the stored-text check compares. The store
 *  ends every document it holds with one, so text a writer gave without one comes back with one
 *  and nothing else changed (`markdown_round_trip_test.go` pins "one" stored as "one\n"). That
 *  one line feed is the only difference the check forgives. */
export function withoutFinalLineFeed(text: string): string {
  return text.endsWith("\n") ? text.slice(0, -1) : text;
}
