/**
 * Who may read the reserve of INFRA-2897's next final test (`reserve.ts`), committed by a reviewed
 * pull request before the read.
 *
 * The registered rule (dispatch://INFRA/artifact/infra-2897-final-test-reserve-md) lets one claim
 * run read the reserve (items 3 and 4), and ends the reserve at that round's draw or at the owner's
 * written release (item 6). A final test is read once, by the bundle the claim names: a second
 * arm, build or run that reads it makes it development data (dispatch://INFRA-3189, "Data sets").
 * So a run reads the reserve only when `APPROVAL_EVAL_RESERVE=read:<reference>` names a read listed
 * here and the run's digest (`runDigestOf` in `reserve.ts`) is the one listed with it. A refused
 * run prints its digest, which is what a registration copies.
 *
 * This file is outside the run digest, so a registration does not change the digest it registers.
 */

/** One registered read of the reserve: one command of the claim run, as its digest names it. */
export interface ReserveRead {
  /** The claim run's registration, written before the read: a dispatch:// reference. */
  reference: string;
  /** The run digest `runDigestOf` prints for that command, as 64 lowercase hexadecimal digits. */
  run: string;
}

/** Each registered read. None today: no run reads the reserve. */
export const RESERVE_READS: readonly ReserveRead[] = [];

/**
 * The record that ended the reserve, as a dispatch:// reference, or null while it holds: the claim
 * run's result, once the claim has read its final test, or the owner's written release. Item 6
 * ends the reserve at the draw, but the final test that draw takes is read by the claim run after
 * it, so the code holds every reserved case until this record exists. Once it does, no case is
 * held: a reserve that ended is development data.
 */
export const RESERVE_ENDED: string | null = null;
