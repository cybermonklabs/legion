import { z } from "zod";
import { GO_TRIM_SPLIT } from "./go-blank";

/**
 * `fixtures/daemon-api/plan-rating-cases.json`, the daemon's verdict on each rating of a plan:
 * a rating and a reason a retro's completion carries, either left out where the case leaves it
 * out, and the daemon's verdict on it, `accepted` or its refusal code. internal/api's
 * TestPlanRatingCasesGolden writes the file. Both readers of it parse it with this schema: the
 * contract's request schema (`./legion-api.test.ts`) and the `legion` tool
 * (`packages/pi-shared/src/legion-phase-ops.test.ts`), so a case of another shape, a member of
 * another type or a member the schema does not name fails both, rather than one test reading it
 * through a cast. So does a golden that could not tell a reader from the daemon: one with no
 * accepted case or no refused one, which a reader that takes every case or none would pass, or one
 * that lacks a reason of either character alone on which JavaScript's trim and Go's
 * strings.TrimSpace differ (`./go-blank.ts`), which a reader that read blank by `trim` would pass.
 */
export const PlanRatingCases = z.strictObject({
  cases: z
    .array(
      z.strictObject({
        name: z.string().min(1),
        planRating: z.number().int().optional(),
        planReason: z.string().optional(),
        verdict: z.union([z.literal("accepted"), z.string().startsWith("PLAN_RATING_")]),
      })
    )
    .min(1)
    .refine((cases) => cases.some((c) => c.verdict === "accepted"), {
      error: "the cases hold no rating the daemon accepted",
    })
    .refine((cases) => cases.some((c) => c.verdict !== "accepted"), {
      error: "the cases hold no rating the daemon refused",
    })
    .refine((cases) => GO_TRIM_SPLIT.every((blank) => cases.some((c) => c.planReason === blank)), {
      error: "the cases hold no reason of one of GO_TRIM_SPLIT alone (./go-blank.ts)",
    }),
});
