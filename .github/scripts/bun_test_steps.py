"""Rule 8 of .github/scripts/check-workflow-files.sh: every bun test of ci.yaml's run runs under
.github/scripts/bun_test_run.py, and every job that runs one keeps its reports.

In ci.yaml and every workflow of this repository it reaches through its calls (the run a pull
request and the merge queue get), it refuses:

  - a line of a step's `run:` that runs `bun test`, or `bun run` of a script whose name starts with
    `test` (`test`, `test:web`, `test:e2e-lib`), other than as the command the wrapper runs: the
    wrapper's path (`.github/scripts/bun_test_run.py`, from any directory), a report name the
    wrapper takes (NAME, bun_test_record.py's), then that bun command. Such a run leaves no record
    bun_test_record.py's rule can read, so a run that wrote no report, or left a file out, is seen
    by nothing;
  - a job holding a step that runs bun test, under the wrapper or not, with no step after the
    last such step that uses UPLOAD_ACTION under the name UPLOAD_STEP, with `if: always()` and
    `check-run-id: ${{ job.check_run_id }}`. For a step whose wrapper .github/scripts/harness-pool.sh
    starts, the upload comes after the job's next step that runs `harness-pool.sh wait`, since the
    run writes its report when it ends in the pool. The reader finds a job's bun tests by that
    step's name, and the artifact by that id;
  - when a job runs bun test, an upload-artifact step of UPLOAD_ACTION's own action.yml whose
    `name` is not PREFIX followed by `${{ inputs.check-run-id }}`, whose `path` is not
    `${{ runner.temp }}/` followed by REPORTS_DIR and `/`, or that does not set
    `include-hidden-files: true`; or an action.yml with no such step, or none at all. The
    reader finds the artifact by PREFIX, and the wrapper writes under REPORTS_DIR, so the action's
    copy of the two is held to the record's.

A step's refusal names the file, job, step number and name, and line. An action's refusal names
its file and line, or only its file if it cannot be read. A line whose first non-space character
is `#` is a comment unless it follows a continuation. Other lines continued with `\\` are joined.
The wrapper behind quotes (`"$GITHUB_WORKSPACE/.github/scripts/bun_test_run.py"`) is not read as
the wrapper, so its bun test is refused as a bare one. These forms are not read, and pass unread:
bun's own options before `test` (`bun --cwd x test`); a bun given by its path
(`/usr/local/bin/bun test`, `$HOME/.bun/bin/bun test`); an option between `run` and the script
(`bun run --silent test`, `bun run --cwd x test`); a quoted script name (`bun run 'test'`);
`bun <script>` with no `run`; `bunx`; a script whose name does not start with `test`; bun words
from a variable or an array
(dispatch-ui's `bun run "${web[@]}"`, which runs under the wrapper); a bun test that a script or a
local action the step runs starts (bun_test_reports.test.sh's runs of its scratch files are its
own fixtures); and every workflow ci.yaml does not reach (dispatch-unit-repeat.yaml's leg). The
names it holds steps to are bun_test_record.py's. Its tests are in bun_test_steps.test.sh.
"""
import re
from dataclasses import dataclass
from pathlib import Path

import yaml
from bun_test_record import NAME, PREFIX, REPORTS_DIR, UPLOAD_ACTION, UPLOAD_ACTION_DIR, UPLOAD_STEP, WRAPPER
from workflow_paths import mapping_get, scalar

BUN_TEST = re.compile(r"(?<![\w./$-])bun\s+(?:test(?![\w:.-])|run\s+test[\w:.-]*)")
UNDER_WRAPPER = re.compile(re.escape(WRAPPER) + r"\s+" + NAME.pattern + r"\s+$")
WRAPPED = re.compile(r"(?:^|[\s;&|(])\S*" + re.escape(WRAPPER) + r"\s")
POOL_START = re.compile(r"harness-pool\.sh\s+start\s.*" + re.escape(WRAPPER))
POOL_WAIT = re.compile(r"harness-pool\.sh\s+wait\b")
CHECK_RUN_ID = "${{ job.check_run_id }}"
# What UPLOAD_ACTION's own upload-artifact step must be given, in the record's names.
ACTION_FILE = Path(UPLOAD_ACTION_DIR) / "action.yml"
ACTION_WITH = {
    "name": PREFIX + "${{ inputs.check-run-id }}",
    "path": "${{ runner.temp }}/" + REPORTS_DIR + "/",
    "include-hidden-files": "true",
}


