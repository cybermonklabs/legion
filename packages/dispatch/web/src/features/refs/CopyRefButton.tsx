import type { ReactNode } from "react";

import { CopyButton } from "../../components/CopyButton";
import { buildDispatchReference, type DispatchReferenceRoute } from "./routes";

/** Matches the copy control of an Inbox item itself - on its row, its card or the reading pane's
 *  head (`item`) - and never one of a comment or a reply drawn inside it: what the Inbox's `y`
 *  presses. */
export const ITEM_COPY_REF_SELECTOR = '[data-copy-ref="item"] button';

const MODIFIER =
  typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";

/** The one copy control for a node's `dispatch://` reference, shown wherever the node is. A click
 *  copies the reference. When the surface has a shorter identifier worth keeping one click away
 *  — the issue header's key — `primary` names it: a click copies `primary.value` and a
 *  Ctrl/Cmd-click copies the reference, and the button's name says so. `shortcutKeys` are the
 *  keys that copy it where the host draws them on the button (the Inbox's `y`, `CopyButton`).
 *  `item` marks the control of an Inbox item itself (`ITEM_COPY_REF_SELECTOR`). */
export function CopyRefButton({
  className,
  item = false,
  primary,
  route,
  shortcutKeys,
}: {
  className?: string;
  item?: boolean;
  primary?: { label: string; value: string };
  route: DispatchReferenceRoute;
  shortcutKeys?: readonly string[];
}): ReactNode {
  const reference = buildDispatchReference(route);
  return (
    <span className="contents" data-copy-ref={item ? "item" : ""}>
      {primary === undefined ? (
        <CopyButton
          className={className}
          shortcutKeys={shortcutKeys}
          value={reference}
          what="reference"
        />
      ) : (
        <CopyButton
          className={className}
          label={`Copy ${primary.label} · ${MODIFIER}-click copies the reference`}
          shortcutKeys={shortcutKeys}
          value={(event) => (event.metaKey || event.ctrlKey ? reference : primary.value)}
          what="reference"
        />
      )}
    </span>
  );
}
