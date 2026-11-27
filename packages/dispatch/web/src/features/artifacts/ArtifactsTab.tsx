import { graphNodeKey, issueReferenceSections } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { memo, type ReactNode, useCallback, useId, useMemo, useRef } from "react";
import { Link } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import type { Artifact, IssueReferencesAnswer, ReferenceLink } from "../../api/types";
import type { BarActionProps, BarActionRefusal } from "../../components/BarAction";
import { textGlyph } from "../../components/bar-look";
import { ControlRow } from "../../components/ControlRow";
import { CONTROL, controlIn, namedControlIn } from "../../components/control-names";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { useCappedAddressParam, usePageAddress } from "../../lib/page-address";
import { compareTimestamps } from "../../lib/timestamps";
import {
  badgePrimary,
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { DeletedBody, EditedMark } from "../refs/PostMarks";
import { PlainText } from "../refs/plain-text";
import { type PostDeletion, postDeletion } from "../refs/post-marks";
import {
  buildIssuePath,
  buildProjectPath,
  buildReferencePath,
  documentRoute,
  isAgentArtifact,
  parseIssuePath,
  parseNodeReference,
  referenceHref,
} from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { artifactVersionUrl, formatArtifactBytes, versionsNewestFirst } from "./ArtifactHeader";
import { ArtifactDropZone, ArtifactUploadStaging, useArtifactUpload } from "./ArtifactUpload";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL, useNewDocument } from "./CreateDocument";
import {
  ARTIFACTS_NOT_LOADED,
  CLOSED_ISSUE_REMOVE_REFUSAL,
  RemoveDocument,
  removalIsOffered,
} from "./DocumentWayBack";

function kindIcon(kind: Artifact["kind"]): ReactNode {
  const common = {
    "aria-hidden": true,
    className: "size-5 shrink-0",
    fill: "none",
    viewBox: "0 0 24 24",
  };
  if (kind === "doc") {
    return (
      <svg {...common} stroke="currentColor">
        <title>Document artifact</title>
        <path
          d="M7 3h7l3 3v15H7z"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
        <path d="M14 3v4h4M10 12h4M10 16h4" strokeLinecap="round" strokeWidth="1.75" />
      </svg>
    );
  }
  if (kind === "image") {
    return (
      <svg {...common} stroke="currentColor">
        <title>Image artifact</title>
        <rect height="16" rx="2" width="18" x="3" y="4" strokeWidth="1.75" />
        <circle cx="9" cy="10" r="1.25" strokeWidth="1.75" />
        <path
          d="m5 18 5-5 3 3 2-2 4 4"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
      </svg>
    );
  }
  return (
    <svg {...common} stroke="currentColor">
      <title>File artifact</title>
      <path d="M7 3h7l3 3v15H7z" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.75" />
      <path d="M14 3v4h4" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.75" />
    </svg>
  );
}

function artifactPath(artifact: Artifact): string {
  // A conversation's picture has its own page, under its agent's session.
  if (isAgentArtifact(artifact)) {
    return buildReferencePath(documentRoute(artifact));
  }
  return artifact.issue_key === null
    ? buildProjectPath({
        kind: "document",
        project: artifact.project,
        slug: artifact.slug,
      })
    : buildIssuePath({
        key: artifact.issue_key,
        kind: "artifact",
        slug: artifact.slug,
      });
}

/** Who owns an artifact, as a References row names it: its issue, its project, or the agent
 *  whose conversation holds it. */
function ArtifactOwner({
  artifact,
}: {
  artifact: Pick<Artifact, "issue_key" | "project" | "session_id">;
}): ReactNode {
  if (artifact.issue_key !== null) return artifact.issue_key;
  if (!isAgentArtifact(artifact)) return artifact.project;
  return (
    <>
      agent <AgentName inline sessionId={artifact.session_id} storedTitle={undefined} />
    </>
  );
}

/** One of the section's disclosures, collapsed by default: the issue's references are background
 *  context for it, not something worth scrolling past on every visit to the tab. Its summary names
 *  its list and counts the list's rows, so the count shows while it is closed. */
function ReferencesDisclosure({
  children,
  count,
  label,
}: {
  children: ReactNode;
  count: number;
  label: "References" | "Referenced by";
}): ReactNode {
  return (
    <details className={`rounded-lg border p-3 ${borderDefault}`}>
      <summary
        className={`min-h-11 cursor-pointer text-sm font-semibold ${textSecondaryOnSurface}`}
      >
        {label} ({count})
      </summary>
      {children}
    </details>
  );
}

