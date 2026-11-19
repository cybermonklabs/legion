/** The order the visual review's rows run in: `normal`, the order `e2e/review-routes.ts` lists its
 *  pages in, or `reverse`, every row of the run the other way round - the phone project's rows
 *  before the desktop project's, and each project's rows last first. `DISPATCH_E2E_VISUAL_ORDER`
 *  names it, and `e2e/visual-run.sh` forwards that variable into the container with the other
 *  `DISPATCH_E2E_*` variables.
 *
 *  WHY A SECOND ORDER. One seed serves the whole visual run, so a page that writes state changes
 *  every page drawn after it, and a baseline then holds the state the rows before it left. That
 *  passes for as long as the rows run in the order their baselines were taken in, and fails as a
 *  row run alone or a row moved. A run in reverse puts every pair of rows the other way round, so a
 *  write one row makes and another reads fails one of the two runs, by the name of the row that
 *  read it. It does not show that a row passes alone: a state the first row writes and a middle row
 *  reads is there for the middle row in both orders. What closes that is the seed writing, before
 *  any row, each mark a page writes (`e2e/review-seed.ts`).
 *
 *  This module imports nothing, so the visual spec, which may reach nothing that spawns `psql`,
 *  can read it. */
export type VisualOrder = "normal" | "reverse";

/** The order `env` asks for. Unset is `normal`, and any other value fails the run by name rather
 *  than running the rows in an order nobody asked for. */
export function visualOrder(env: NodeJS.ProcessEnv = process.env): VisualOrder {
  const order = env.DISPATCH_E2E_VISUAL_ORDER;
  if (order === undefined || order === "" || order === "normal") return "normal";
  if (order === "reverse") return "reverse";
  throw new Error(
    `DISPATCH_E2E_VISUAL_ORDER is ${JSON.stringify(order)}; it must be unset, normal or reverse`
  );
}

/** `rows` in `order`: as they are for `normal`, last first for `reverse`. */
export function inVisualOrder<Row>(rows: readonly Row[], order: VisualOrder): readonly Row[] {
  return order === "reverse" ? [...rows].reverse() : rows;
}