@dataclass
class Census:
    commands: int = 0
    steps: int = 0
    jobs: int = 0


def logical_lines(text: str, first: int):
    """(line number, line) for each line of a `run:`, a line continued with `\\` joined to the
    next and numbered by its first; a comment line is left out."""
    held, start = "", None
    for offset, line in enumerate(text.split("\n")):
        if start is None:
            if line.lstrip().startswith("#"):
                continue
            start = first + offset
        if line.endswith("\\"):
            held += line[:-1] + " "
            continue
        line, held = held + line, ""
        if line.strip() and not line.lstrip().startswith("#"):
            yield start, line
        start = None
    if held.strip() and not held.lstrip().startswith("#"):
        yield start, held


def first_line(node: yaml.ScalarNode) -> int:
    """The file's line of a `run:` scalar's first line of text: a block scalar's text starts on
    the line after its `|`."""
    return node.start_mark.line + (2 if node.style in ("|", ">") else 1)


def check_bun_test_steps(path: Path, root: object, problem, census: Census) -> None:
    """Rule 8 over one workflow file, counting what it held into `census`."""
    jobs = mapping_get(root, "jobs")
    if not isinstance(jobs, yaml.MappingNode):
        return
    for job_name, job in jobs.value:
        steps = mapping_get(job, "steps")
        if not isinstance(steps, yaml.SequenceNode):
            continue
        job_id = scalar(job_name)
        # The index the job's upload must come after: each bun test step's own, or, for a run the
        # pool starts, its next wait's.
        # If two steps share a wait, use the later step in the diagnostic.
        anchors = []
        for index, step in enumerate(steps.value):
            run = mapping_get(step, "run")
            if not isinstance(run, yaml.ScalarNode):
                continue
            label = f"step {index + 1} ('{scalar(mapping_get(step, 'name')) or '(no name)'}')"
            where = f"{label} of job {job_id}"
            run_line = run.start_mark.line + 1
            found = pooled = False
            for line_number, line in logical_lines(run.value, first_line(run)):
                # Count each wrapper the line starts.
                wrapped = len(WRAPPED.findall(line))
                census.commands += wrapped
                found = found or wrapped > 0
                pooled = pooled or bool(POOL_START.search(line))
                for match in BUN_TEST.finditer(line):
                    found = True
                    if UNDER_WRAPPER.search(line[:match.start()]):
                        continue
                    problem(
                        path,
                        f"{path}:{line_number}: {where} runs `{match.group(0)}` other than under {WRAPPER}, so "
                        f"its report, if it writes one, has no marker or exit status beside it, and a run that "
                        f"wrote none or left a file out reads as complete. Run it as `{WRAPPER} <report name> "
                        f"{match.group(0)} …`, the path from the step's working directory (`../../` from a "
                        f"package's).",
                        line=line_number,
                    )
            if not found:
                continue
            census.steps += 1
            if not pooled:
                anchors.append((index, index, label, run_line))
                continue
            wait = next((later for later in range(index + 1, len(steps.value))
                         if POOL_WAIT.search(scalar(mapping_get(steps.value[later], "run")) or "")), None)
            if wait is None:
                problem(
                    path,
                    f"{path}:{run_line}: {where} starts its bun test in harness-pool.sh, and no later step of "
                    f"the job runs `harness-pool.sh wait`, so nothing waits for its report to be written before "
                    f"the job's upload.",
                    line=run_line,
                )
                wait = index
            anchors.append((wait, index, label, run_line))
        if not anchors:
            continue
        census.jobs += 1
        last, _, label, line = max(anchors)
        uploads = [(index, step) for index, step in enumerate(steps.value)
                   if scalar(mapping_get(step, "uses")) == UPLOAD_ACTION]
        after = [(index, step) for index, step in uploads if index > last]
        line_of_job = job_name.start_mark.line + 1
        if not after:
            problem(
                path,
                f"{path}:{line_of_job}: job {job_id} runs bun test (its last such run is {label}, line {line}) "
                f"and has no step after it that uses {UPLOAD_ACTION}"
                + (f" (its step {uploads[-1][0] + 1} does, before that run ends)" if uploads else "")
                + f", so the reports its runs write are not kept. Add, after it, `- name: {UPLOAD_STEP}`, "
                f"`if: always()`, `uses: {UPLOAD_ACTION}`, `with: check-run-id: {CHECK_RUN_ID}`.",
                line=line_of_job,
            )
            continue
        index, step = after[0]
        step_line = step.start_mark.line + 1
        wrong = []
        name, condition = scalar(mapping_get(step, "name")), scalar(mapping_get(step, "if"))
        check_run_id = scalar(mapping_get(mapping_get(step, "with"), "check-run-id"))
        if name != UPLOAD_STEP:
            wrong.append(f"is not named {UPLOAD_STEP!r}, the name .github/scripts/bun_test_reports.py "
                         "finds the job's records by")
        if condition != "always()":
            wrong.append(("has no `if:`" if condition is None else f"runs if {condition!r}") + ", not `if: always()`, "
                         "so a job whose test failed, or that was cancelled, would keep none of its reports")
        if check_run_id != CHECK_RUN_ID:
            wrong.append(f"gives check-run-id {check_run_id!r}, not `{CHECK_RUN_ID}`, the job id the artifact is "
                         "named for")
        label = f"step {index + 1} ({name or '(no name)'!r})"
        for fault in wrong:
            problem(path, f"{path}:{step_line}: {label} of job {job_id}, its upload of the bun test "
                          f"reports, {fault}.", line=step_line)