/** The record whose text a references row's name is the head of, against whose document a `#id`
 *  in the name is read: the far node where it is a comment, an ask or a message. Any other node
 *  is named by a document's name, an issue's title or a component's, which is the text of no
 *  record a `#id` could name a question of. */
function nameSource(node: ReferenceLink["node"]): TextSource | undefined {
  return node.kind === "comment" || node.kind === "ask" || node.kind === "message"
    ? textSourceOf(node.kind, node.id, node.issue_key)
    : undefined;
}

/** A references row's name, as the link to its node, or as words where the node opens no page.
 *  A comment or a message, or a block of one, carries its post's marks: a deleted one is named
 *  by the deleted line (`DeletedBody`) and never by the words the read gave, and an edited one
 *  says ` · edited` after its name (`EditedMark`). */
function NodeName({
  deleted,
  edited,
  href,
  name,
}: {
  deleted: PostDeletion | null;
  edited: string | null | undefined;
  href: string | undefined;
  name: string;
}): ReactNode {
  const drawn =
    deleted === null ? (
      <>
        {name}
        <EditedMark at={edited} className={`font-normal ${textMutedOnSurface}`} />
      </>
    ) : (
      <DeletedBody deleted={deleted} />
    );
  return href === undefined ? (
    <span className="font-medium">{drawn}</span>
  ) : (
    <Link className={`font-medium underline ${linkText} ${linkHoverText}`} to={href}>
      {drawn}
    </Link>
  );
}

/** One row of the references read: a link to the node at the far end through its own reference,
 *  named by the words the read gives for it, then the node's kind and the kind of the issue's node
 *  at the near end, which a row of `links` links from (`via`) and a row of `linked_from` links to
 *  (`to`). A document's row names its owner first, as a member's row does. A node with no
 *  reference (a session) has no page to open, so its row names it without a link. A comment or a
 *  message, or a block of one, carries its post's marks (`NodeName`).
 *
 *  A name that is the head of a record's text - a comment's, an ask's or a message's
 *  (`nameSource`) - is drawn as its plain-text projection, the reading list's
 *  `artifacts-reference-name`: a link's words hold no markup, so the formatting goes and a
 *  reference in the name reads as its target's name, never its address. It is read from the tree
 *  the server sent of it, and from a parse of the name where a Dispatch from before `name_tree`
 *  sent none. The words are the whole name, as the server cut it, and none until the projection
 *  has answered, which is one pre-paint commit on a page whose engine is loaded. Any other name -
 *  a document's name, an issue's title, a component's - is drawn as the characters it is, as the
 *  reference graph's excerpts draw those (`api/display_trees.go`'s `graphExcerptRead`), so a
 *  document called `*draft*.md` keeps its asterisks. A deleted post's name is its deleted line,
 *  which holds none of the words the read gave. */
function ReferenceLinkRow({
  alsoReferences,
  near,
  row,
}: {
  alsoReferences: boolean;
  near: "via" | "to";
  row: ReferenceLink;
}): ReactNode {
  const { node } = row;
  const route = node.ref === undefined ? undefined : parseNodeReference(node.ref);
  // The path the reference opens, through the dashboard's one gate (`referenceHref`); a node with
  // no reference, and a person, whom no page shows, open none.
  const href = route === undefined ? undefined : referenceHref(route);
  const source = nameSource(node);
  const deleted = postDeletion(node);
  return (
    <li className={`rounded-lg border p-3 text-sm ${borderDefault}`}>
      {source === undefined || deleted !== null ? (
        <NodeName deleted={deleted} edited={node.edited_at} href={href} name={row.name} />
      ) : (
        <PlainText
          markdown={row.name}
          source={source}
          surface="artifacts-reference-name"
          tree={row.name_tree}
        >
          {(name) => (
            <NodeName deleted={null} edited={node.edited_at} href={href} name={name ?? ""} />
          )}
        </PlainText>
      )}
      <p className={textMutedOnSurface}>
        {node.kind === "artifact" ? (
          <>
            {/* A node names its issue or its project; an agent's artifact has neither and
                belongs to the session whose conversation holds it. */}
            <ArtifactOwner
              artifact={{
                issue_key: node.issue_key ?? null,
                project: node.project ?? "",
                session_id: node.parent?.kind === "session" ? node.parent.id : null,
              }}
            />{" "}
            ·{" "}
          </>
        ) : null}
        {node.kind} · {near} {row.via.kind}
        {alsoReferences ? " · also references this issue" : null}
      </p>
    </li>
  );
}

