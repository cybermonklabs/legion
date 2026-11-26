import type { Artifact } from "@legion/contracts";
import {
  ARTIFACT_NAME_EXISTS_RULE,
  artifactKindConflictRule,
  fileNameExistsRule,
  uploadStoredName,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import { uploadIsDocument } from "../askgate-marker";
import { sniffPictureType } from "../dispatch-pictures";

/** The document an upload under `name` would write the next version of: the owner's artifact
 *  whose slug or filename that name is, as the upload itself resolves one (`$2 in (name, slug)`,
 *  the slug first), under the name Dispatch stores (`uploadStoredName`, trimmed with Go's
 *  `strings.TrimSpace`, which is not `trim`; `packages/envoy/internal/dispatch/api/artifacts.go`).
 *  Undefined where the owner holds no such document. */
export function artifactUnderName(
  artifacts: readonly Artifact[],
  name: string
): Artifact | undefined {
  const written = uploadStoredName(name);
  return (
    artifacts.find((held) => held.slug === written) ??
    artifacts.find((held) => held.name === written)
  );
}

/**
 * The upload's stored kind: check Markdown with the same name and path as the upload, then
 * inspect only the first PICTURE_SNIFF_BYTES bytes of a non-Markdown file for a picture signature.
 */
export async function uploadKindOf(
  name: string,
  path: string | undefined,
  cwd: string,
  head: (path: string) => Promise<Uint8Array>
): Promise<Artifact["kind"]> {
  if (path === undefined || uploadIsDocument({ name, path }, cwd)) return "doc";
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
