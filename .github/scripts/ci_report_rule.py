"""Rule 26 of check-ci-filters.sh: a job of any workflow that runs the results action makes its go
test -json report with no -v but -json's own test2json, with the reader of a run: script's bash that
it reads each go test command with. ci_filter_coverage.main() runs it over the workflows of the
directory, as ci_workflow_rules.directory_workflows read them once for every rule that walks it.
"""

import json
import re

from ci_filter_coverage import error, script_lines, step_label
from ci_workflow_rules import RACE_TEST


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
# see. One such word passes: a word that bash fills in only inside double quotes, which it neither
# splits nor matches to file names, as the value of a flag that takes one (-run "$pattern",
# -run="$pattern"). go test gives that flag the word as its value whatever it holds, so the word is
# never a flag of go test, a -v or -args among them.
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
# A flag as go reads one (cmdflag.ParseOne, cmd/go/internal/cmdflag/flag.go, go1.27.3): one dash or
# two, a name that starts with neither, and the value an = joins to it.
FLAG = re.compile(r"--?([^-=][^=]*)(=.*)?", re.S)
# The flags of go test that take a value: every flag of its flag set but the booleans
# (cmd/go/internal/test/testflag.go's init, and the AddBuildFlags and AddCoverFlags it calls,
# cmd/go/internal/work/build.go, go1.27.3). cmdflag.ParseOne gives such a flag the next word as its
# value whatever that word holds. -C reads its next word too: go takes `-C dir` off the command
# line when it is the first flag after test, and fails on it anywhere else.
GO_TEST_VALUE_FLAGS = {
    "asmflags", "bench", "benchtime", "blockprofile", "blockprofilerate", "buildmode", "C", "compiler",
    "count", "covermode", "coverpkg", "coverprofile", "cpu", "cpuprofile", "debug-actiongraph",
    "debug-runtime-trace", "debug-trace", "exec", "fuzz", "fuzzminimizetime", "fuzztime", "gccgoflags",
    "gcflags", "installsuffix", "ldflags", "list", "memprofile", "memprofilerate", "mod", "modfile",
    "mutexprofile", "mutexprofilefraction", "o", "outputdir", "overlay", "p", "parallel", "pgo", "pkgdir",
    "run", "shuffle", "skip", "tags", "timeout", "toolexec", "trace", "vet",
}
# Those that go test reads under a test. prefix too (passFlagToTest,
# cmd/go/internal/test/flagdefs.go, go1.27.3).
TEST_PREFIX_VALUE_FLAGS = {
    "bench", "benchtime", "blockprofile", "blockprofilerate", "count", "coverprofile", "cpu", "cpuprofile",
    "fuzz", "fuzzminimizetime", "fuzztime", "list", "memprofile", "memprofilerate", "mutexprofile",
    "mutexprofilefraction", "outputdir", "parallel", "run", "shuffle", "skip", "timeout", "trace",
}


def takes_a_value(argument: str):
    """How a word that go test reads as one of its flags that take a value holds that value:
    "joined" for one that holds it after an =, and "next" for one whose value is the word after
    it. None for any other word."""
    flag = FLAG.fullmatch(argument)
    if not flag:
        return None
    name = flag[1]
    if name not in GO_TEST_VALUE_FLAGS and not (name.startswith("test.") and name[5:] in TEST_PREFIX_VALUE_FLAGS):
        return None
    return "joined" if flag[2] is not None else "next"


def go_test_reading(arguments: list) -> list:
    """What go test makes of each argument of a go test command, as testFlags reads them through
    cmdflag.ParseOne (cmd/go/internal/test/testflag.go, go1.27.3): "value" for the word after a
    flag that takes a value and holds none, whatever that word holds, a -v or -args among them;
    "test binary" for -args or --args and each word after it; and "go test" for each other word, a
    flag of go test or a package."""
    roles, reading, value_next = [], "go test", False
    for argument in arguments:
        if value_next:
            roles.append("value")
            value_next = False
            continue
        if reading == "go test" and argument in TEST_BINARY_ARGUMENTS:
            reading = "test binary"
        elif reading == "go test":
            value_next = takes_a_value(argument) == "next"
        roles.append(reading)
    return roles


def verbose_flags(arguments: list) -> list:
    """The arguments of a `go test` command that take the marks off its report's frame lines: a -v
    or -test.v of go test before -args, and a -test.v of the test binary after it, with one dash or
    two, bare or with any value but go's own test2json. The value of a flag that takes one is no
    flag."""
    found = []
    for argument, role in zip(arguments, go_test_reading(arguments)):
        flag = VERBOSE.fullmatch(argument)
        if flag and role != "value" and (flag[1] or role == "go test") and flag[2] != "test2json":
            found.append(argument)
    return found


def makes_a_report(arguments: list) -> bool:
    """Whether a `go test` command's arguments ask for its -json report: a -json of go test, before
    -args, bare or true, and not the value of a flag that takes one."""
    return any(role == "go test" and JSON.fullmatch(argument)
               for argument, role in zip(arguments, go_test_reading(arguments)))


# The text after a $ that bash fills in as one word inside double quotes: a name, a positional or
# special parameter but @, and a ${ } that holds no quote, $, backtick, backslash, @ or brace,
# since "$@", "${a[@]}" and "${!p@}" become several words and a quote or a $ inside ${ } is read
# by rules this reader does not follow.
ONE_WORD_EXPANSION = re.compile(r"\$(?:[A-Za-z_][A-Za-z0-9_]*|[0-9*#?$!-]|\{[^{}\"'`$\\@]*\})")
# The character after a $ that makes bash expand it (a name, a parameter, ${, $( or $[); any other
# $ is a literal one, as the $ of `-run "^TestA$"`. Outside quotes, a $' or $" expands too.
DOLLAR_EXPANDS = re.compile(r"[A-Za-z0-9_{(\[@*#?$!-]")


