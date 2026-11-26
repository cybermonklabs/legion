// The stored-text check of an upload Dispatch stores as a Markdown document: whether the call
// refuses its text for its size, and, for inline text, whether Dispatch would store the text under
// another spelling, read from the store's own parse and render. `dispatch artifact` and its dry run
// both make it, chosen by one function (`artifactTextCheck`, which `artifactCommand` and
// `artifactDryRun` in documents.ts call), so the two answer alike. A document's bytes, a `--path`
// file's or those of inline text, given in the argument itself or read from a file or stdin, are
// refused in the upload's order, their size first and then the first U+0000 or byte that is not
// UTF-8, by one function (`documentBytesRefusal`).
import { resolve as resolvePath } from "node:path";
import {
  MARKDOWN_DOCUMENT_MAX_BYTES,
  MARKDOWN_DOCUMENT_TOO_LARGE,
  uploadExtensionOf,
  utf8Bytes,
} from "@legion/contracts";
import { goBlankPattern } from "@legion/contracts/go-blank";
import type { BunFile } from "bun";
import { uploadIsDocument } from "../askgate-marker";
import { fileTextBytes, unstorableBytes } from "../dispatch-file-text";
import {
  DispatchAnswerNotKeptError,
  type DispatchClient,
  DispatchServiceError,
} from "../dispatch-http";
import { messageFor } from "../errors";
import { ToolInputError } from "../tool-input-errors";
import { dispatchAnswered } from "./failures";
import { storedUpload } from "./stored-spelling";

/** The two round trips the stored-text check asks, as a refusal or a warning names them: inline
 *  text's and a `--path` file's. Each writes nothing. */
const MARKDOWN_ROUND_TRIP = "POST /api/v1/markdown/round-trip";
const FILE_ROUND_TRIP = "POST /api/v1/markdown/file-round-trip";

/**
 * The refusal of an upload whose stored-text check could not run, `failed` its check
 * (`storedTextCheck`). The check is the one thing that says whether the store would write this
 * text under another spelling, so a failure that is not the route's absence leaves that question
 * open, and an upload sent anyway would store the changed text this check exists to stop. A text
 * Dispatch refused, its answer marked as a refusal of the text (`TEXT_REFUSAL_HEADER`), is no such
 * failure: the check answers it with Dispatch's own refusal (`askRoundTrip`).
 *
 * The round trip writes nothing, so where Dispatch kept no copy of its answer under either of the
 * two keys the client asks it under (`DispatchAnswerNotKeptError`), nothing was made either: the
 * refusal says so (`askRoundTrip`), and that sending the upload again asks it afresh.
 */
export function storedTextCheckFailed(failed: Extract<StoredTextCheck, { kind: "failed" }>): Error {
  const again =
    failed.error instanceof DispatchAnswerNotKeptError
      ? "Send the upload again, which asks it under a new key."
      : "Send the upload again once that call is answered.";
  return new Error(
    `Dispatch could not say how it would store this text, so nothing was uploaded: ${failed.failure}. ${again}`,
    { cause: failed.error }
  );
}

/**
 * Whether the round trip failed because Dispatch itself holds no such route: the 404 or 405 a
 * Dispatch older than the route answers, as its own JSON. Such a server cannot say what it would
 * store, and refused the upload itself before this check existed, so the upload goes on and that
 * server makes its own answer.
 *
 * A 404 or 405 page from an ingress, a proxy or a load balancer is `DispatchGatewayError`,
 * which says nothing about the route, so `dispatchAnswered` is what this asks: reading such a
 * page as an absent route would skip the check and store the respelled text it exists to stop.
 */
function roundTripRouteAbsent(error: unknown): error is DispatchServiceError {
  return dispatchAnswered(error, 404) || dispatchAnswered(error, 405);
}

