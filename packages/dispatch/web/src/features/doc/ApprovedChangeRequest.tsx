import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useId } from "react";
import { Link } from "react-router-dom";

import { isNoRoute } from "../../api/client";
import { changeRequestQuery, specNodeQuery } from "../../api/queries";
import type { Artifact, SpecNodeRead, SpecProvenance } from "../../api/types";
import {
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { blockParts } from "./change-request";

/** One node an approval wrote, as the issue shows it after the approval. */
interface ApprovedNode {
  readonly id: string;
  readonly kind: string;
  readonly what: "added" | "changed" | "removed";
  /** The approved version the approval wrote, or the version it removed. */
  readonly version: number;
  readonly block: string;
  readonly at: string;
  readonly provenance: SpecProvenance;
}

/** The approval an issue shows its change request as approved by: the spec's version it approved
 *  and the approval request it answered. */
interface Approval {
  readonly issueKey: string;
  readonly version: number;
  readonly ask: string | null;
}

/** Whether `provenance` is the approval's: of this issue and this spec version, by its request. */
function wroteBy(provenance: SpecProvenance, approval: Approval): boolean {
  return (
    provenance.issue === approval.issueKey &&
    provenance.spec?.version === approval.version &&
    (approval.ask === null || provenance.ask === approval.ask)
  );
}

/**
 * What `approval` wrote of the node `read`: the approved version it wrote (added when the node had
 * no version before it, or its version before was removed), or the version it removed. One approval
 * writes a node once. Undefined when the approval wrote nothing of it: a version another approval
 * wrote is no part of this approval's change request.
 */
function approvedNode(read: SpecNodeRead, approval: Approval): ApprovedNode | undefined {
  const versions = read.versions ?? [];
  const removals = read.removals ?? [];
  const removal = removals.find((entry) => wroteBy(entry.provenance, approval));
  if (removal !== undefined) {
    const removedVersion = versions.find((entry) => entry.version === removal.version);
    if (removedVersion === undefined) return undefined;
    return {
      at: removal.at,
      block: removedVersion.block,
      id: read.id,
      kind: read.kind,
      provenance: removal.provenance,
      version: removal.version,
      what: "removed",
    };
  }
  const written = versions.find((entry) => wroteBy(entry.provenance, approval));
  if (written === undefined) return undefined;
  const added =
    written.version === 1 || removals.some((entry) => entry.version === written.version - 1);
  return {
    at: written.at,
    block: written.block,
    id: read.id,
    kind: read.kind,
    provenance: written.provenance,
    version: written.version,
    what: added ? "added" : "changed",
  };
}

/** How the approval left a node, as a row's heading says it. */
const WHAT_WORDS: Readonly<Record<ApprovedNode["what"], string>> = {
  added: "Added",
  changed: "Changed",
  removed: "Removed",
};

function ApprovedRow({ node, issueKey }: { node: ApprovedNode; issueKey: string }): ReactNode {
  const parts = blockParts(node.block, "approved");
  const { provenance } = node;
  return (
    <li className="space-y-1" data-node-id={node.id} data-what={node.what}>
      <p className={`text-sm font-semibold ${textSecondaryOnSurface}`}>
        {WHAT_WORDS[node.what]} {node.kind}{" "}
        <code className="[overflow-wrap:anywhere]">{node.id}</code>
      </p>
      {parts.attributes.map(([name, value]) => (
        <p className={`text-xs ${textMutedOnSurface}`} key={name}>{`${name} ${value}`}</p>
      ))}
      <p
        className={`text-sm whitespace-pre-wrap [overflow-wrap:anywhere] ${textPrimaryOnSurface}`}
        data-testid="approved-change-request-text"
      >
        {parts.body}
      </p>
      <p
        className={`text-xs ${textMutedOnSurface}`}
        data-testid="approved-change-request-provenance"
      >
        <span>version {node.version}</span>, approved by{" "}
        <AgentName actor={provenance.person} inline /> <Timestamp at={node.at} />
        {provenance.ask === undefined ? null : (
          <>
            {" "}
            in{" "}
            <Link
              className={`${linkText} ${linkHoverText}`}
              to={buildIssuePath({ id: provenance.ask, key: issueKey, kind: "ask" })}
            >
              the approval request
            </Link>
          </>
        )}
      </p>
    </li>
  );
}

/**
 * The change request as the person approved it (R22: "After the approval, the issue shows the
 * change request as the person approved it, with its digest"), on the issue's Spec tab. It reads
 * which nodes the request named from the change request at the version the person approved, and
 * each node's text from the approved version that approval wrote, with its provenance: who
 * approved, when, and the approval request. It never shows the issue's own blocks, which may
 * change after the approval. The digest is the one the approval pinned on the spec. It shows
 * nothing for an issue of no spec source, before an approval, after a request for changes, and
 * while no node names this approval.
 */
export function ApprovedChangeRequest({
  issueKey,
  spec,
}: {
  issueKey: string;
  spec: Artifact;
}): ReactNode {
  const { approval: state } = spec;
  const headingId = useId();
  const approval: Approval | undefined =
    state !== undefined &&
    (state.state === "approved" || state.state === "stale") &&
    state.version !== undefined
      ? { ask: state.ask_id ?? null, issueKey, version: state.version }
      : undefined;
  const request = useQuery({
    ...changeRequestQuery(issueKey, approval?.version ?? 0),
    enabled: approval !== undefined,
  });
  const read = request.data;
  const named =
    read === undefined || !read.spec_source
      ? []
      : [...(read.proposals ?? []), ...(read.changed_checks ?? [])];
  const nodes = useQueries({
    queries: named.map((node) => specNodeQuery(spec.project, read?.repo ?? "", node.id)),
  });
  if (approval === undefined) return null;
  // A failed read says so rather than showing part of the request or nothing; a Dispatch older
  // than the change request (its read has no route) holds no approved spec to show.
  if ((request.isError && !isNoRoute(request.error)) || nodes.some((node) => node.isError)) {
    return (
      <p className={`text-sm ${dangerText}`} role="alert">
        Could not read the approved change request.
      </p>
    );
  }
  const shown = nodes.flatMap((node) => {
    const written = node.data === undefined ? undefined : approvedNode(node.data, approval);
    return written === undefined ? [] : [written];
  });
  if (shown.length === 0) return null;
  const digest = shown[0]?.provenance.spec?.digest;

  return (
    <section aria-labelledby={headingId} className={`space-y-3 rounded-lg border p-3 ${card}`}>
      <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`} id={headingId}>
        Approved change request
      </h2>
      <ul aria-label="What the approval wrote" className="space-y-3">
        {shown.map((node) => (
          <ApprovedRow issueKey={issueKey} key={`${node.kind} ${node.id}`} node={node} />
        ))}
      </ul>
      {digest === undefined ? null : (
        <div className="space-y-1">
          <p className={`text-xs ${textMutedOnSurface}`}>
            The approval pinned this digest of version {approval.version} of the spec:
          </p>
          <p
            className={`font-mono text-xs break-all ${textPrimaryOnSurface}`}
            data-testid="approved-change-request-digest"
          >
            {digest}
          </p>
        </div>
      )}
    </section>
  );
}