def check_upload_action(problem) -> None:
    """The third refusal of the docstring: ACTION_FILE's upload-artifact step uploads REPORTS_DIR
    under PREFIX, with hidden files."""
    try:
        root = yaml.compose(ACTION_FILE.read_text())
    except (OSError, UnicodeError) as error:
        problem(ACTION_FILE, f"{ACTION_FILE} could not be read ({' '.join(str(error).split())}), and every job that "
                             f"runs bun test uses it to keep its reports")
        return
    except yaml.YAMLError as error:
        mark = getattr(error, "problem_mark", None)
        line = mark.line + 1 if mark is not None else 1
        problem(ACTION_FILE, f"{ACTION_FILE}:{line}: its YAML does not parse ({' '.join(str(error).split())}), "
                             "and every job that runs bun test uses it to keep its reports", line=line)
        return
    runs = mapping_get(root, "runs")
    steps = mapping_get(runs, "steps")
    uploads = [step for step in (steps.value if isinstance(steps, yaml.SequenceNode) else [])
               if (scalar(mapping_get(step, "uses")) or "").startswith("actions/upload-artifact@")]
    if not uploads:
        node = steps or runs
        line = node.start_mark.line + 1 if node is not None else 1
        problem(ACTION_FILE, f"{ACTION_FILE}:{line}: it has no step that uses actions/upload-artifact, "
                             "so no job keeps its bun test reports", line=line)
        return
    for step in uploads:
        given = mapping_get(step, "with")
        for key, want in ACTION_WITH.items():
            node = mapping_get(given, key)
            got = scalar(node)
            if got != want:
                line = (node or step).start_mark.line + 1
                problem(ACTION_FILE, f"{ACTION_FILE}:{line}: its upload-artifact step gives {key} {got!r}, not "
                                     f"{want!r}, which bun_test_record.py's PREFIX and REPORTS_DIR make: the reader "
                                     f"finds the artifact by PREFIX, and the wrapper writes under REPORTS_DIR",
                        line=line)
