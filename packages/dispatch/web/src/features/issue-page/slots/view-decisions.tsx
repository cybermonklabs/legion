import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { whoAmIQuery } from "../../../api/queries";
import { useAddressTarget } from "../address-target";
import { DecisionsFilterBody } from "../decisions-filter/DecisionsFilterBody";
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { useOutlineReads } from "../outline-reads";
import { useOutlineRequest } from "../outline-request";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import { EVENT_FRESH } from "../useIssueTree";
import { waitingPressOf } from "../waiting";
import type { ViewSlotProps } from "./stage";

/** Filled by INFRA-1196 part Q, "The Decisions view". */
export function ViewDecisionsSlot({ issue, onAddressDrawn, route }: ViewSlotProps): ReactNode {
  // The Decisions view (part Q's R1): the issue's asks and decisions as one list in parts, drawn
  // from the issue's one tree as the Decisions view leaves it. The stage draws this slot only
  // where the address opens Decisions (`slots/stage.tsx`).
  const { hash, key: following, search, state } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  // The tree, and whether every read its nodes come from holds its answer (`useOutlineReads`),
  // each under the key and with the freshness the tree reads it by, so they cost no request of
  // their own.
  const { readsHeld, root } = useOutlineReads(issue);
  // Who is reading, whose own answers carry the ink rule and whose decisions stand under "Your
  // decisions": the shell's one read of who is signed in, so it costs no request of its own, and
  // kept fresh as the tree keeps it (`EVENT_FRESH`), so a press of Decisions reads it no second
  // time.
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the words filter would not have matched still draws it, but for a
  // person's write-up of an answered question (below); the entry it names takes the focus, once
  // per address (R6).
  const { named, reveal } = useAddressTarget(root, address);
  // Whether the list draws the node an address beside the view names, said to the stage once
  // for each following of the address, once every read the tree is built from holds its
  // answer (`ViewSlotProps.onAddressDrawn`): before that the node may not have arrived. The body
  // says whether it does (`onRevealDrawn`): only the body knows the rows it draws, and which
  // decisions are a person's write-up of an answered question, which is no row here, so that an
  // address naming one lands on the row of a question it answers where the list draws one. The
  // body is handed the call only while the address is beside the view and those reads are held,
  // and what it first says for a following is what the stage is told. Where the list draws none
  // of the node - the tree holds none, or the words filter matched none of the questions a
  // write-up answers - the stage draws the outline in the view's place, which lands on the node
  // or says that the issue holds none of it.
  const beside = address.target !== undefined && address.document === undefined;
  const told = useRef<string | undefined>(undefined);
  const tell = useCallback(
    (drawn: boolean) => {
      if (told.current === following) return;
      told.current = following;
      onAddressDrawn(drawn);
    },
    [following, onAddressDrawn]
  );
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
        {...(beside && readsHeld ? { onRevealDrawn: tell } : {})}
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
