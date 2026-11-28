"""Rule 26 of check-ci-filters.sh: a job of any workflow that runs the results action makes its go
test -json report with no -v but -json's own test2json, with the reader of a run: script's bash that
it reads each go test command with. ci_filter_coverage.main() runs it.
"""

import json
import re

from ci_filter_coverage import error, script_lines, step_label
from ci_workflow_rules import RACE_TEST, directory_workflows


# ------------------------------------------- the report the results step reads
# The action that reads a job's `go test -json` report and writes one result for each check whose
# test ran (packages/envoy/cmd/ci-report). go test -json runs the test binary with
# -test.v=test2json, whose frame lines carry a mark. A -v or -test.v of any other value, false too,
# replaces it, so the frame lines carry no mark: a test can print a line that reads as its own pass,
# and the report cannot be told from one of `go test -json` by its content. Before -args go test
# reads -v and -test.v; after -args or --args the test binary reads -test.v alone, and a -v there
# is a flag of the test's own, such as the log level that glog and klog register. A -v that GOFLAGS
# carries reaches no test binary: under -json, go drops v and test.v from the flags it forwards from
# GOFLAGS (cmd/go/internal/test/testflag.go, go1.27.3, the -json branch of testFlags), so only the
# command line can carry it. go test reads its flags after the package list too, so the rule reads
# a go test command to its end, past its redirections, and refuses a word whose value it cannot
# see.
RESULTS_ACTION = "./.github/actions/dispatch-results"
VERBOSE = re.compile(r"--?(test\.)?v(?:=(.*))?")
JSON = re.compile(r"--?json(?:=(?:1|t|T|TRUE|true|True))?")
TEST_BINARY_ARGUMENTS = {"-args", "--args"}
# The characters bash reads as operators outside quotes, which shell_tokens joins into one token
# when they stand together, as `|&`, `&&`, `2>&1` and `<(`.
OPERATOR_CHARACTERS = set("();<>|&")
# The operators that end a simple command: a pipe and the lists of bash. A ")" ends the command of
# the subshell or the substitution it closes.
COMMAND_ENDS = {"|", "|&", "||", "&&", ";", "&"}
# A redirection, with the descriptor before it: its next word is its target, and neither is an
# argument of the command.
REDIRECTION = re.compile(r"\d*(?:<|>|>>|>\||<>|<<|<<<|<&|>&|&>|&>>)")


def verbose_flags(arguments: list) -> list:
    """The arguments of a `go test` command that take the marks off its report's frame lines: a -v
    or -test.v of go test before -args, and a -test.v of the test binary after it, with one dash or
    two, bare or with any value but go's own test2json."""
    found, to_test_binary = [], False
    for argument in arguments:
        if argument in TEST_BINARY_ARGUMENTS:
            to_test_binary = True
            continue
        flag = VERBOSE.fullmatch(argument)
        if flag and (flag[1] or not to_test_binary) and flag[2] != "test2json":
            found.append(argument)
    return found


def makes_a_report(arguments: list) -> bool:
    """Whether a `go test` command's arguments ask for its -json report: a -json of go test, before
    -args, bare or true."""
    for argument in arguments:
        if argument in TEST_BINARY_ARGUMENTS:
            return False
        if JSON.fullmatch(argument):
            return True
    return False


def shell_tokens(line: str):
    """The tokens of one line of bash, as (text, operator, expands): a word with its quotes taken
    off, or a run of operator characters standing together outside quotes, with the digits of a
    descriptor that stand right before a < or > joined to it, and the "$(" that opens a command
    substitution as an operator of its own. A word expands when bash substitutes any of it: a $ or
    a backtick outside single quotes, whatever follows it, the ${{ }} of a GitHub expression among
    them, since GitHub puts its value in the text first. A # that starts a word ends the line. None
    for a line whose quote does not close, which no reading of it can split."""
    tokens, word, expands, in_word = [], [], False, False
    i = 0

    def end_word():
        nonlocal word, expands, in_word
        if in_word:
            tokens.append(("".join(word), False, expands))
        word, expands, in_word = [], False, False

    while i < len(line):
        c = line[i]
        if c in " \t":
            end_word()
        elif c == "#" and not in_word:
            break
        elif c in OPERATOR_CHARACTERS:
            descriptor = "".join(word) if in_word and not expands and "".join(word).isdigit() and c in "<>" else ""
            if descriptor:
                word, in_word = [], False
            end_word()
            j = i
            while j < len(line) and line[j] in OPERATOR_CHARACTERS:
                j += 1
            tokens.append((descriptor + line[i:j], True, False))
            i = j
            continue
        elif c == "'":
            close = line.find("'", i + 1)
            if close < 0:
                return None
            word.append(line[i + 1:close])
            in_word, i = True, close + 1
            continue
        elif c == '"':
            i += 1
            while i < len(line) and line[i] != '"':
                if line[i] == "\\" and i + 1 < len(line) and line[i + 1] in '$`"\\':
                    word.append(line[i + 1])
                    i += 2
                    continue
                if line[i] in "$`":
                    expands = True
                word.append(line[i])
                i += 1
            if i >= len(line):
                return None
            in_word = True
        elif c == "\\":
            if i + 1 < len(line):
                word.append(line[i + 1])
            in_word = True
            i += 2
            continue
        elif c == "$" and line[i + 1:i + 2] == "(":
            # A command substitution, or arithmetic: the opener is an operator of its own, so the
            # check names it as bash reads it.
            end_word()
            tokens.append(("$(", True, True))
            i += 2
            continue
        else:
            if c in "$`":
                expands = True
            word.append(c)
            in_word = True
        i += 1
    end_word()
    return tokens


