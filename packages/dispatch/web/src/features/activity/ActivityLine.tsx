import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useShownQuery } from "../../api/cache/shown-query";
import { issueQuery } from "../../api/queries";
import type { Artifact, DispatchEvent } from "../../api/types";
import { linkText } from "../../theme/classes";
import { type AgentPart, AgentParts } from "../refs/AgentName";
import { buildIssuePath, buildReferencePath, documentRoute } from "../refs/routes";

type ArtifactVersionEvent = Extract<DispatchEvent, { type: "artifact.version" }>;

/** The activity text with the artifact's name turned into a link, so the reader opens the
 *  upload from the Conversation instead of hunting for it on the Artifacts tab. The text itself
 *  comes from `activityDescription`, in a text part; an agent part is never searched for it. The
 *  link goes on the last reading of the name, because a correction's line names the document
 *  twice - `renamed <old> to <new>` - and it is the new title that has the page. */
function LinkedName({
  description,
  name,
  to,
}: {
  description: readonly AgentPart[];
  name: string;
  to: string;
}): ReactNode {
  const linked = description.findIndex((part) => typeof part === "string" && part.includes(name));
  return (
    <span>
      <AgentParts
        parts={description}
        text={(part, index) => {
          if (index !== linked) {
            return part;
          }
          const at = part.lastIndexOf(name);
          return (
            <>
              {part.slice(0, at)}
              <Link className={`dispatch-inline-link underline ${linkText}`} to={to}>
                {name}
              </Link>
              {part.slice(at + name.length)}
            </>
          );
        }}
      />
    </span>
  );
}

/** `artifact.version` carries the artifact's id and name but not its slug; the issue query
 *  (already loaded by the page) supplies the artifact the version belongs to. */
function SavedVersionLine({
  description,
  event,
  issueKey,
}: {
  description: readonly AgentPart[];
  event: ArtifactVersionEvent;
  issueKey: string;
}): ReactNode {
  const issue = useShownQuery(issueQuery(issueKey));
  const artifact: Artifact | undefined = issue.data?.artifacts?.find(
    ({ id }) => id === event.payload.artifact_id
  );
  if (artifact === undefined) {
    return (
      <span>
        <AgentParts parts={description} />
      </span>
    );
  }
  return (
    <LinkedName
      description={description}
      name={event.payload.name}
      to={buildReferencePath(documentRoute(artifact, event.payload.version.number))}
    />
  );
}

export function ActivityLine({
  description,
  event,
  issueKey,
}: {
  description: readonly AgentPart[];
  event: DispatchEvent;
  issueKey: string;
}): ReactNode {
  switch (event.type) {
    // The slug does not move with a correction, so the new title links to the page the old one
    // did, which is the page a creation links to. A removed document has no page at all, so
    // `artifact.removed` takes the plain line the default below renders: a link to a gone
    // document would answer "No artifact … on …".
    case "artifact.created":
    case "artifact.renamed":
      return (
        <LinkedName
          description={description}
          name={event.payload.artifact.name}
          to={buildReferencePath(documentRoute(event.payload.artifact))}
        />
      );
    case "artifact.version":
      return <SavedVersionLine description={description} event={event} issueKey={issueKey} />;
    case "comment.created":
      return (
        <>
          <span>
            <AgentParts parts={description} />
          </span>
          <Link
            className={`dispatch-inline-link underline ${linkText}`}
            to={buildIssuePath({ id: event.payload.id, key: issueKey, kind: "comment" })}
          >
            view
          </Link>
        </>
      );
    default:
      return (
        <span>
          <AgentParts parts={description} />
        </span>
      );
  }
}
