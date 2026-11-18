/** The first-rollout run of e2e/cross-build.multiprocess.e2e.ts: Dispatch B is the first-rollout
 *  build, the newest release before the first that registers its schema numbers. CI's
 *  dispatch-two-builds job sets `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT` to that build's commit (its
 *  `FIRST_ROLLOUT_COMMIT`, which `firstRolloutRelease` in
 *  packages/envoy/cmd/dispatch/twobuild_harness_test.go holds too) for its second browser run.
 *  The variable holds the run to two things: B's `COMMIT` file is that commit, and the live
 *  registry reads a lowest schema version of 0 while B is live, since a process of a build before
 *  the fleet columns registers no numbers. A run whose B fails either one tests another build than
 *  the one it names, so `firstRolloutRefusal` refuses it. Nothing here runs on import except the
 *  read of the variable, so e2e/__tests__/first-rollout.test.ts drives it without a server. */

/** The commit `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT` holds Dispatch B to, or undefined when it is
 *  unset or empty: then the run is not the first-rollout run, and nothing here refuses it. */
export const firstRolloutCommit = process.env.DISPATCH_E2E_FIRST_ROLLOUT_COMMIT || undefined;

/** The numbers the live server processes register, as the spec's `liveFleet` reads them. */
export interface LiveFleet {
  minVersion: number;
  maxCreatable: number;
}

/** The sentence that refuses a first-rollout run, or undefined when the run may go on. `commit`
 *  is `firstRolloutCommit`, `commitB` the trimmed `COMMIT` file beside B's binary (undefined when
 *  B has none), and `fleet` the live registry's numbers while both servers serve. */
export function firstRolloutRefusal(
  commit: string | undefined,
  commitB: string | undefined,
  fleet: LiveFleet
): string | undefined {
  if (commit === undefined) {
    return undefined;
  }
  if (commitB === undefined) {
    return (
      `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT holds Dispatch B to the first-rollout build, commit ` +
      `${commit}. Dispatch B has no COMMIT file beside its binary, so this run cannot show that B ` +
      `is that build. Build B with packages/envoy/scripts/build-dispatch-build.sh --commit ` +
      `${commit} --out <dir>.`
    );
  }
  if (commitB !== commit) {
    return (
      `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT holds Dispatch B to the first-rollout build, commit ` +
      `${commit}. Dispatch B is a build of commit ${commitB}. Build B again with ` +
      `packages/envoy/scripts/build-dispatch-build.sh --commit ${commit} --out <dir>.`
    );
  }
  if (fleet.minVersion !== 0) {
    return (
      `The live registry reads a lowest schema version of ${fleet.minVersion} and a highest ` +
      `creatable number of ${fleet.maxCreatable} while Dispatch B, commit ${commit}, is live. A ` +
      `process of the first-rollout build registers no schema numbers, so the registry reads ` +
      `version 0 for it. This build registers its schema numbers, so it is not the first-rollout ` +
      `build.`
    );
  }
  return undefined;
}
