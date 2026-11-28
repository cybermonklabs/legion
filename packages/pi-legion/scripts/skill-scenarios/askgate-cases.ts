import { catalogLines } from "./catalog-file";

/** Every case of the askgate scenario, each line of askgate-cases.txt one, in its order: the one
 * catalog that rig.sh (which checks SKILL_SCENARIOS_ASKGATE_CASE against it, and whose run_askgate
 * has a prompt for each case) and score.ts read. */
export const ASKGATE_CASES = Object.freeze(
  catalogLines(new URL("./askgate-cases.txt", import.meta.url), 1, "<case>").map(
    ([which = ""]) => which
  )
);
