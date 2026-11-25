import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import { whoAmIQuery } from "../../../api/queries";
import { EVENT_FRESH } from "../../refs/reference-target";
import { useAddressTarget } from "../address-target";
import { useOutlineEditing } from "../IssuePresetPanels";
import { briefCounts } from "../IssueThreeThings";
import { issueAddress } from "../issue-address";
import { NoTooltips } from "../NoTooltips";
import { blocksKey, type ReadKey, useOutlineReads } from "../outline-reads";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import { SpecFilterBody } from "../spec-filter/SpecFilterBody";
import { nodesInOrder } from "../tree-keys";
import { useTellsAddressDrawn } from "./refused-reads";
import type { ViewSlotProps } from "./stage";

/**
 * The Spec view's slot: what the stage draws in place of the outline
 * with Spec on (`slots/stage.tsx`). It is `SpecFilterBody` over the issue's one tree as the Spec
 * view leaves it (`presetView`), at the width the approved picture draws it (the prototype's
 * `.spec`, 1080 px), and reads everything else itself: the tree and the outline's reads
 * (`useOutlineReads`), who is reading, the Contents list's last request (`useOutlineRequest`),
 * and the node the address names, which the view keeps whatever the words filter leaves and puts
 * the focus on.
 */
export function ViewSpecSlot({ acts, issue, onAddressDrawn, route }: ViewSlotProps): ReactNode {
  const { hash, search } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  // The body watches the reads it draws from itself, under the keys the tree fills, so it waits
  // on those alone (`SpecFilterBody`). The tree and the outline's reads come from one place
  // (`useOutlineReads`), under the keys and with the freshness the tree reads them by, so they
  // cost no request of their own and build no second tree. The names reads are the tree's, and
  // the view draws from them too - the three sentences, and the name of every reference a plain
  // line cites - so their line while they stand failed (`namesFailure`) is passed on for the body
  // to draw. The body lands on the node the address names only once every read the tree's rows
  // come from has settled (`readsSettled`), for the one reason `SpecFilterBody` gives.
  const { failed, namesFailure, namesHeld, readsHeld, readsSettled, root } = useOutlineReads(issue);
  // What the first group's two rows count, over the whole tree and under the same words filter as
  // the view row's signals, once the tree may be counted (`briefCounts`).
  const counts = useMemo(
    () => briefCounts({ namesHeld, readsHeld, root }, address.filter),
    [address.filter, namesHeld, readsHeld, root]
  );
  // Who is reading, so a decision of theirs reads as their own words (R5), read as the tree reads
  // it (`useIssueTree`, `EVENT_FRESH`): staleness is per observer, so under the app's 30 s
  // `staleTime` the view opened later would ask for it again.
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  // The address's own node, resolved against the whole tree and kept by the view
  // (`useAddressTarget`, as the outline reads it).
  const { named, reveal } = useAddressTarget(root, address);
  const revealed = reveal?.at(-1)?.id;
  const view = useMemo(
    () => presetView(root, "spec", address.filter, named),
    [address.filter, named, root]
  );
  // Whether the view draws a row of the node the address names, which the stage is told by the
  // rule for refused reads (`useTellsAddressDrawn`), from the outline's reads above. The body
  // draws a line of its own for the coverage, the spec's blocks, the results, the Verification
  // document's blocks, the decisions and the asks (`SpecFilterBody`). It draws a row for every
  // node the view keeps below the issue: each statement, and any other node the address names,
  // which the view keeps whatever the words filter leaves (`presetView`) and draws by its kind. So
  // where the view may tell, it says it draws the node where its view holds it, and that it draws
  // none where the tree holds no node the address names (`reveal` undefined).
  // It says so before the browser paints, so where the reads were held as the view was drawn, as
  // on Back to its address, no statement is painted in the outline's place.
  const documents = [primarySpec(issue), roleDocument(issue, "verification")];
  const own: readonly ReadKey[] = [
    "coverage",
    "results",
    "decisions",
    "asks",
    ...documents.flatMap((one) => (one === undefined ? [] : [blocksKey(one.id)])),
  ];
  const tells = useTellsAddressDrawn(failed, readsSettled, own, onAddressDrawn);
  useLayoutEffect(() => {
    if (!tells) return;
    const rows = nodesInOrder(view.root).slice(1);
    onAddressDrawn(rows.some(({ id }) => id === revealed));
  }, [onAddressDrawn, revealed, tells, view.root]);
  const request = useOutlineRequest();
  const { onEditing, panels } = useOutlineEditing(issue);
  return (
    <NoTooltips>
      {/* The view's measure, the prototype's `.spec`: at most 1080 px, in the middle of the
          column; on a phone 4 px from the left edge and 10 px from the right, as the outline. */}
      <div className="mx-auto flex w-full max-w-[1080px] min-w-0 flex-col max-[761px]:-mr-1 max-[761px]:-ml-2.5 max-[761px]:w-auto">
        <SpecFilterBody
          // The reader's acts since the address was followed, which the stage watches and the
          // body's landing reads: a reader who acted keeps the page as they left it.
          {...(acts === undefined ? {} : { addressActs: acts })}
          context={view.context}
          counts={counts}
          emptyLine={address.filter === "" ? EMPTY_STATE_LINES.spec : EMPTY_STATE_LINES.no_match}
          // The words filter's Escape comes before the view is left; turning the view off with
          // Escape is a later step of the page's key table.
          holdsEscape={address.filter !== ""}
          issue={issue}
          onEditing={onEditing}
          reader={viewer}
          {...(namesFailure === undefined ? {} : { namesFailure })}
          readsSettled={readsSettled}
          {...(request === undefined ? {} : { request })}
          {...(revealed === undefined ? {} : { reveal: revealed })}
          root={root}
          view={view.root}
        />
      </div>
      {panels}
    </NoTooltips>
  );
}
