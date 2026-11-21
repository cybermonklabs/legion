import type { Artifact } from "@legion/contracts";
import { ARTIFACT_NAME_EXISTS_RULE } from "@legion/contracts";
import { actorText } from "../actor-text";

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
