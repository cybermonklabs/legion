/**
 * Who may read the reserve of INFRA-2897's next final test (`reserve.ts`), committed by a reviewed
 * pull request before the read.
 *
 * The registered rule (dispatch://INFRA/artifact/infra-2897-final-test-reserve-md, version 2) lets
 * one claim run read the reserve (items 5 and 6), and ends the reserve at that run's recorded
 * result or at the owner's written release (item 7). A final test is read once, by the bundle the
 * claim names: a second arm, build or run that reads it makes it development data
 * (dispatch://INFRA-3189, "Data sets").
 * So a run reads the reserve only when `APPROVAL_EVAL_RESERVE=read:<reference>` names a read listed
 * here and the run's digest (`runDigestOf` in `reserve.ts`) is the one listed with it. A refused
 * run prints its digest, which is what a registration copies. A digest admits one run, not every
 * run made with it: the reference is the claim's registration comment in Dispatch, the claim run
 * starts with a reply to it whose body starts `approval-eval claim run starts`, and a run reads
 * only when `APPROVAL_EVAL_CLAIM_RUN` names the first such reply's id (`claimRunOf` in
 * `reserve.ts`). Every command of the claim run, a resumed one included, names that same reply; a
 * later start reply is a second claim run and is refused.
 *
 * The claim run's build (`build-corpus.ts`) is one of its registered commands: a build that holds
 * the reserve writes none of its cases to `corpus.jsonl`, and `readCorpus` lets a read of the
 * reserve take only a corpus whose build wrote it under that same read, so the claim's draw reads
 * the corpus its own registered build wrote, never an older or another run's.
 *
 * This file is outside the run digest, so a registration does not change the digest it registers.
 */

/** One registered read of the reserve: one command of the claim run, as its digest names it. */
export interface ReserveRead {
  /**
   * The claim run's registration, written before the read: a Dispatch comment, as
   * `dispatch://<item>/comment/<id>`, whose first start reply starts the claim run.
   */
  reference: string;
  /** The run digest `runDigestOf` prints for that command, as 64 lowercase hexadecimal digits. */
  run: string;
}

/** Each registered read. None today: no run reads the reserve. */
export const RESERVE_READS: readonly ReserveRead[] = [];

/**
 * The record that ended the reserve, as a dispatch:// reference, or null while it holds: the claim
 * run's result, once the claim has read its final test, or the owner's written release. Once it is
 * set no case is held, the drawn final test included: every reserved case is development data and
 * any run reads it. Item 7 ends the reserve at the claim's recorded result, not at the draw: the
 * final test the draw takes is still read once, by the claim run after it, so this record is never
 * set at the draw.
 */
export const RESERVE_ENDED: string | null = null;
