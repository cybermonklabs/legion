// One project's delivery settings as `GET /api/v1/projects/{key}/settings/delivery` answers them
// (dispatch://INFRA-978), for the tests of the pages that read them to decide what they show: the
// Evals tab's delivery and CI sections, and `/delivery`'s default project.
import type { ProjectDeliverySettings } from "../api/types";

/** `project`'s settings with delivery on and one GitHub workflow source in acme/deploys, which
 *  has reconciled, as `overrides` changes them. */
export function deliverySettings(
  project: string,
  overrides: Partial<ProjectDeliverySettings> = {}
): ProjectDeliverySettings {
  return {
    code_groups: [],
    default_repo: "acme/widgets",
    delivery: true,
    excluded_repos: [],
    failure_sources: [],
    failures_measured_since: null,
    population_authors: [],
    project,
    sources: [
      {
        deploy_workflow_path: ".github/workflows/deploy.yml",
        id: `${project}-source-1`,
        kind: "github_workflow",
        last_error: null,
        last_event_at: "2026-11-23T08:00:00Z",
        last_reconcile_at: "2026-11-23T08:00:00Z",
        pr_checks_rollup_job_name: null,
        pr_checks_workflow_path: ".github/workflows/pr.yml",
        production_apply_step_name: null,
        production_job_names: ["deploy-production"],
        reconciled_through: "2026-11-23T08:00:00Z",
        repo: "acme/deploys",
        resolved_repo: "acme/deploys",
        staging_apply_job_name: null,
        staging_e2e_job_name: null,
      },
    ],
    targets: {},
    updated_at: "2026-11-01T00:00:00Z",
    updated_by: { id: "alice", kind: "user" },
    ...overrides,
  };
}
