import { type ReactNode, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { useAddressTarget } from "../address-target";
import { DecisionsFilterBody } from "../decisions-filter/DecisionsFilterBody";
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { useOutlineReads } from "../outline-reads";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import { useWhoIsReading } from "../useIssueTree";
import { waitingPressOf } from "../waiting";
import { useTellsAddressDrawn } from "./refused-reads";
import type { ViewSlotProps } from "./stage";

/** Filled by INFRA-1196 part Q, "The Decisions view". */
export function ViewDecisionsSlot({ issue, onAddressDrawn, route }: ViewSlotProps): ReactNode {
  // The Decisions view (part Q's R1): the issue's asks and decisions as one list in parts, drawn
  // from the issue's one tree as the Decisions view leaves it. The stage draws this slot only
  // where the address opens Decisions (`slots/stage.tsx`).
  const { hash, search, state } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  // The tree, the line of each read it is built from that stands refused, and whether every one
  // of those reads has settled (`useOutlineReads`), each under the key and with the freshness the
  // tree reads it by, so they cost no request of their own.
  const { failed, readsSettled, root } = useOutlineReads(issue);
  // Who is reading, whose own answers carry the ink rule and whose decisions stand under "Your
  // decisions": the shell's one read of who is signed in, so it costs no request of its own, and
  // kept fresh as the tree keeps it (`EVENT_FRESH`), so a press of Decisions reads it no second
  // time.
  const viewer = useWhoIsReading().data?.login;
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the words filter would not have matched still draws it, but for a
  // person's write-up of an answered question (below); the entry it names takes the focus, once
  // per address (R6).
  const { named, reveal } = useAddressTarget(root, address);
  // Whether the list draws the node the address names, which the stage is told by the rule for
  // refused reads (`useTellsAddressDrawn`). The list draws a line of its own for the decisions
  // read and the asks read (`DecisionsFilterBody`). Where the view may tell, the body says
  // whether it draws the node (`onRevealDrawn`): only the body knows the rows it draws, and which
  // decisions are a person's write-up of an answered question, which is no row here, so that an
  // address naming one lands on the row of a question it answers where the list draws one. Where
  // the list draws none of the node - the tree holds none, the words filter matched none of the
  // questions a write-up answers, or a read the list draws no line for stands refused - the stage
  // draws the outline in the view's place, which lands on the node or says that the issue holds
  // none of it or that its read failed.
  const tells = useTellsAddressDrawn(failed, readsSettled, ["decisions", "asks"], onAddressDrawn);
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
        emptyLine={
          address.filter.trim() === "" ? EMPTY_STATE_LINES.decisions : EMPTY_STATE_LINES.no_match
        }
        // The view's Escape, which turns it off, comes before the list is left (part N).
        holdsEscape
        isClosed={issue.closed_at !== null}
        issue={issue}
        {...(tells ? { onRevealDrawn: onAddressDrawn } : {})}
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
