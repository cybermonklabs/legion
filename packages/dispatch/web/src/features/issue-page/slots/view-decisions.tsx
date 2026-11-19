import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { whoAmIQuery } from "../../../api/queries";
import { useAddressTarget } from "../address-target";
import { DecisionsFilterBody } from "../decisions-filter/DecisionsFilterBody";
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { useIssueTree } from "../useIssueTree";
import { waitingPressOf } from "../waiting";

/** Filled by INFRA-1196 part Q, "The Decisions view". */
export function ViewDecisionsSlot({ issue, route }: SlotProps): ReactNode {
  // The Decisions view (part Q's R1): the issue's asks and decisions as one list in parts, drawn
  // from the issue's one tree as the Decisions view leaves it. The stage draws this slot only
  // where the address opens Decisions (`slots/stage.tsx`).
  const { hash, search, state } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  const { root } = useIssueTree(issue);
  // Who is reading, whose own answers carry the ink rule and whose decisions stand under "Your
  // decisions": the shell's one read of who is signed in, so it costs no request of its own.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the words filter would not have matched still draws it; the
  // entry it names takes the focus, once per address (R6).
  const { named, reveal } = useAddressTarget(root, address);
  const view = useMemo(
    () => presetView(root, "decisions", address.filter, named),
    [address.filter, named, root]
  );
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  // What the Contents list asks of the outline, which the view takes as the outline does.
  const request = useOutlineRequest();
  return (
    <NoTooltips>
      <DecisionsFilterBody
        artifacts={artifacts}
        emptyLine={address.filter === "" ? EMPTY_STATE_LINES.decisions : EMPTY_STATE_LINES.no_match}
        // The view's Escape, which turns it off, comes before the list is left (part N).
        holdsEscape
        isClosed={issue.closed_at !== null}
        issue={issue}
        reader={viewer}
        request={request}
        reveal={reveal?.at(-1)?.id}
        root={root}
        view={view.root}
        // The press of the header's count of what waits on the reader, left in the history
        // entry (part P): each press goes to the next thing that waits (the tester's F12).
        waitingPress={waitingPressOf(state)}
      />
    </NoTooltips>
  );
}