/** The References disclosure of an answer from a build before the one-hop read, drawn as that
 *  build's walk always was: the documents of its `members`, each with its depth, and a line when
 *  the walk was cut at 8 hops; nothing when it has no member. This path lasts only while such a
 *  build is deployed. */
function MemberReferences({
  references,
}: {
  references: Pick<IssueReferencesAnswer, "members" | "truncated">;
}): ReactNode {
  const members = references.members ?? [];
  if (members.length === 0) {
    return null;
  }
  return (
    <ReferencesDisclosure count={members.length} label="References">
      <section aria-label="References" className="mt-3 space-y-2">
        <ul className="space-y-2">
          {members.map(({ artifact, depth, via }) => (
            <li className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={artifact.id}>
              <Link
                className={`font-medium underline ${linkText} ${linkHoverText}`}
                to={artifactPath(artifact)}
              >
                {artifact.name}
              </Link>
              <p className={textMutedOnSurface}>
                <ArtifactOwner artifact={artifact} /> · via {via.kind} · depth {depth}
              </p>
            </li>
          ))}
        </ul>
        {references.truncated ? (
          <p className={`text-sm ${textMutedOnSurface}`}>more references beyond 8 hops</p>
        ) : null}
      </section>
    </ReferencesDisclosure>
  );
}

// The issue's references read, one hop in each direction, drawn as two disclosures, each collapsed
// by default and each counting its rows in its summary, so each count shows while it is closed:
// References, each node the issue's own content links to, and under it Referenced by, each node
// that links to the issue or to its own content. A disclosure with no row is not drawn. A node of
// both is drawn once, under References, marked as one that also references the issue
// (`issueReferenceSections`). Each row's node and name come in the answer, so the section reads
// nothing to draw them, and a one-hop read cuts no walk, so no row has a depth. This is not the
// issue header's "Referenced by" toggle, which reads the edges into the issue's own node alone. An
// answer without `links` is drawn by `MemberReferences`. It is memoized: a keystroke in the tab's
// filter renders the tab again, and the answer, the one prop, is the same object until the read
// answers anew.
const References = memo(function References({
  references,
}: {
  references: IssueReferencesAnswer;
}): ReactNode {
  if (references.links === undefined) {
    return <MemberReferences references={references} />;
  }
  const sections = issueReferenceSections(references);
  if (sections.references.length === 0 && sections.referencedBy.length === 0) {
    return null;
  }

  return (
    <div className="space-y-3">
      {sections.references.length === 0 ? null : (
        <ReferencesDisclosure count={sections.references.length} label="References">
          <section aria-label="References" className="mt-3">
            <ul className="space-y-2">
              {sections.references.map(({ alsoReferences, row }) => (
                <ReferenceLinkRow
                  alsoReferences={alsoReferences}
                  key={graphNodeKey(row.node)}
                  near="via"
                  row={row}
                />
              ))}
            </ul>
          </section>
        </ReferencesDisclosure>
      )}
      {sections.referencedBy.length === 0 ? null : (
        <ReferencesDisclosure count={sections.referencedBy.length} label="Referenced by">
          <section aria-label="Referenced by" className="mt-3">
            <ul className="space-y-2">
              {sections.referencedBy.map((row) => (
                <ReferenceLinkRow
                  alsoReferences={false}
                  key={graphNodeKey(row.node)}
                  near="to"
                  row={row}
                />
              ))}
            </ul>
          </section>
        </ReferencesDisclosure>
      )}
    </div>
  );
});

/**
 * One artifact of the issue. Its groups, in this order: the name (the kind's icon, an image's
 * thumbnail, the name, which truncates, and under it the facts - kind, versions, size and when it
 * was updated - which wrap between facts, never inside one); its state (Primary, and the approval
 * chip); and its actions (copy the reference, download, remove). When the row is too narrow a
 * group moves to the next line whole.
 */
