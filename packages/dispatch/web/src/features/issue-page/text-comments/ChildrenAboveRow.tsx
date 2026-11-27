import type { ReactNode } from "react";

import { useMediaQuery } from "../../shell/useDialog";
import { ChildrenSection } from "../children-section";
import { CONTENTS_PANE_QUERY, PAGE_INSET } from "../contents-frame";
import type { SlotProps } from "../slot-props";

/**
 * The issue's children (`children-section.tsx`) where the page's root draws them from 1024 px
 * (`CONTENTS_PANE_QUERY`): above the row of the main column and the column of comments on text,
 * beside the Contents pane and as wide as it leaves the page, inside the page's insets (R2). So
 * the column narrows the stage and never the section, and the column's top stands under the
 * section's foot. The section stands 10 px under the view row, as the main column's first row
 * does where the issue has no children; the main column's own 10 px over its first row then
 * stands inside the section's 16 px foot margin (`-mb-2.5`), so that row stands 16 px under the
 * section, as when the stage drew the section above it. Where the section draws nothing, this
 * draws no box (`empty:hidden`). Below 1024 px it draws nothing: no column stands there, and the
 * stage draws the section under the issue's three sentences (`ChildrenInStage`).
 */
export function ChildrenAboveRow(props: SlotProps): ReactNode {
  const wide = useMediaQuery(CONTENTS_PANE_QUERY);
  if (!wide) return null;
  return (
    <div className={`-mb-2.5 pt-2.5 empty:hidden ${PAGE_INSET}`}>
      <ChildrenSection {...props} />
    </div>
  );
}

/**
 * The issue's children where the stage draws them, above its slot's first row (R14): at every
 * width where the page's root draws none above its row (`above` false, as for a stage drawn
 * alone), and else below 1024 px alone, under the issue's three sentences, where the root's
 * `ChildrenAboveRow` draws nothing.
 */
export function ChildrenInStage({
  above,
  ...props
}: SlotProps & {
  /** Whether the page's root draws the children above its row from 1024 px (`ChildrenAboveRow`). */
  readonly above: boolean;
}): ReactNode {
  const wide = useMediaQuery(CONTENTS_PANE_QUERY);
  if (above && wide) return null;
  return <ChildrenSection {...props} />;
}
