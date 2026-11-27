import { useState } from "react";

/** What `useOneCreateOut` answers a create's form. */
export interface OneCreateOut {
  /** Whether a press of the create now sends nothing, because the create's newest press is still
   *  out. A press it refuses is noted, so `refused` holds from that press until the newest press
   *  is decided. */
  readonly refuse: () => boolean;
  /** Whether the reader pressed the create while its newest press is still out: the form says so
   *  beside the control it was pressed from, in its own words, until that press lands or fails. */
  readonly refused: boolean;
}

/**
 * The one press of a create that is out at a time. A form holds its create's control while the
 * create is out within its deadline, and lets it go past it (`KeyedWriteResult.late`), so a create
 * that never answers holds nothing for good; but a second press then would make a second issue,
 * token or comment while the first is still tried under its key. So a press while the create's
 * newest press is out sends nothing (`refuse`), and the form says so beside the control
 * (`refused`): that the first is still being sent, and what the reader sees once it lands or
 * fails. Once that press fails, the control takes a press again, which sends a create of its own;
 * once it lands, the form opens what it made, or empties, as its landing says.
 */
export function useOneCreateOut(create: {
  readonly isPending: boolean;
  readonly submittedAt: number;
}): OneCreateOut {
  // The press out when a press was refused, by the time it was made (`submittedAt`): the note
  // goes once that press is decided, and a later press starts with none.
  const [refusedOver, setRefusedOver] = useState<number>();
  return {
    refuse: () => {
      if (!create.isPending) return false;
      setRefusedOver(create.submittedAt);
      return true;
    },
    refused: create.isPending && refusedOver === create.submittedAt,
  };
}
