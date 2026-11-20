import type { ReactNode } from "react";

import { NoTooltips } from "../NoTooltips";
import { OutlineWriterProvider } from "../outline-writer";
import type { SlotProps } from "../slot-props";

/** Filled by INFRA-1196 part G, "A node's row". */
export function ProvidersSlot({
  children,
  issue,
}: SlotProps & { readonly children: ReactNode }): ReactNode {
  // The row writer (`outline-writer.tsx`): what the reader's next words answer, which the
  // body's rows set and the foot's box and Details' history read, so it stands above all of
  // them (part G's R7, decisions 18 and 29). Inside `NoTooltips`, so it names a reply's target
  // as the page names an author.
  return (
    <NoTooltips>
      <OutlineWriterProvider issueKey={issue.key}>
        {/* A later part adds the providers that two slots share here, by lines added. */}
        {children}
      </OutlineWriterProvider>
    </NoTooltips>
  );
}
