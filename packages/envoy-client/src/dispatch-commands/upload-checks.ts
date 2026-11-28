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

/** The slugs of the documents an issue holds beside its spec: its plan, its Verification document
 *  and its decisions document (`model.RoleDocumentSlugs`, packages/envoy/internal/dispatch/model). */
const ISSUE_ROLE_DOCUMENT_SLUGS: ReadonlySet<string> = new Set([
  "plan-md",
  "verification-md",
  "decisions-md",
]);

/** Whether Dispatch takes an upload of kind `upload` under `held`'s name as `held`'s next version
 *  although the two kinds differ: a Markdown upload over an issue's artifact at a role document's
 *  slug that an earlier upload stored as a file or a picture, which that upload makes the role's
 *  document (`storeArtifact` and `documentRole`, packages/envoy/internal/dispatch/api/artifacts.go).
 *  Every other change of kind Dispatch refuses under that name. */
function convertsRoleArtifactToDocument(held: Artifact, upload: Artifact["kind"]): boolean {
  return (
    upload === "doc" &&
    held.kind !== "doc" &&
    held.issue_key !== null &&
    held.issue_key !== "" &&
    ISSUE_ROLE_DOCUMENT_SLUGS.has(held.slug)
  );
}

/** What `dispatch artifact` says of an upload convertsRoleArtifactToDocument answers true for. It
 *  writes the file's or the picture's next version, so it is refused as fileNameExistsRule refuses
 *  one, but the version it writes is a document. The upload that makes the document names no
 *  token, since none stands under the name yet: Dispatch refuses one 409 PRECONDITION_FAILED
 *  (uploadNoTokenRefusal), whatever the file's version states, so the rule names --force alone.
 *  dispatch doc-edit changes no file, so the rule does not name it. */
function roleDocumentConversionRule(held: Artifact): string {
  const what = held.kind === "image" ? "picture" : "file";
  return `An upload under the name of a ${what} the owner already holds is refused. This one is Markdown, and the ${what}'s slug, ${held.slug}, is that of one of the documents an issue holds beside its spec (its plan, Verification document or decisions document), so Dispatch takes the upload as the ${what}'s next version and makes the ${what} that document, over the bytes it holds. To write that version, send the upload again with force (--force) alone: no document stands under the name yet, so Dispatch refuses a token on this upload 409 PRECONDITION_FAILED.`;
}

/** The refusal of an upload under a name its owner already holds: the artifact found, its kind,
 *  its newest version and who wrote that version, then what to do. An upload of another kind than
 *  the name holds is told to take another name (artifactKindConflictRule), save a Markdown upload
 *  over an issue's role document held as a file or a picture, which Dispatch takes as that
 *  artifact's next version and makes a document (roleDocumentConversionRule, --force alone). One of
 *  the same kind is told what replaces it, a document by ARTIFACT_NAME_EXISTS_RULE and a file or a
 *  picture by fileNameExistsRule, which advises --force alone where the newest version states no
 *  token. No refusal prints a token: the writer names the one its own read printed. */
export function artifactNameExistsProblem(
  held: Artifact,
  owner: string,
  upload: Artifact["kind"]
): string {
  const versions = held.versions ?? [];
  const newest = versions[versions.length - 1];
  const rule = convertsRoleArtifactToDocument(held, upload)
    ? roleDocumentConversionRule(held)
    : upload !== held.kind
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