function ArtifactRow({
  artifact,
  isClosed,
  issueKey,
}: {
  artifact: Artifact;
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const versions = versionsNewestFirst(artifact.versions);
  const latestVersion = versions[0];

  return (
    <li
      className={`flex flex-wrap items-center gap-x-3 gap-y-2 border-b py-3 last:border-b-0 ${borderDefault}`}
      data-testid={`artifact-${artifact.slug}`}
    >
      <div className="flex min-w-32 flex-1 items-center gap-3">
        <span className={`shrink-0 ${textSecondaryOnSurface}`}>{kindIcon(artifact.kind)}</span>
        {artifact.kind === "image" && latestVersion !== undefined ? (
          <img
            alt={`${artifact.name} version ${latestVersion.number}`}
            className={`h-10 w-10 shrink-0 rounded border object-cover ${borderDefault}`}
            loading="lazy"
            src={artifactVersionUrl(artifact.id, latestVersion.number)}
          />
        ) : null}
        <div className="min-w-0 flex-1">
          {/* `max-w-full`: below 1280 px the link is inline-flex rather than `block`, and an
              inline-flex box is as wide as its whole name unless something caps it. */}
          <Link
            className={`block max-w-full truncate font-medium underline ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug })}
          >
            <TruncatedText title={artifact.name}>{artifact.name}</TruncatedText>
          </Link>
          <p className={`text-xs ${textMutedOnSurface}`}>
            <span className="whitespace-nowrap">{artifact.kind}</span>
            {" · "}
            <span className="whitespace-nowrap">
              {versions.length} {versions.length === 1 ? "version" : "versions"}
            </span>
            {" · "}
            {latestVersion?.size === undefined ? null : (
              <>
                <span className="whitespace-nowrap">{formatArtifactBytes(latestVersion.size)}</span>
                {" · "}
              </>
            )}
            <span className="whitespace-nowrap">
              Updated <Timestamp at={latestVersion?.created_at ?? artifact.created_at} />
            </span>
          </p>
        </div>
      </div>
      <div className="flex min-w-0 max-w-full flex-wrap items-center gap-2 empty:hidden">
        {artifact.primary ? (
          <span
            className={`shrink-0 rounded-full px-2 py-1 text-xs font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
          >
            Primary
          </span>
        ) : null}
        <ApprovalChip artifact={artifact} />
      </div>
      <div className="flex min-w-0 max-w-full flex-wrap items-center gap-2">
        <CopyRefButton route={documentRoute(artifact)} />
        {latestVersion === undefined ? null : (
          <a
            className={`shrink-0 text-sm font-medium underline ${linkText} ${linkHoverText}`}
            download=""
            href={artifactVersionUrl(artifact.id, latestVersion.number, { download: true })}
          >
            Download version {latestVersion.number}
          </a>
        )}
        {/* A removal is offered only where the server can take one: a document of this issue
            that is neither its spec nor a document that plays a role, since those are refused
            whatever state they are in (`removalIsOffered`). Every other refusal is the server's,
            shown on the row. */}
        {removalIsOffered(artifact) ? (
          <RemoveDocument
            artifact={artifact}
            refusal={isClosed ? CLOSED_ISSUE_REMOVE_REFUSAL : null}
          />
        ) : null}
      </div>
    </li>
  );
}

/** **Upload**'s glyph, drawn before its words where the control bar draws it. */
const UPLOAD_GLYPH = textGlyph("↑");

/** **Upload**'s label, which the bar's action is named by (`BarAction`) and found by. */
const UPLOAD_LABEL = "Upload";

/**
 * The issue's artifacts: a control bar (`ControlRow`) of the search box and two actions, **New
 * document** and **Upload**, which a closed issue refuses, and every artifact the issue holds.
 * Below 640 px of the bar's width both actions are items of **More**; the bar has no trigger and
 * no chip, since its one narrowing is the words typed. What the actions open stands under the
 * bar, outside it: the New document form and a staged file's summary and its own Upload. While a
 * file is staged the bar offers no **Upload**: the staged file's own is the one press that stores
 * it, and two buttons of one name would do two things.
 */