/**
 * What Dispatch says of the text an upload would store as a Markdown document (`storedTextCheck`
 * for inline text, `storedFileCheck` for a `--path` file), and so what the call does:
 * - `kept`: the store takes the text: inline, as given but for one final line feed, or blank,
 *   which is held to no spelling; from a file, however the store spells it. The call uploads it.
 * - `reordered`: inline text that differs from what the store writes only in the order of typed
 *   blocks' attributes (`storedUpload`). The call uploads `sent`, the text as the store writes it,
 *   and its answer carries `lines`, one for each such block.
 * - `refused`: the call refuses the text, and `refusal` is what it throws for it: a
 *   `ToolInputError` for inline text the store writes under another spelling
 *   (`storedUpload`'s refusal), an `Error` in Dispatch's words for text past the bytes one document
 *   may hold (`documentTooLarge`), and Dispatch's own refusal of the round trip, which is its
 *   upload's refusal of the same text (`askRoundTrip`).
 * - `unchecked`: the round trip gave no answer about the text, and the call uploads it unchecked,
 *   leaving every refusal to the upload: `failure` says why, and `routeAbsent` whether this
 *   Dispatch holds no such route (`roundTripRouteAbsent`). Blank text and a file are asked for a
 *   refusal alone, which their upload makes too, so every failure of theirs is `unchecked`; inline
 *   text that is not blank is only where the route is absent.
 * - `failed`: inline text that is not blank, whose round trip failed otherwise: the call refuses
 *   the upload (`storedTextCheckFailed`), since a check that did not run says nothing about what
 *   the store would write.
 */
export type StoredTextCheck =
  | { readonly kind: "kept" }
  | { readonly kind: "reordered"; readonly sent: string; readonly lines: readonly string[] }
  | { readonly kind: "refused"; readonly refusal: Error }
  | { readonly kind: "unchecked"; readonly failure: string; readonly routeAbsent: boolean }
  | { readonly kind: "failed"; readonly failure: string; readonly error: unknown };

/**
 * The refusal of a Markdown document's text past MARKDOWN_DOCUMENT_MAX_BYTES, or undefined for
 * text within it. `measured` is the text, inline text counted in UTF-8 as the server counts the
 * text it is sent (`utf8Bytes`), never in the UTF-16 units of its `length`; a `--path` file's
 * bytes as they are sent; or a file's size in bytes before it is read. The refusal is the server's
 * own `413 CAP_EXCEEDED` in its own words (MARKDOWN_DOCUMENT_TOO_LARGE), which `messageFor` prints
 * as it prints that answer, for an upload within the server's 25 MiB file cap; past that cap the
 * server answers `artifact blob exceeds 25 MB` instead (errUploadTooLarge). The server refuses
 * such an upload whatever name it carries, so `dispatch artifact` refuses such text first: the
 * call before it reads whether the owner holds the name (`artifactCommand`), the call and its dry
 * run before they ask the round trip (`storedTextCheck`, and `documentFileRead` for a file), and
 * a document's bytes before the first U+0000 or byte that is not UTF-8 in them
 * (`documentBytesRefusal`).
 */