def go_test_commands(script: str) -> list:
    """Each `go test` command of a run: script, as (its line, its arguments up to the end of the
    command, what this check could not read), with a line a backslash continues read as one line
    and comments aside. The command ends at a pipe, a list operator or the ")" that closes it; a
    redirection and its target are no arguments of it. What it could not read is None for a
    command whose every argument it read; the word or the operator it stopped at, for one that
    holds a word bash expands ($, a backtick, a GitHub expression) or an operator that opens a
    subshell or a substitution, since any of them can carry a -v past this check; and "" for a
    line that holds `go test` (RACE_TEST's spelling of one, -race or not) and that shell_tokens
    cannot split."""
    commands = []
    for line in script_lines(script.replace("\\\n", " ")):
        tokens = shell_tokens(line)
        if tokens is None:
            if RACE_TEST.search(line):
                commands.append((line, [], ""))
            continue
        for i, (text, operator, expands) in enumerate(tokens):
            if operator or expands or text.rsplit("/", 1)[-1] != "go":
                continue
            rest = tokens[i + 3:] if tokens[i + 1:i + 2] == [("-C", False, False)] else tokens[i + 1:]
            if rest[:1] != [("test", False, False)]:
                continue
            arguments, unread, after = [], None, iter(rest[1:])
            for text, operator, expands in after:
                if operator and (text in COMMAND_ENDS or text.startswith(")")):
                    break
                if operator and REDIRECTION.fullmatch(text):
                    next(after, None)
                    continue
                if operator or expands:
                    unread = text
                    break
                arguments.append(text)
            commands.append((line, arguments, unread))
    return commands


def check_report_steps(problems: list) -> list:
    """A job of any workflow that runs RESULTS_ACTION makes its report with no -v but -json's own
    test2json. Each `go test` command in a run: script of the job is held to no flag that
    verbose_flags names (a -v or -test.v of go test before -args, and a -test.v of the test binary
    after it, one dash or two, bare or with any value but test2json), since the report the action
    reads is one of them; a command whose flags it cannot read to the command's end - a line it
    cannot split, a word bash or GitHub expands, a subshell or a substitution - is refused, since
    any of them can carry such a flag. A job with no `go test -json` command where this rule reads
    it is refused too. The rule does not tie a command to the report path the action is given: a
    job that makes its report in a script or another job's artifact, and holds an unrelated
    `go test -json` of its own, passes, so the report is made in a run: step of the job. Returns
    its one report line, which says how many jobs and commands it read, so a run that read none
    says so."""
    jobs = commands = 0
    for path, document in directory_workflows().items():
        for job_id, job in ((document or {}).get("jobs") or {}).items():
            steps = [s for s in (job or {}).get("steps") or [] if isinstance(s, dict)]
            if not any(str(s.get("uses", "")).split("@")[0].rstrip("/") == RESULTS_ACTION for s in steps):
                continue
            jobs += 1
            reports = False
            for step in steps:
                for line, arguments, unread in go_test_commands(str(step.get("run", ""))):
                    commands += 1
                    where = f"{path} job {job_id} step \"{step_label(step)}\" runs `{line}`"
                    if unread == "":
                        reports = True
                        problems.append(error(path, f"{where}, which this check cannot split into words, so it "
                                                    f"cannot read whether its go test passes -v: the job runs "
                                                    f"{RESULTS_ACTION}, which reads the job's go test -json report"))
                        continue
                    reports = reports or makes_a_report(arguments)
                    if unread is not None:
                        reports = True
                        problems.append(error(path, f"{where}, whose go test holds {json.dumps(unread)}, which this check "
                                                    f"cannot read past: a word that bash or GitHub expands, a "
                                                    f"subshell or a substitution can carry a -v, and the job runs "
                                                    f"{RESULTS_ACTION}, which reads the job's go test -json "
                                                    f"report. Write each flag of the go test command as a word of "
                                                    f"the line"))
                        continue
                    verbose = verbose_flags(arguments)
                    if verbose:
                        problems.append(error(path, f"{where}, which passes {', '.join(f'`{a}`' for a in verbose)}: the "
                                                    f"job runs {RESULTS_ACTION}, which reads the job's go test -json "
                                                    f"report, and a -v or -test.v of any value but test2json, false "
                                                    f"too, replaces the -test.v=test2json that -json passes the test "
                                                    f"binary, so its frame lines carry no mark, a test can print a "
                                                    f"line that reads as its own pass and the report cannot be told "
                                                    f"from one made without -v. Remove the flag"))
            if not reports:
                problems.append(error(path, f"{path} job {job_id} runs {RESULTS_ACTION}, and no run: step of it runs "
                                            f"`go test -json` where this check reads it: the report the action reads "
                                            f"is held to no -v but -json's own test2json, so the go test -json "
                                            f"command that makes it is a line of a run: step of the same job"))
    return [f"{RESULTS_ACTION}: run by {jobs} job(s), which hold {commands} go test command(s)"]
