import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { whoAmIQuery } from "../../../api/queries";
import { useAddressTarget } from "../address-target";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { SpecFilterBody } from "../spec-filter/SpecFilterBody";
import { useIssueTree } from "../useIssueTree";

/**
 * The Spec view's slot: what the stage draws in place of the outline
 * with Spec on (`slots/stage.tsx`). It is `SpecFilterBody` over the issue's one tree as the Spec
 * view leaves it (`presetView`), at the width the approved picture draws it (the prototype's
 * `.spec`, 1080 px), and reads everything else itself: the tree (`useIssueTree`), who is reading,
 * the Contents list's last request (`useOutlineRequest`), and the node the address names, which
 * the view keeps whatever the words filter leaves and puts the focus on.
 */
export function ViewSpecSlot({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  // The body watches the reads it draws from itself, under the keys the tree fills, so it waits
  // on those alone (`SpecFilterBody`).
  const { root } = useIssueTree(issue);
  // Who is reading, so a decision of theirs reads as their own words (R5).
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // The address's own node, resolved against the whole tree and kept by the view
  // (`useAddressTarget`, as the outline reads it).
  const { named, reveal } = useAddressTarget(root, address);
  const revealed = reveal?.at(-1)?.id;
  const view = useMemo(
    () => presetView(root, "spec", address.filter, named),
    [address.filter, named, root]
  );
  const request = useOutlineRequest();
  return (
    <NoTooltips>
      {/* The view's measure, the prototype's `.spec`: at most 1080 px, in the middle of the
          column; on a phone 4 px from the left edge and 10 px from the right, as the outline. */}
      <div className="mx-auto flex w-full max-w-[1080px] min-w-0 flex-col max-[761px]:-mr-1 max-[761px]:-ml-2.5 max-[761px]:w-auto">
        <SpecFilterBody
          context={view.context}
          emptyLine={address.filter === "" ? EMPTY_STATE_LINES.spec : EMPTY_STATE_LINES.no_match}
          // The words filter's Escape comes before the view is left; turning the view off with
          // Escape is a later step of the page's key table.
          holdsEscape={address.filter !== ""}
          issue={issue}
          reader={viewer}
          {...(request === undefined ? {} : { request })}
          {...(revealed === undefined ? {} : { reveal: revealed })}
          root={root}
          view={view.root}
        />
      </div>
    </NoTooltips>
  );
}
