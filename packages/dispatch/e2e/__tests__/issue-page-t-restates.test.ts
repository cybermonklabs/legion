import { expect, test } from "bun:test";

import { fixtureSeed, type KeyedRow } from "../issue-page-t";
import { restatesAnAnswer } from "../issue-page-t-rows";

/** The row of `rows` named `key`, failing the test if the fixture does not hold it. */
function rowByKey(rows: readonly KeyedRow[], key: string): KeyedRow {
  const row = rows.find((one) => one.key === key);
  if (row === undefined) throw new Error(`the fixture holds no row ${key}`);
  return row;
}

// INFRA-1196's decision 521 narrows decision 513 on T's fixture rule (`restatesAnAnswer`): a
// decision row the fixture draws under an answered question restates the person's answer only
// where every node its shaping chips name is named as shaped on the question's row.
// `seedSpineCases`'s `decisionBlockId` carries a shaping chip naming `requirements.shapedElsewhere`
// while its answered question carries none, so it is an agent's decision and does not restate;
// `seedWholeIssue`'s `decisions[1]` carries no shaping chips under its answered question `/ask#1`
// and still restates.
test("restatesAnAnswer is false for seedSpineCases's decision row and true for seedWholeIssue's decisions[1]", () => {
  const spine = fixtureSeed("seedSpineCases").rows;
  const spineDecision = rowByKey(spine, "decisionBlockId");
  expect(restatesAnAnswer(spineDecision, spine)).toBe(false);

  const whole = fixtureSeed("seedWholeIssue").rows;
  const wholeDecision = rowByKey(whole, "decisions[1]");
  expect(restatesAnAnswer(wholeDecision, whole)).toBe(true);
});
