import type { ReactNode } from "react";

import { FocusedNodeProvider } from "../focused-node";
import { NoTooltips } from "../NoTooltips";
import { OutlineRequestProvider } from "../outline-request";
import { OutlineWriterProvider } from "../outline-writer";
import type { SlotProps } from "../slot-props";
import { DocumentToolbarsProvider } from "../useDocumentToolbars";

/** Filled by INFRA-1196 part G, "A node's row". */
export function ProvidersSlot({
  children,
  issue,
}: SlotProps & { readonly children: ReactNode }): ReactNode {
  // The row writer (`outline-writer.tsx`): what the reader's next words answer, which the
  // body's rows set and the foot's box and Details' history read, so it stands above all of
  // them (part G's R7, decisions 18 and 29). Inside `NoTooltips`, so it names a reply's target
  // as the page names an author.
  //
  // Inside it, the outline's three: the node the reader's focus is on, which
  // the outline sets and Details reads, held for one issue (`focused-node.tsx`); what the
  // Contents list asks of the outline, and its Focus branch, which the pane, the sheet and the
  // outline share (`outline-request.tsx`); and each open document's toolbar, which the body's
  // row editor reports and the view row's version control is to read: nothing reads
  // it yet (`useDocumentToolbars.ts`).
  return (
    <NoTooltips>
      <OutlineWriterProvider issueKey={issue.key}>
        {/* A later part adds the providers that two slots share here, by lines added. */}
        <FocusedNodeProvider key={issue.key}>
          <OutlineRequestProvider>
            <DocumentToolbarsProvider>
              {/* Each slot the root draws. */}
              {children}
            </DocumentToolbarsProvider>
          </OutlineRequestProvider>
        </FocusedNodeProvider>
      </OutlineWriterProvider>
    </NoTooltips>
  );
}
