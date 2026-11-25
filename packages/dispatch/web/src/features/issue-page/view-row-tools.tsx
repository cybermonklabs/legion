import type { ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { useMediaQuery } from "../shell/useDialog";
import { OutlineSheet } from "./contents-frame";
import { useOpenedDocument, useOutlineAddress } from "./document-address";
import { HEADER_SCROLLS_QUERY } from "./identity-block";
import { issueAddress } from "./issue-address";
import type { SlotProps } from "./slot-props";
import { useFollowing } from "./slots/following";
import { useDocumentToolbars } from "./useDocumentToolbars";
import { useIssueDetail } from "./useIssueDetail";
import { VersionControl } from "./VersionControl";

/** What part N hands the view row's `IssuePresets` beside part D's props (its R3, R4). */
export interface ViewRowTools {
  readonly filter: string;
  readonly onFilterChange: (next: string) => void;
  /** Present at 760 px and below only: the Outline sheet that holds the two small controls,
   *  which the row draws in place of part D's Outline control (`slots/view-row.tsx`). */
  readonly sheet?: (tools: ReactNode) => ReactNode;
  readonly toolbar: ReactNode;
}

/**
 * The view row's two small controls, as `IssuePresets` props (dispatch://INFRA-1196 part N's
 * R3, R4): the filter by words, which writes `?q=` in place; and the version control, which reads
 * the opened document's versions, else the view's document's, else is not drawn. At 760 px and
 * below the two stand in the Outline sheet, which the row then draws with a view on or a document
 * open too.
 */
export function useViewRowTools(props: SlotProps): ViewRowTools {
  const { issue, route } = props;
  const navigate = useNavigate();
  const { hash, pathname, search } = useLocation();
  const following = useFollowing();
  const { planArtifact, primaryArtifact, verificationArtifact } = useIssueDetail(issue, route);
  const address = issueAddress(route, search, hash, primaryArtifact?.slug);
  const opened = useOpenedDocument(issue, route);
  const outline = useOutlineAddress(issue.key);
  const { diffs, showDiff, toolbars } = useDocumentToolbars();
  const phone = useMediaQuery(HEADER_SCROLLS_QUERY);
  const isClosed = issue.closed_at !== null;
  // What the filter by words holds is `?q=`, which the box writes in place, keeping the rest of
  // the address, by a replace marked with the following it was typed in (`slots/following.ts`),
  // so the stage never swaps a view for the outline while the reader types (decision 368). The
  // mark keeps the outline an opened document goes back to, and leaves out a press of the
  // count of what waits (`waitingPress`), which acts once.
  const onFilterChange = (next: string) => {
    const query = new URLSearchParams(search);
    if (next === "") query.delete("q");
    else query.set("q", next);
    const rest = query.toString();
    navigate(`${pathname}${rest === "" ? "" : `?${rest}`}${hash}`, {
      replace: true,
      state: { outline, words: following },
    });
  };
  // A document opened at a version of its own: its own address at that version, which keeps the
  // outline it was opened from to go back to.
  const openDocumentAt = (artifact: Artifact, version: number | undefined) =>
    navigate(buildIssuePath({ key: issue.key, kind: "artifact", slug: artifact.slug, version }), {
      state: { outline },
    });
  // The document a view's version control reads while the outline or a view is drawn (R3): the
  // document of the view's role. The Decisions view has no document of its role, and with no
  // view on no version control is drawn.
  const viewDocument = (
    {
      conversation: undefined,
      decisions: undefined,
      plan: planArtifact,
      spec: primaryArtifact,
      verification: verificationArtifact,
    } as const
  )[address.preset];
  const openedDocument = opened?.artifact?.kind === "doc" ? opened.artifact : undefined;
  const toolbar =
    openedDocument !== undefined ? (
      <VersionControl
        artifact={openedDocument}
        isClosed={isClosed}
        onShowDiffChange={(next) => showDiff(openedDocument.id, next)}
        onVersionChange={(version) => {
          showDiff(openedDocument.id, false);
          openDocumentAt(openedDocument, version ?? undefined);
        }}
        showDiff={diffs.has(openedDocument.id)}
        toolbar={toolbars.get(openedDocument.id)}
        version={opened?.route.version}
      />
    ) : opened === undefined && viewDocument !== undefined ? (
      <VersionControl
        artifact={viewDocument}
        isClosed={isClosed}
        // Nothing in the view is the document's text to draw a comparison over, so the
        // comparison opens the document at the pinned version with its changes drawn.
        onShowDiffChange={(next) => {
          showDiff(viewDocument.id, next);
          if (next) openDocumentAt(viewDocument, address.version);
        }}
        // A version picked in a view pins the view's address to it (`?v=`), and keeps its words
        // filter; the fragment is a node of the version the reader is leaving.
        onVersionChange={(version) => {
          const query = new URLSearchParams(search);
          if (version === null) query.delete("v");
          else query.set("v", String(version));
          const rest = query.toString();
          navigate(`${pathname}${rest === "" ? "" : `?${rest}`}`);
        }}
        showDiff={false}
        toolbar={toolbars.get(viewDocument.id)}
        version={address.version}
      />
    ) : undefined;
  return {
    filter: address.filter,
    onFilterChange,
    toolbar,
    ...(phone ? { sheet: (tools: ReactNode) => <OutlineSheet slot={props} tools={tools} /> } : {}),
  };
}
