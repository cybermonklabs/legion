"""Rule 3 of check-go-offline.sh: in every job of a required workflow, each step that runs
go-fetch.sh comes after a step of the same job that restores ~/go/pkg/mod, so the fetch tops up the
job's module cache instead of downloading it whole.

Imported with PYTHONPATH naming the directory this file lives in. Its test is
check-go-offline.test.sh, with the cases it sources.
"""
from pathlib import Path

import yaml

from source_shell import shell_script


def runs_fetch(run):
    """Whether a step's run text runs go-fetch.sh deps, tools or file (a file of deps and tools lines)."""
    for command in shell_script(run):
        values = command.values()
        if any(v.endswith("go-fetch.sh") and values[k + 1:k + 2] in (["deps"], ["tools"], ["file"])
               for k, v in enumerate(values)):
            return True
    return False


def check(workflows, errors):
    """Rule 3 over each workflow of workflows (path -> text): each fetch step with no restore of
    ~/go/pkg/mod before it in its job adds an error. The line naming each other fetch step and the
    restore before it, in order."""
    pairs = []
    for workflow_path in sorted(workflows):
        document = yaml.safe_load(Path(workflow_path).read_text()) or {}
        for job_id, job in (document.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            restore = None
            for index, step in enumerate(job.get("steps") or []):
                uses = str(step.get("uses") or "")
                if uses.startswith(("actions/cache/restore@", "actions/cache@")) \
                        and "~/go/pkg/mod" in str((step.get("with") or {}).get("path") or "").split():
                    restore = step.get("name") or step.get("id") or f"step {index + 1}, {uses.split('@')[0]}"
                run = step.get("run")
                if not (isinstance(run, str) and "go-fetch.sh" in run and runs_fetch(run)):
                    continue
                name = step.get("name") or run.strip().splitlines()[0]
                if restore is None:
                    errors.append((workflow_path, 1, f"job {job_id}, step \"{name}\", runs go-fetch.sh with no step "
                                   "before it that restores ~/go/pkg/mod (actions/cache/restore or actions/cache): "
                                   "restore the job's module cache first, so the fetch adds to it instead of "
                                   "downloading every module"))
                else:
                    pairs.append(f"  {workflow_path} job {job_id}: \"{name}\" after \"{restore}\"")
    return pairs