export function ArtifactsTab({ isClosed }: { readonly isClosed: boolean }): ReactNode {
  const { pathname } = usePageAddress();
  const route = parseIssuePath(pathname);
  const issueKey = route?.key;
  // The words that narrow the list live in the address (`?q=`), so a reader who reloads, shares
  // the tab or comes back to it finds the list narrowed the way they left it.
  const [filter, setFilter] = useCappedAddressParam("q");
  // One sentence covers both actions of the bar: a closed issue takes neither a new document
  // nor an upload, and both actions are described by the line under the bar that says it.
  const closedRefusalId = useId();
  const refusal: BarActionRefusal | undefined = isClosed
    ? { id: closedRefusalId, words: CLOSED_ISSUE_DOCUMENT_REFUSAL }
    : undefined;
  const upload = useArtifactUpload({ issue: issueKey ?? "" }, { refusal: refusal?.words ?? null });
  const tab = useRef<HTMLDivElement>(null);
  const artifacts = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["artifacts", issueKey],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Artifact list requires an issue route.");
      }
      return api.listArtifacts(issueKey);
    },
  });
  const references = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey, "references"],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Issue references require an issue route.");
      }
      return api.getIssueReferences(issueKey);
    },
  });
  const orderedArtifacts = useMemo(
    () =>
      [...(artifacts.data ?? [])].sort((left, right) => {
        if (left.primary !== right.primary) {
          return left.primary ? -1 : 1;
        }
        const leftUpdated = (left.versions ?? []).at(-1)?.created_at ?? left.created_at;
        const rightUpdated = (right.versions ?? []).at(-1)?.created_at ?? right.created_at;
        return compareTimestamps(rightUpdated, leftUpdated) || left.id.localeCompare(right.id);
      }),
    [artifacts.data]
  );
  // The New document form hands the focus back, as it closes, to the action that opened it: the
  // bar's first action, or **More** where the bar holds its actions there. One function for every
  // render, since the form's effect reads it (`TitleForm`).
  const opener = useCallback(
    () =>
      tab.current === null
        ? null
        : (controlIn(tab.current, CONTROL.barAction) ?? controlIn(tab.current, CONTROL.moreMenu)),
    []
  );
  const newDocument = useNewDocument({
    documents: orderedArtifacts,
    opener,
    owner: { issue: issueKey ?? "" },
    refusal,
  });
  // The staged file's line hands the focus it goes with - its Upload landed, its Cancel pressed -
  // to the bar's **Upload**, which the same commit draws again among the bar's actions, found by
  // its name, or to **More** where the bar holds its actions there (`ArtifactUploadStaging`).
  const uploadAction = useCallback(
    () =>
      tab.current === null
        ? null
        : (namedControlIn(tab.current, CONTROL.barAction, UPLOAD_LABEL) ??
          controlIn(tab.current, CONTROL.moreMenu)),
    []
  );
  const filterQuery = filter.trim().toLocaleLowerCase();
  const visibleArtifacts =
    filterQuery === ""
      ? orderedArtifacts
      : orderedArtifacts.filter(
          (artifact) =>
            artifact.name.toLocaleLowerCase().includes(filterQuery) ||
            artifact.kind.toLocaleLowerCase().includes(filterQuery)
        );

  if (issueKey === undefined) {
    return (
      <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Open an issue to view its artifacts.</p>
    );
  }
  if (artifacts.isPending) {
    return <p className={`pt-3 text-sm ${textMutedOnSurface}`}>Loading artifacts…</p>;
  }
  if (artifacts.isError) {
    return <p className={`pt-3 text-sm ${dangerText}`}>{ARTIFACTS_NOT_LOADED}</p>;
  }

  const actions: BarActionProps[] = [
    newDocument.action,
    ...(upload.pendingFile === null
      ? [{ icon: UPLOAD_GLYPH, label: UPLOAD_LABEL, onPress: upload.openPicker, refusal }]
      : []),
  ];

  return (
    <div className="space-y-3 pt-3" ref={tab}>
      <ControlRow
        actions={actions}
        search={{
          label: "Filter artifacts",
          onChange: setFilter,
          placeholder: "Filter artifacts",
          value: filter,
        }}
      />
      {/* What the bar's actions open stands under the bar, outside it. */}
      {newDocument.form}
      <ArtifactUploadStaging handBack={uploadAction} upload={upload} />
      {refusal === undefined ? null : (
        <p className={`text-xs ${textMutedOnSurface}`} id={refusal.id}>
          {refusal.words}
        </p>
      )}
      {/* A references read that fails - a read cut at the server's bound among them, which the
          shared retry policy leaves to this Retry - says so rather than hiding the section. */}
      {references.isError ? (
        <QueryError
          error={references.error}
          message={`References unavailable: ${apiErrorMessage(references.error, "the read failed")}`}
          onRetry={() => void references.refetch()}
          retrying={references.isFetching}
        />
      ) : references.data === undefined ? null : (
        <References references={references.data} />
      )}
      <ArtifactDropZone upload={upload}>
        {visibleArtifacts.length === 0 ? (
          <p className={`text-sm ${textMutedOnSurface}`}>
            {orderedArtifacts.length === 0
              ? "No artifacts yet."
              : "No artifacts match this filter."}
          </p>
        ) : (
          <ul className={`rounded-lg border px-3 ${borderDefault}`}>
            {visibleArtifacts.map((artifact) => (
              <ArtifactRow
                artifact={artifact}
                isClosed={isClosed}
                issueKey={issueKey}
                key={artifact.id}
              />
            ))}
          </ul>
        )}
      </ArtifactDropZone>
    </div>
  );
}