function documentTooLarge(measured: string | Uint8Array | number): Error | undefined {
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
 * The refusal of a Markdown document's bytes, in the order Dispatch's upload refuses a file's
 * (`documentTextRefusal`, packages/envoy/internal/dispatch/api/artifacts.go), or undefined for
 * bytes that order passes: first the size (`documentTooLarge`), and then the first U+0000 or
 * byte that is not UTF-8, whichever comes first (`unstorableBytes`), in Dispatch's own code,
 * words and count, under `field`. `field` is the field the upload names: `file` for a `--path`
 * file (`documentFileRead`), `content` for inline text, given in the argument itself or read by a
 * flag from a file or stdin (`documentFieldRefusal`, and `runDispatchCli` for bytes that are not
 * UTF-8). Each calls this, so the same bytes get the same refusal but for the field, whichever way
 * they are given.
 */
export function documentBytesRefusal(field: string, bytes: Uint8Array): Error | undefined {
  return documentTooLarge(bytes) ?? unstorableBytes(field, bytes);
}

/**
 * The refusal of a Markdown document's `text` for `field`, given in the argument itself or read
 * from a file or stdin and decoded by the strict read (`readFileText`), or undefined for text
 * Dispatch stores: the refusal of the text's UTF-8 (`fileTextBytes`, which for a text read is the
 * bytes it was read from) by `documentBytesRefusal`. The strict read decodes UTF-8 alone, and a
 * text given in the argument is a string, which encodes to UTF-8, so only the text's size or a
 * U+0000 can refuse it. A text given in argv holds no U+0000 on any kernel. Linux holds one
 * argument to MAX_ARG_STRLEN, 32 pages, which is 128 KiB on 4 KiB pages and 2 MiB on 64 KiB pages
 * (include/uapi/linux/binfmts.h), so with 4 KiB pages argv carries no text past the bytes one
 * document holds, and with 64 KiB pages it can. An in-process caller of `runDispatchCli` can give
 * either on any kernel. The check runs on the text however it is given, so what it refuses does
 * not depend on the page size. The text is encoded and scanned again only where its UTF-8 is past
 * the bytes one document holds or it holds a U+0000.
 */
export function documentFieldRefusal(field: string, text: string): Error | undefined {
  return Buffer.byteLength(text, "utf8") > MARKDOWN_DOCUMENT_MAX_BYTES || text.includes("\u0000")
    ? documentBytesRefusal(field, fileTextBytes(text))
    : undefined;
}

/**
 * The file `dispatch artifact --path` sends for `path`, resolved against `cwd`, the directory the
 * call runs in, under `name`, the name it is sent as, and under the type its upload declares,
 * which Dispatch stores it by: an upload whose file part is declared `text/markdown`, its
 * parameters and its case aside, is a document, and any other is a picture or a file
 * (`artifactKind`, packages/envoy/internal/dispatch/api/artifacts.go). The type is
 * `text/markdown` exactly when AskGate's predicate holds the upload as a document
 * (uploadIsDocument, ../askgate-marker.ts), from its name and the path, and never otherwise, so
 * Dispatch stores as a document every upload AskGate holds as one and no other (INFRA-4071 R15):
 * a plan written to plan.txt and sent as plan.md is a document, as the plan the issue reads must
 * be, and so is a Markdown file sent under a title (`Design notes`, `Plan v1.2`, `notes.md/`) or
 * from `notes.md/`, which the path resolves to notes.md. Every other upload declares the type Bun
 * gives the extension that decides in its name (`x.<extension>`, uploadExtensionOf, past trailing
 * extensions of digits alone), whatever the path, so cases.txt sent from cases.md is a file of the
 * bytes as written, notes.txt.1 goes up as `text/plain` and rows.jsonl.1 as
 * `application/octet-stream`; a name with no extension, or no name, declares the type Bun reads
 * from the resolved path. A type of those that is still `text/markdown` goes up as
 * `application/octet-stream`. The call uploads this file, or the bytes read of it under its type,
 * and the CLI's check of the file, the call and its dry run read from it whether the file is
 * Markdown (`documentFileRead`), so the upload and its check take one rule of which files are
 * Markdown.
 */
export function uploadedFile(cwd: string, path: string, name: string | undefined): BunFile {
  const local = resolvePath(cwd, path);
  if (uploadIsDocument({ name, path }, cwd)) return Bun.file(local, { type: "text/markdown" });
  const extension = name === undefined ? undefined : uploadExtensionOf(name);
  return Bun.file(local, {
    type: notMarkdown(Bun.file(extension === undefined ? local : `x.${extension}`).type),
  });
}

/** `type`, or `application/octet-stream` for `text/markdown`, which only an upload AskGate holds
 *  as a document declares (uploadedFile). */
function notMarkdown(type: string): string {
  return type.split(";", 1)[0]?.trim().toLowerCase() === "text/markdown"
    ? "application/octet-stream"
    : type;
}

/**
 * What the check of an upload's `--path` file found (`documentFileRead`):
 * - `passed`: the check passes the file. `bytes` are its bytes where Dispatch stores it as a
 *   Markdown document, which `runDispatchCli` hands the dry run's step and the call
 *   (`uploadBytes`), and undefined for a file that is not read: one Dispatch stores as its bytes,
 *   and one that is neither a regular file nor a directory (a FIFO, a device, a socket), whose read
 *   can wait forever. The call uploads such a file unchecked, and Dispatch's upload judges it.
 * - `refused`: Dispatch's own refusal of the file's text, which the call throws.
 * - `unread`: the file could not be read, and `error` is the read's own error. That is no refusal
 *   of the text: the call throws it when it reads the file.
 */
export type DocumentFileRead =
  | { readonly kind: "passed"; readonly bytes: Uint8Array<ArrayBuffer> | undefined }
  | { readonly kind: "refused"; readonly refusal: Error }
  | { readonly kind: "unread"; readonly error: unknown };

/**
 * The check of an upload's `--path` file `file` (`uploadedFile`), which reads a file Dispatch
 * stores as a Markdown document once and refuses its text in Dispatch's own words, in its upload's
 * order, before anything is asked. Dispatch stores an upload as a document exactly when the type
 * it declares is `text/markdown`, its parameters and its case aside; any other file is not read.
 * The bytes checked are `held` where the caller holds them, or else the file's own. A file whose
 * stat says it is neither a regular file nor a directory (a FIFO, a character or block device, a
 * socket, or a link to one) is not read either: its size says nothing of what a read returns, and
 * a read of a FIFO no process writes to waits forever. It passes with no bytes, as a file Dispatch
 * stores as its bytes does, and the call uploads it unchecked, for Dispatch's upload to judge. A
 * directory is read, and the read fails (`unread`). A file whose size is past the bytes one
 * document holds (`documentTooLarge`) is refused before it is read, and the bytes in hand by
 * `documentBytesRefusal`, which refuses bytes past that size, from a file whose size could not be
 * read or that grew since, and then the first U+0000 or byte that is not UTF-8 in them.
 * `runDispatchCli` checks the file with this once the flags are read, and the dry run's step
 * (`documentFileBytes`) checks the bytes it read. The call checks those bytes too, save where
 * AskGate, which reviews the call, read the file a second time for its digest: the call then
 * checks and sends the bytes that read took in their place.
 */
export async function documentFileRead(
  file: BunFile,
  held?: Uint8Array<ArrayBuffer>
): Promise<DocumentFileRead> {
  if (file.type.split(";", 1)[0]?.trim().toLowerCase() !== "text/markdown") {
    return { kind: "passed", bytes: undefined };
  }
  let bytes = held;
  if (bytes === undefined) {
    const stats = await file.stat().catch(() => undefined);
    if (stats !== undefined && !stats.isFile() && !stats.isDirectory()) {
      return { kind: "passed", bytes: undefined };
    }
    const tooLarge = stats === undefined ? undefined : documentTooLarge(stats.size);
    if (tooLarge !== undefined) return { kind: "refused", refusal: tooLarge };
    try {
      bytes = await file.bytes();
    } catch (error) {
      return { kind: "unread", error };
    }
  }
  const refusal = documentBytesRefusal("file", bytes);
  return refusal === undefined ? { kind: "passed", bytes } : { kind: "refused", refusal };
}

/**
 * The bytes of an upload's `--path` file `file` that Dispatch stores as a Markdown document, as
 * `documentFileRead` checks them, or undefined for a file it does not read: one Dispatch stores as
 * its bytes, or a FIFO, a device or a socket. `held` are the bytes the CLI read of the file where
 * it read them (`ExecuteDispatchToolInput.uploadBytes`, `DryRunContext.uploadBytes`), which are
 * checked in place of a read. The refusal of the text, and the error of a read that failed, are
 * thrown, so the call and its dry run answer them before anything is asked.
 */
export async function documentFileBytes(
  file: BunFile,
  held?: Uint8Array<ArrayBuffer>
): Promise<Uint8Array<ArrayBuffer> | undefined> {
  const read = await documentFileRead(file, held);
  if (read.kind === "refused") throw read.refusal;
  if (read.kind === "unread") throw read.error;
  return read.bytes;
}

/** What a round trip answers for a text: the markdown Dispatch would store for it, Dispatch's
 *  refusal of it, or no answer about it, with what the round trip failed with. */
type RoundTripAnswer =
  | { readonly kind: "stored"; readonly stored: string }
  | { readonly kind: "refused"; readonly refusal: Error }
  | {
      readonly kind: "unanswered";
      readonly failure: string;
      readonly error: unknown;
      readonly routeAbsent: boolean;
    };

/** The check of a text whose round trip gave no answer about it, which the call uploads
 *  unchecked: what the round trip failed with, and whether the route is absent. */
function unchecked(answer: Extract<RoundTripAnswer, { kind: "unanswered" }>): StoredTextCheck {
  return { kind: "unchecked", failure: answer.failure, routeAbsent: answer.routeAbsent };
}

/**
 * Asks `route`, a round trip, which writes nothing, through `ask`: the text inline
 * (`markdownRoundTrip`) or a file's bytes (`fileRoundTrip`). `client` is called once, for the
 * client the round trip goes through, and what it throws is thrown; every failure of the round
 * trip is returned.
 *
 * An answer Dispatch marks as its refusal of the text (`TEXT_REFUSAL_HEADER`, read into
 * `DispatchServiceError.textRefusal`), whatever its status, is the call's refusal, as Dispatch
 * worded it, printed as the call prints the upload's refusal (`CAP_EXCEEDED: <message>`,
 * `INVALID_MARKDOWN: <message>`). Dispatch marks every refusal of the checks an upload makes of
 * the text alone, in that upload's own order, status, code and words, and makes those only a new
 * document's seed makes only for a request that asks for them (`new_document`), which the check
 * asks where the call makes a new document, without `--force`: a replacement is judged against
 * the document it replaces, which relaxes those rules. So every marked refusal the check gets
 * holds for the call, the call and its dry run refuse such a text as the upload would, and a
 * check Dispatch adds to that upload, at any status, reaches them with no change here.
 *
 * Every unmarked failure says nothing of the text, and is `unanswered`: a 404 or 405 is a
 * Dispatch without the route (`roundTripRouteAbsent`), and anything else is the route's own
 * refusal of its body, a 401, a 403, a 429, a 5xx of Dispatch's own, a gateway's page or no
 * answer at all.
 */
async function askRoundTrip(
  client: () => DispatchClient,
  route: string,
  ask: (dispatch: DispatchClient) => Promise<string>
): Promise<RoundTripAnswer> {
  const dispatch = client();
  try {
    return { kind: "stored", stored: await ask(dispatch) };
  } catch (error) {
    const routeAbsent = roundTripRouteAbsent(error);
    if (!routeAbsent && error instanceof DispatchServiceError && error.textRefusal) {
      return { kind: "refused", refusal: error };
    }
    return {
      kind: "unanswered",
      failure:
        error instanceof DispatchAnswerNotKeptError
          ? `${route} wrote nothing, and Dispatch kept no copy of its answer (${error.code})`
          : // A failure whose own message ends its sentence (a transport failure, a gateway's
            // advice) gets no second stop where the refusal goes on after it.
            `${route} failed with ${messageFor(error).replace(/\.$/, "")}`,
      error,
      routeAbsent,
    };
  }
}

/**
 * The stored-text check of inline `content` uploaded under `name` by `tool`, `force` its
 * `--force`. `dispatch artifact` and its dry run both make it, so the two never answer
 * differently, and it is the one place that decides what the call does with blank text. Inline
 * text is stored as Markdown: Dispatch parses it and stores its rendering, so a text it would write
 * under another spelling is refused rather than stored changed, but for one whose typed blocks'
 * attributes differ only in their order, which goes up as Dispatch writes it (`storedUpload`), and
 * a text Dispatch refuses is refused with its refusal (`askRoundTrip`). Without `force` the call makes a new document, so the
 * round trip is asked for a new document's refusals; with it, it is not, and Dispatch answers as
 * the most permissive replacement takes the text: with the rendering that replacement stores,
 * which is compared as any other, or with a refusal every replacement makes, which is the call's.
 *
 * Text past MARKDOWN_DOCUMENT_MAX_BYTES is `refused` (`documentTooLarge`) without calling
 * `client`, since the server refuses an upload of such text whatever it holds, so nothing is asked
 * for it and a dry run of it builds no client. The empty string is `kept` without calling `client`
 * too: it is a create of an empty document, which no check of the text refuses.
 *
 * Any other blank text (`goBlankPattern`) is asked, and only a refusal counts: the upload refuses
 * blank text as it refuses any, too many elements among it, so `refused` is returned as it came,
 * every answer is `kept`, never refused for its spelling, and a round trip that gave no answer
 * leaves the upload to make its own refusal (`unchecked`). The round trip answers "" for every
 * text that pattern calls blank and that it does not refuse, since markdownRoundTrip answers ""
 * for whatever `strings.TrimSpace` empties (that package's markdown_round_trip.go), and that short
 * answer is not what the store writes for each of them: the parse and render a seed runs keep
 * every character of this set but a tab, a line feed, a carriage return and a space, each of the
 * rest rendering to itself.
 */
async function storedTextCheck(
  client: () => DispatchClient,
  tool: string,
  name: string,
  content: string,
  force: boolean
): Promise<StoredTextCheck> {
  const tooLarge = documentTooLarge(content);
  if (tooLarge !== undefined) return { kind: "refused", refusal: tooLarge };
  if (content === "") return { kind: "kept" };
  const answer = await askRoundTrip(client, MARKDOWN_ROUND_TRIP, (dispatch) =>
    dispatch.markdownRoundTrip(content, !force)
  );
  const blank = goBlankPattern.test(content);
  switch (answer.kind) {
    case "refused":
      return answer;
    case "unanswered":
      return blank || answer.routeAbsent
        ? unchecked(answer)
        : { kind: "failed", failure: answer.failure, error: answer.error };
    case "stored": {
      if (blank) return { kind: "kept" };
      // What goes up, and what is refused, is storedUpload's rule (stored-spelling.ts).
      const upload = storedUpload(content, answer.stored, name);
      if ("refusal" in upload) {
        return { kind: "refused", refusal: new ToolInputError(tool, [upload.refusal]) };
      }
      return upload.reordered.length === 0
        ? { kind: "kept" }
        : { kind: "reordered", sent: upload.sent, lines: upload.reordered };
    }
  }
}

/**
 * The stored-text check of a `--path` file Dispatch stores as a Markdown document, `bytes` its
 * content as `documentFileBytes` answers it, already held to the bytes one document holds and
 * free of U+0000 and of bytes that are not UTF-8, and `force` the upload's `--force`. `dispatch
 * artifact` and its dry run both make it, with the bytes the call sends, so the two never answer
 * differently. The file round trip is asked about the file's bytes as they are (`fileRoundTrip`),
 * for a new document's refusals too where the call makes a new document, without `force`, and only
 * its refusal is taken (`askRoundTrip`): Dispatch refuses there, in the upload's own words, the
 * bytes the upload of that file refuses for the text alone - the parse's refusal, an ask that
 * breaks its content rule, a rendering too large, and the size, a NUL and a byte that is not UTF-8,
 * which `documentFileBytes` refused already in the same words - and the call throws that refusal
 * before it uploads.
 * Whatever spelling it answers is `kept`: the call stores a Markdown file as the store spells it,
 * with no refusal for its spelling. The upload refuses the file for everything the round trip
 * would refuse its bytes for, so a round trip that gave no answer - none in time, a failure, or a
 * Dispatch without the route, which answers 404 or 405 - is `unchecked`, and the call uploads the
 * file, leaving its refusal to the upload. The call uploads the file whatever the round trip failed
 * with, so asking it again would only delay that upload: `client` asks it once in the call, as in
 * the dry run (`artifactTextCheck`).
 */
async function storedFileCheck(
  client: () => DispatchClient,
  bytes: Uint8Array,
  force: boolean
): Promise<StoredTextCheck> {
  const answer = await askRoundTrip(client, FILE_ROUND_TRIP, (dispatch) =>
    dispatch.fileRoundTrip(bytes, !force)
  );
  switch (answer.kind) {
    case "stored":
      return { kind: "kept" };
    case "refused":
      return answer;
    case "unanswered":
      return unchecked(answer);
  }
}

/**
 * The check of the text `dispatch artifact` uploads as a Markdown document, chosen here for the
 * call and its dry run alike (`artifactCommand`, `artifactDryRun`), so the two refuse a text alike:
 * inline `content` takes storedTextCheck, asked through `textClient`; a `--path` file's `bytes`, as
 * `documentFileBytes` answers them, take storedFileCheck, asked through `fileClient`; and an upload
 * holding neither - a file `documentFileRead` passes with no bytes (one Dispatch stores as its
 * bytes, or a FIFO, a device or a socket), or a name alone - is asked nothing (undefined). The
 * call hands its own client for inline text, since a failed check of such text refuses the upload,
 * so the check asks again as every request of the call does; for a file it hands a client that
 * asks once within DRY_RUN_DEADLINE_MS (`askOnceWithinDryRun`), since the file goes up whatever
 * its check failed with. The dry run hands its own client for both. The check is returned unasked,
 * so its caller decides what becomes of an error building a client throws.
 */
export function artifactTextCheck(upload: {
  readonly tool: string;
  readonly name: string;
  readonly content: string | undefined;
  readonly bytes: Uint8Array | undefined;
  readonly force: boolean;
  readonly textClient: () => DispatchClient;
  readonly fileClient: () => DispatchClient;
}): (() => Promise<StoredTextCheck>) | undefined {
  const { tool, name, content, bytes, force } = upload;
  if (content !== undefined) {
    return () => storedTextCheck(upload.textClient, tool, name, content, force);
  }
  if (bytes !== undefined) return () => storedFileCheck(upload.fileClient, bytes, force);
  return undefined;
}
