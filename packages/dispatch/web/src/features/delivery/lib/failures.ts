// The three failure measures of a measures answer, as the measures panel reads them from a server
// of either build a rolling release runs at once (packages/envoy/cmd/dispatch/README.md, "Two
// builds at once", "Response members": a page of this build works when the build before answers
// without a member it added). This build's server answers the change fail rate's own counts and
// rate, `failed_deployment_recovery` and `deployment_rework_rate`, and keeps, every leaf null, the
// members the previous build read: `change_failure_rate.per_pr` and `.per_deploy`, and
// `time_to_restore`. A server of that build answers those members alone, from the failed-change
// flags it counted, which measure nothing this build measures. So each measure here is read from
// this build's own members only, and is null, "no data", where the answer holds none of them: no
// panel shows a figure of the earlier build's as one of this build's. Pure.

import type {
  DeliveryChangeFailRate,
  DeliveryRecovery,
  DeliveryReworkRate,
} from "../../../api/types";

/** A change failure rate the window measured: the deploys with a counted failure, every deploy,
 *  and the first over the second. */
export interface MeasuredChangeFailRate {
  failed: number;
  total: number;
  rate: number;
}

/** The three failure measures of one measures answer, each null where it reads "no data". */
export interface FailureMeasures {
  changeFailRate: MeasuredChangeFailRate | null;
  recovery: DeliveryRecovery | null;
  rework: DeliveryReworkRate | null;
}

/** The members of a measures answer the failure measures are read from, as a server of either
 *  build writes them: this build writes each as `DeliveryMeasures` declares it. The previous build
 *  writes `change_failure_rate` with none of its own counts and rate, and no
 *  `failed_deployment_recovery` or `deployment_rework_rate`, which read here as absent. */
interface EitherBuildFailureMembers {
  change_failure_rate: Partial<Pick<DeliveryChangeFailRate, "failed" | "total" | "rate">> | null;
  failed_deployment_recovery?: DeliveryRecovery | null;
  deployment_rework_rate?: DeliveryReworkRate | null;
}

/** `measures`' three failure measures: the change fail rate where its counts and rate all hold a
 *  value, the recovery time and the rework rate where the answer holds them, and each null
 *  otherwise, whatever the members the earlier build read hold. */
export function readFailureMeasures(measures: EitherBuildFailureMembers): FailureMeasures {
  const rate = measures.change_failure_rate;
  return {
    changeFailRate:
      rate !== null &&
      typeof rate.failed === "number" &&
      typeof rate.total === "number" &&
      typeof rate.rate === "number"
        ? { failed: rate.failed, total: rate.total, rate: rate.rate }
        : null,
    recovery: measures.failed_deployment_recovery ?? null,
    rework: measures.deployment_rework_rate ?? null,
  };
}
