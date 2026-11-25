import type { Artifact } from "@legion/contracts";
import {
  ARTIFACT_NAME_EXISTS_RULE,
  artifactKindConflictRule,
  fileNameExistsRule,
  PATH_KEEPS_BYTES,
  pathKeepsBytesOf,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import { DispatchAnswerNotKeptError } from "../dispatch-http";
import { sniffPictureType } from "../dispatch-pictures";
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

/**
 * The kind Dispatch stores an upload as, decided as the server decides it (artifactKind in
 * artifacts.go): a declared Markdown upload is a document, which inline content always is and a
 * path is when its name says Markdown (the media type the upload declares for it,
 * `pathKeepsBytesOf`); bytes that begin as a PNG, JPEG, GIF or WebP are a picture, by the
 * signatures Go's http.DetectContentType matches (`sniffPictureType`); anything else is a file.
 * `head` reads the first PICTURE_SNIFF_BYTES bytes of the file at the path it is given, and is
 * asked only for a path that is not Markdown.
 */
export async function uploadKindOf(
  path: string | undefined,
  head: (path: string) => Promise<Uint8Array>
): Promise<Artifact["kind"]> {
  if (path === undefined || !pathKeepsBytesOf(path)) return "doc";
  return sniffPictureType(await head(path)) === undefined ? "file" : "image";
}

/** The refusal of an upload under a name its owner already holds: the artifact found, its kind,
 *  its newest version and who wrote that version, then what to do. An upload of another kind than
 *  the name holds is told to take another name (artifactKindConflictRule); one of the same kind is
 *  told what replaces it, a document by ARTIFACT_NAME_EXISTS_RULE and a file or a picture by
 *  fileNameExistsRule, which advises --force alone where the newest version states no token. No
 *  refusal prints a token: the writer names the one its own read printed. */
export function artifactNameExistsProblem(
  held: Artifact,
  owner: string,
  upload: Artifact["kind"]
): string {
  const versions = held.versions ?? [];
  const newest = versions[versions.length - 1];
  const rule =
    upload !== held.kind
      ? artifactKindConflictRule(held.kind, upload)
      : held.kind === "doc"
        ? ARTIFACT_NAME_EXISTS_RULE
        : fileNameExistsRule(held.kind, newest?.token !== undefined);
  if (newest === undefined) {
    return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}), which has no version yet and which ${actorText(held.created_by)} created. ${rule}`;
  }
  const authors = (newest.authors ?? []).map(actorText);
  const wroteIt = authors.length === 0 ? actorText(held.created_by) : authors.join(" and ");
  return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}) at version ${newest.number}, which ${wroteIt} wrote. ${rule}`;
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
 *
 * The round trip writes nothing, so where Dispatch kept no copy of its answer under either of the
 * two keys the client asks it under (`DispatchAnswerNotKeptError`), nothing was made either: the
 * refusal says so, and that sending the upload again asks it afresh.
 */
export function storedTextCheckFailed(error: unknown, name: string): string {
  if (error instanceof DispatchAnswerNotKeptError) {
    return `Dispatch could not say how it would store this text, so nothing was uploaded: POST /api/v1/markdown/round-trip wrote nothing, and Dispatch kept no copy of its answer (${error.code}). Send the upload again, which asks it under a new key.`;
  }
  const failure = `Dispatch could not say how it would store this text, so nothing was uploaded: POST /api/v1/markdown/round-trip failed with ${messageFor(error)}.`;
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
