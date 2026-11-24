import type { Artifact } from "@legion/contracts";
import { ARTIFACT_NAME_EXISTS_RULE, PATH_KEEPS_BYTES, pathKeepsBytesOf } from "@legion/contracts";
import { actorText } from "../actor-text";
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

/** The refusal of an upload under a name its owner already holds: the artifact found, its kind,
 *  its newest version and who wrote that version, then what to do. A document's is the rule that
 *  names --force and the token dispatch doc-read prints; a file's or a picture's names the token
 *  itself, or --force alone (fileNameExistsRule). */
export function artifactNameExistsProblem(held: Artifact, owner: string): string {
  const versions = held.versions ?? [];
  const newest = versions[versions.length - 1];
  const rule = held.kind === "doc" ? ARTIFACT_NAME_EXISTS_RULE : fileNameExistsRule(held, newest);
  if (newest === undefined) {
    return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}), which has no version yet and which ${actorText(held.created_by)} created. ${rule}`;
  }
  const authors = (newest.authors ?? []).map(actorText);
  const wroteIt = authors.length === 0 ? actorText(held.created_by) : authors.join(" and ");
  return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}) at version ${newest.number}, which ${wroteIt} wrote. ${rule}`;
}

/**
 * What the refusal of an upload under a file's or a picture's name says to do. A file holds no
 * text dispatch doc-edit changes, so its one way to a next version is the upload: with --force
 * and the token of the version it replaces, which the owner's listing states beside the version
 * (`versions[].token`) and which the refusal therefore names whole, so the writer sends exactly
 * what it reads. Where Dispatch states no token for the version, it refuses one on the file's
 * upload (a build from before file tokens, 400 INVALID_PRECONDITION; a version stored without its
 * bytes' hash, 409 PRECONDITION_FAILED), so the refusal names --force alone, which it takes.
 */
function fileNameExistsRule(
  held: Artifact,
  version: NonNullable<Artifact["versions"]>[number] | undefined
): string {
  const what = held.kind === "image" ? "picture" : "file";
  const head = `An upload under the name of a ${what} the owner already holds is refused: it writes that ${what}'s next version, over the bytes it holds.`;
  if (version?.token === undefined) {
    return `${head} This Dispatch states no token for the version it would replace and refuses one on the upload of a ${what}: to write the next version, send the upload again with force (--force) alone.`;
  }
  return `${head} To write the next version, send the upload again with --force --precondition-json '${JSON.stringify({ document: version.token })}', the token of version ${version.number}, which dispatch doc-read prints for it.`;
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
