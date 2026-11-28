/** Which artifact `e2e/tools/shard-weights.ts` reads for a run `--run` names, or why it refuses
 *  the run (`namedArtifactOf`), over GitHub's answers captured on 2026-11-27 under
 *  `fixtures/shard-weights-run/`: the `dispatch-browser-report-1` artifacts of full-lane run
 *  46005406939 and of pull-request-lane run 46048358172, and run 46005406939's own answer. */
import { expect, mock, test } from "bun:test";

import { namedArtifactOf } from "../tools/shard-weights";
import runAnswer from "./fixtures/shard-weights-run/run-46005406939.json";
import activeAnswer from "./fixtures/shard-weights-run/run-46005406939-artifacts.json";
import emptyAnswer from "./fixtures/shard-weights-run/run-46048358172-artifacts.json";

const [activeArtifact] = activeAnswer.artifacts;

test("an artifact that names its workflow run is read at that run's commit, without the run's own answer", () => {
  const runAnswerOf = mock(() => runAnswer);
  expect(namedArtifactOf(46005406939, activeAnswer, runAnswerOf)).toEqual({
    commit: "480dd5d418bb723d963c83e62504b328cfb96d18",
    id: 15201889647,
    run: 46005406939,
  });
  expect(runAnswerOf).not.toHaveBeenCalled();
});

test("a run whose answer lists no artifact, as a pull-request-lane run's, is refused", () => {
  expect(namedArtifactOf(46048358172, emptyAnswer, () => runAnswer)).toEqual({
    refusal: expect.stringContaining("has no dispatch-browser-report-1 artifact"),
  });
});

test("an artifact whose workflow_run is null is read at the commit of the run's own answer", () => {
  // The first capture with one field changed: when inspected, live GitHub Actions held neither an
  // artifact whose `workflow_run` is null nor an expired dispatch-browser-report-1 artifact.
  const answer = { ...activeAnswer, artifacts: [{ ...activeArtifact, workflow_run: null }] };
  const runAnswerOf = mock(() => runAnswer);
  expect(namedArtifactOf(46005406939, answer, runAnswerOf)).toEqual({
    commit: runAnswer.head_sha,
    id: 15201889647,
    run: 46005406939,
  });
  expect(runAnswerOf).toHaveBeenCalledTimes(1);
});

test("a run whose every listed artifact has expired is refused", () => {
  // The first capture with one field changed: when inspected, live GitHub Actions held neither an
  // artifact whose `workflow_run` is null nor an expired dispatch-browser-report-1 artifact.
  const answer = { ...activeAnswer, artifacts: [{ ...activeArtifact, expired: true }] };
  expect(namedArtifactOf(46005406939, answer, () => runAnswer)).toEqual({
    refusal: expect.stringContaining("has expired"),
  });
});