def shell_tokens(line: str):
    """The tokens of one line of bash, as (text, operator, expands, splits): a word with its quotes
    taken off, or a run of operator characters standing together outside quotes, with the digits
    of a descriptor that stand right before a < or > joined to it, and the "$(" that opens a
    command substitution as an operator of its own. A word expands when bash fills in any of it: a
    $ that DOLLAR_EXPANDS follows or a backtick, outside single quotes, the ${{ }} of a GitHub
    expression among them, since GitHub puts its value in the text first. A $ followed by anything
    else is a literal one. A word splits when bash or GitHub can make it more words than one, or
    words this reader cannot see: an expansion outside double quotes, which bash splits and matches
    to file names; a GitHub expression anywhere, whose value can close a quote; and inside double
    quotes, every expansion but those of ONE_WORD_EXPANSION. A # that starts a word ends the line.
    None for a line whose quote does not close, which no reading of it can split."""
    tokens, word, expands, splits, in_word = [], [], False, False, False
    i = 0

    def end_word():
        nonlocal word, expands, splits, in_word
        if in_word:
            tokens.append(("".join(word), False, expands, splits))
        word, expands, splits, in_word = [], False, False, False

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
            tokens.append((descriptor + line[i:j], True, False, False))
            i = j
            continue
        elif c == "'":
            close = line.find("'", i + 1)
            if close < 0:
                return None
            word.append(line[i + 1:close])
            # GitHub puts an expression's value in the text before bash reads it, so inside single
            # quotes too, and the value can close the quote.
            if "${{" in line[i + 1:close]:
                expands = splits = True
            in_word, i = True, close + 1
            continue
        elif c == '"':
            i += 1
            while i < len(line) and line[i] != '"':
                if line[i] == "\\" and i + 1 < len(line) and line[i + 1] in '$`"\\':
                    word.append(line[i + 1])
                    i += 2
                    continue
                one = ONE_WORD_EXPANSION.match(line, i)
                if one:
                    expands = True
                    word.append(one[0])
                    i = one.end()
                    continue
                if line[i] == "`" or line[i] == "$" and DOLLAR_EXPANDS.match(line, i + 1):
                    expands = splits = True
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
            tokens.append(("$(", True, True, True))
            i += 2
            continue
        else:
            if c == "`" or c == "$" and (DOLLAR_EXPANDS.match(line, i + 1) or line[i + 1:i + 2] in ("'", '"')):
                expands = splits = True
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
    command whose every argument it read, or that holds a word bash fills in only as the value of a
    flag that takes one (go_test_reading's "value", or the value an = joins to such a flag); the
    first word or operator it could not read past, for one that holds a word that splits (a $ or
    a backtick outside double quotes, a GitHub expression, in any quotes or none, or in the target
    of a redirection), a word that expands anywhere else, or an operator that opens a subshell or
    a substitution, since any of them can carry a -v past
    this check; and "" for a line that holds `go test` (RACE_TEST's spelling of one, -race or not)
    and that shell_tokens cannot split."""
    commands = []
    for line in script_lines(script.replace("\\\n", " ")):
        tokens = shell_tokens(line)
        if tokens is None:
            if RACE_TEST.search(line):
                commands.append((line, [], ""))
            continue
        for i, (text, operator, expands, _) in enumerate(tokens):
            if operator or expands or text.rsplit("/", 1)[-1] != "go":
                continue
            rest = tokens[i + 3:] if tokens[i + 1:i + 2] == [("-C", False, False, False)] else tokens[i + 1:]
            if rest[:1] != [("test", False, False, False)]:
                continue
            words, unread, after = [], None, iter(rest[1:])
            for text, operator, expands, splits in after:
                if operator and (text in COMMAND_ENDS or text.startswith(")")):
                    break
                if operator and REDIRECTION.fullmatch(text):
                    # bash reads the target as one word, but a GitHub expression in it can end the
                    # word, and the value after it reads as words of the command.
                    target = next(after, None)
                    if target and "${{" in target[0]:
                        unread = target[0]
                        break
                    continue
                if operator or splits:
                    unread = text
                    break
                words.append((text, expands))
            arguments = [text for text, _ in words]
            for (text, expands), role in zip(words, go_test_reading(arguments)):
                if expands and role != "value" and not (role == "go test" and takes_a_value(text) == "joined"):
                    unread = text
                    break
            commands.append((line, arguments, unread))
    return commands


def check_report_steps(workflows: dict, problems: list) -> list:
    """A job of any of workflows ({a repository-relative path: its parsed document}) that runs
    RESULTS_ACTION makes its report with no -v but -json's own
    test2json. Each `go test` command in a run: script of the job is held to no flag that
    verbose_flags names (a -v or -test.v of go test before -args, and a -test.v of the test binary
    after it, one dash or two, bare or with any value but test2json), since the report the action
    reads is one of them; a command whose flags it cannot read to the command's end - a line it
    cannot split, a word bash or GitHub expands but one that bash fills in inside double quotes as
    the value of a flag that takes one, a subshell or a substitution - is refused, since any of
    them can carry such a flag. A job with no `go test -json` command where this rule reads
    it is refused too. The rule does not tie a command to the report path the action is given: a
    job that makes its report in a script or another job's artifact, and holds an unrelated
    `go test -json` of its own, passes, so the report is made in a run: step of the job. Returns
    its one report line, which says how many jobs and commands it read, so a run that read none
    says so."""
    jobs = commands = 0
    for path, document in workflows.items():
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
                                                    f"the line, and a value bash fills in inside double quotes "
                                                    f"after the flag that takes it, as -run \"$pattern\""))
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
