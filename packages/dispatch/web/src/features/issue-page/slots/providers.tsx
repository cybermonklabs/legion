import type { ReactNode } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { DocumentAdmissionProvider } from "../document-admission";
import { FocusedNodeProvider } from "../focused-node";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { OutlineRequestProvider } from "../outline-request";
import { OutlineWriterProvider } from "../outline-writer";
import type { SlotProps } from "../slot-props";
import { DocumentToolbarsProvider } from "../useDocumentToolbars";

/** Filled by INFRA-1196 part G, "A node's row". */
export function ProvidersSlot({
  children,
  issue,
  route,
}: SlotProps & { readonly children: ReactNode }): ReactNode {
  const { hash, search } = useLocation();
  // The row writer (`outline-writer.tsx`): what the reader's next words answer, which the
  // body's rows set and the foot's box and Details' history read, so it stands above all of
  // them (part G's R7, decisions 18 and 29). Inside `NoTooltips`, so it names a reply's target
  // as the page names an author. Under Threads, read off the address as the foot reads it
  // (`slots/foot.tsx`), the page mounts no box at the foot, so Details' Reply and Edit there set
  // no target (the writer's `threads`, decision 127).
  //
  // Inside it, the outline's three: the node the reader's focus is on, which
  // the outline sets and Details reads, held for one issue (`focused-node.tsx`); what the
  // Contents list asks of the outline, and its Focus branch, which the pane, the sheet and the
  // outline share (`outline-request.tsx`); and each open document's toolbar, which the body's
  // row editor reports and the view row's version control is to read: nothing reads
  // it yet (`useDocumentToolbars.ts`).
  //
  // Innermost, what each document's room last admitted the page to, which the body's row editor
  // reports and the outline's rows read, so no row of a document its room admitted read-only
  // draws the cue that it takes typing (`document-admission.tsx`).
  const threads = issueAddress(route, search, hash, primarySpec(issue)?.slug).preset === "threads";
  return (
    <NoTooltips>
      <OutlineWriterProvider issueKey={issue.key} openAsks={issue.open_asks} threads={threads}>
        {/* A later part adds the providers that two slots share here, by lines added. */}
        <FocusedNodeProvider key={issue.key}>
          <OutlineRequestProvider>
            <DocumentToolbarsProvider>
              <DocumentAdmissionProvider>
                {/* Each slot the root draws. */}
                {children}
              </DocumentAdmissionProvider>
            </DocumentToolbarsProvider>
          </OutlineRequestProvider>
        </FocusedNodeProvider>
      </OutlineWriterProvider>
    </NoTooltips>
  );
}
