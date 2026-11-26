#!/usr/bin/env python3
"""Generated shell forms, run under bash and read by this repository's three readers of shell.

The readers are shell_commands.commands_of with `every_body` (check-release-credentials.sh's rule:
read more than bash runs, never less), check-log-guard.sh's reader of a step's network clients
(a client outside .github/scripts/ci-log-guard.sh), and check-log-guard.sh's reader of loose
ci_log_guard_stderr calls (a call whose stdout reaches the log outside a block). A form is a
context crossed with a payload:
  - a context wraps a body of shell: a trap that fires and one that never does, `bash -c`,
    `sh -c`, eval (single- and double-quoted, and an eval inside an eval), a subshell, a brace
    group, a called, an uncalled and a `local` function, `if true` and `if false`, a case pattern
    and a case item, `((…))`, `$(…)`, `<(…)`, a here-document or a pipe that a shell reads; or it
    puts a derailer before the body: text that a reader can miscount (a `#` after the `)` of a
    nested substitution, after an operator, inside a word);
  - a payload is the client in one of the ways a script reaches a command: written directly,
    behind a prefix assignment or a keyword, in an array assigned and expanded in the body or
    after it, an array a decoy assignment in the body might shadow, an array appended to after a
    decoy, and an alias defined and used in the body or after it, or shadowed by a decoy.
The client is `bunx evil-package` for the release reader, `curl -sS https://example.com/x` for the
log guard, and `ci_log_guard_stderr curl …` for the stderr reader, whose forms source the guard.
For every form, three facts: whether bash runs the client (a recording stand-in on PATH; for the
stderr reader, whether the client's stdout reaches the log outside a block), whether this tree's
reader finds it, and, given --base, whether that tree's reader does. It also counts the forms in
which the stderr reader names a loose call where bash keeps the stdout in: that reader reads by the
line and knows no control flow, so it over-reads some, and check-log-guard.test.sh bounds how many.

It fails on:
  - a regression: a form bash runs, the base's reader finds, and this tree's reader does not;
  - a form bash runs that this tree's reader misses and no row of .github/shell-reader-misses.tsv
    (a pattern of form ids, a reader, and the reason it is a known limit) matches; and
  - a row of that list that matches no such form.
A later reviewer extends CONTEXTS, PAYLOADS or DERAILERS rather than planting forms by hand.

  python3 .github/scripts/shell_reader_forms.py [--base DIR] [--list]
    --base DIR  a directory holding another tree's shell_commands.py (and check-log-guard.sh, for
                the log guard), such as main's, to count regressions against
    --list      print every form bash runs and the readers' verdicts
check-log-guard.test.sh runs it with no base.
"""
import fnmatch
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
MISSES = HERE.parent / "shell-reader-misses.tsv"
CLIENTS = {"release": "bunx evil-package", "guard": "curl -sS https://example.com/x",
           "stderr": "ci_log_guard_stderr curl -sS https://example.com/x"}
HEADER = "shopt -s expand_aliases\n"
# The stderr reader's forms source the guard, so ci_log_guard_stderr is the library's.
HEADERS = {"release": HEADER, "guard": HEADER, "stderr": HEADER + '. "$CI_LOG_GUARD"\n'}
GUARD_LIBRARY = HERE / "ci-log-guard.sh"
# The stand-in client's line on stdout, which reaches the log outside a block when the stderr
# reader's call leaks it.
STDOUT_MARK = "CLIENT-STDOUT-LINE"


def sq(text: str) -> str:
    """`text` as one single-quoted shell word."""
    return "'" + text.replace("'", "'\\''") + "'"


def dq(text: str) -> str:
    """`text` as one double-quoted shell word that expands to itself."""
    return '"' + "".join("\\" + c if c in '"$`\\' else c for c in text) + '"'


# Each context wraps a body, `x`, which may span lines.
CONTEXTS = {
    "plain": lambda x: x,
    "subshell": lambda x: "(\n" + x + "\n)",
    "brace": lambda x: "{\n" + x + "\n}",
    "function-called": lambda x: "f() {\n" + x + "\n}\nf",
    "function-uncalled": lambda x: "f() {\n" + x + "\n}",
    "function-local": lambda x: "f() {\nlocal a\n" + x + "\n}\nf",
    "if-true": lambda x: "if true; then\n" + x + "\nfi",
    "if-false": lambda x: "if false; then\n" + x + "\nfi",
    "eval-single": lambda x: "eval " + sq(x),
    "eval-double": lambda x: "eval " + dq(x),
    "eval-in-eval": lambda x: "eval " + sq("eval " + sq(x)),
    "eval-if-false": lambda x: "if false; then eval " + sq(x) + "; fi",
    "eval-function-uncalled": lambda x: "f() { eval " + sq(x) + "; }",
    "bash-c": lambda x: "bash -c " + sq(x),
    "sh-c": lambda x: "sh -c " + sq(x),
    "bash-c-in-subshell": lambda x: "( bash -c " + sq(x) + " )",
    "trap-fires": lambda x: "trap " + sq(x) + " EXIT",
    "trap-never-fires": lambda x: "trap " + sq(x) + " USR1",
    "command-substitution": lambda x: ': "$(\n' + x + '\n)"',
    "process-substitution": lambda x: "cat <(\n" + x + "\n) >/dev/null",
    "case-pattern": lambda x: 'case x in "$(\n' + x + '\n)") : ;; esac',
    "case-item": lambda x: "case x in x)\n" + x + "\n;; esac",
    "arithmetic": lambda x: "(( $(\n" + x + "\necho 1) ))",
    "heredoc-to-bash": lambda x: "bash <<'EOT'\n" + x + "\nEOT",
    "heredoc-piped-to-bash": lambda x: "cat <<'EOT' | bash\n" + x + "\nEOT",
}
# Text a reader can miscount, before a body that runs the client.
DERAILERS = {
    "hash-after-nested-substitution": 'case x in "$(echo $(echo y)#)") : ;; esac',
    "hash-after-process-substitution": 'case x in "$(cat <(echo a)#)") : ;; esac',
    "hash-after-operator": 'case x in "$(echo y ;# )\n)") : ;; esac',
    "hash-inside-word": 'x=$(echo a#b)',
    "hash-after-subshell": 'x=$( (echo a)#c\n)',
    "hash-at-word-start": 'case x in "$(: # )\n)") : ;; esac',
}
for name, text in list(DERAILERS.items()):
    CONTEXTS["after-" + name] = (lambda t: lambda x: t + "\n" + x)(text)
# The same-line forms take a one-line body. The brace forms put the body in a group whose stdout
# goes to /dev/null, after a word that ends in a substitution's `)` and a `}` or `{` that
# continues the word (`$(echo a)}` is the word `a}`), so the group still holds the body.
CONTEXTS["same-line-hash-after-nested-substitution"] = lambda x: 'case x in "$(echo $(echo y)#b)") : ;; esac; ' + x
CONTEXTS["same-line-brace-after-substitution"] = lambda x: "{ x=$(echo a)}; " + x + "; } >/dev/null"
CONTEXTS["same-line-brace-after-process-substitution"] = lambda x: "{ x=<(echo a)}; " + x + "; } >/dev/null"
CONTEXTS["same-line-group-redirected"] = lambda x: "{ " + x + "; } >/dev/null"
# A `#` just after a substitution's `)` continues the word, so the redirect after it is the body's.
CONTEXTS["same-line-hash-after-substitution-before-redirect"] = lambda x: x + " $(echo a)#b >/dev/null"


# Each payload: (text before the context, the body the context wraps, text after it). {c} is the
# client.
PAYLOADS = {
    "direct": ("", "{c}", ""),
    "stdout-redirected": ("", "{c} >/dev/null", ""),
    "stdout-captured": ("", "v=$({c})", ""),
    "assignment-prefix": ("", "FOO=1 {c}", ""),
    "bang": ("", "! {c}", ""),
    "brace-group": ("", "{{ {c}; }}", ""),
    "if-condition": ("", "if {c}; then :; fi", ""),
    "array-in-body": ("", "a=({c})\n\"${{a[@]}}\"", ""),
    "array-used-after": ("", "a=({c})", "\"${{a[@]}}\""),
    "array-shadowed-by-decoy": ("a=({c})", "a=(true)", "\"${{a[@]}}\""),
    "array-appended-after-decoy": ("", "a=(true)", "a+=({c})\n\"${{a[@]}}\""),
    "declared-array": ("", "declare -a a=({c})\n\"${{a[@]}}\"", ""),
    "alias-in-body": ("", "alias b={q}\nb", ""),
    "alias-used-after": ("", "alias b={q}", "b"),
    "alias-shadowed-by-decoy": ("alias b={q}", "alias b=true", "b"),
    # Command words a shell builds from values the text holds elsewhere.
    "array-copied": ("base=({c})", "args=(\"${{base[@]}}\")\n\"${{args[@]}}\"", ""),
    "array-sliced": ("", "a=({c})\n\"${{a[@]:0}}\"", ""),
    "array-nameref": ("", "a=({c})\ndeclare -n r=a\n\"${{r[@]}}\"", ""),
    "array-element": ("", "a=({c})\n${{a[0]}} ${{a[1]}} ${{a[2]}}", ""),
    "variable-word": ("", "set -- {c}\n\"$@\"", ""),
    "string-split": ("", "s={q}\n$s", ""),
    "eval-of-variable": ("", "s={q}\neval \"$s\"", ""),
    "script-written-and-sourced": ("", "printf '%s\\n' {q} >x.sh\n. ./x.sh", ""),
}


def forms() -> dict:
    """{form id: {reader: the form's text, with that reader's client}}."""
    out = {}
    for context, wrap in CONTEXTS.items():
        for payload, (before, body, after) in PAYLOADS.items():
            if context.startswith("same-line") and "\n" in body:
                continue
            texts = {}
            for reader, client in CLIENTS.items():
                fill = lambda s: s.format(c=client, q=sq(client))
                parts = [fill(before), wrap(fill(body)), fill(after)]
                texts[reader] = HEADERS[reader] + "\n".join(p for p in parts if p) + "\n"
            out[f"{context}/{payload}"] = texts
    return out


def outside_blocks(lines: list) -> list:
    """The lines of one stream that stand outside every ci-log-guard.sh stop-commands block."""
    out, token = [], None
    for line in lines:
        if token is None and line.startswith("::stop-commands::"):
            token = line[len("::stop-commands::"):]
        elif token is not None and line == f"::{token}::":
            token = None
        elif token is None:
            out.append(line)
    return out


def bash_runs(text: str, reader: str) -> bool:
    """Whether bash runs the reader's client when it runs `text` as a script: for the stderr
    reader, whether the client's stdout reaches the log outside a block (on the step's stdout, or
    on its stderr outside every block), which is what a loose ci_log_guard_stderr call lets out."""
    client = CLIENTS[reader]
    word = client.split()[1] if reader == "stderr" else client.split()[0]
    with tempfile.TemporaryDirectory() as work:
        bin_dir = Path(work, "bin")
        bin_dir.mkdir()
        record = Path(work, "ran")
        stand_in = bin_dir / word
        stand_in.write_text(f'#!/bin/sh\necho {word} >>"{record}"\necho {STDOUT_MARK}\n')
        stand_in.chmod(0o755)
        Path(work, "form.sh").write_text(text)
        env = dict(os.environ, PATH=f"{bin_dir}:{os.environ['PATH']}", CI_LOG_GUARD=str(GUARD_LIBRARY))
        try:
            done = subprocess.run(["bash", "form.sh"], cwd=work, env=env, stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=10)
            out, err = done.stdout, done.stderr
        except subprocess.TimeoutExpired:
            out = err = ""
        ran = record.is_file() and word in record.read_text().split()
        if reader != "stderr":
            return ran
        return ran and (STDOUT_MARK in out.split("\n") or STDOUT_MARK in outside_blocks(err.split("\n")))


def read_with(directory: Path, texts: dict) -> dict:
    """{form id: {reader: whether the readers of `directory` find the client}}, read in a process
    of its own, so each tree's shell_commands is the one its check imports."""
    result = subprocess.run([sys.executable, __file__, "--read", str(directory)], input=json.dumps(texts),
                            capture_output=True, text=True, env=dict(os.environ, PYTHONDONTWRITEBYTECODE="1"))
    if result.returncode != 0:
        sys.exit(f"shell_reader_forms: reading with {directory} failed:\n{result.stderr}")
    return json.loads(result.stdout)


def read_here(directory: str) -> None:
    """The --read mode: reads {id: {reader: text}} on stdin, prints {id: {reader: found}}."""
    sys.path.insert(0, directory)
    import shell_commands
    guard = None
    script = Path(directory, "check-log-guard.sh")
    if script.is_file():
        source = script.read_text()
        body = source[source.index("<<'PY'\n") + len("<<'PY'\n"):source.rindex("\nPY")]
        sys.argv = ["check-log-guard", "check"]
        guard = {}
        exec(compile(body[:body.index("files = sorted(")], str(script), "exec"), guard)
    takes_every_body = "every_body" in shell_commands.commands_of.__code__.co_varnames
    out = {}
    for form, texts in json.load(sys.stdin).items():
        verdicts = {}
        text = texts["release"]
        found = shell_commands.commands_of(text, every_body=True) if takes_every_body else shell_commands.commands_of(text)
        verdicts["release"] = any(words and words[0].rsplit("/", 1)[-1] == "bunx" for words in found)
        if guard is not None:
            commands = guard["commands"](texts["guard"])
            verdicts["guard"] = any(not routed and guard["network_client"](words) == "curl" for routed, words in commands)
            # The third reader: a ci_log_guard_stderr call whose stdout reaches the log, read as
            # check-log-guard.sh's read() reads a text.
            stderr_text = guard["EXPRESSION"].sub("EXPR", texts["stderr"])
            verdicts["stderr"] = bool(guard["loose_stderr_guards"](guard["without_heredocs"](stderr_text)))
        out[form] = verdicts
    print(json.dumps(out))


def listed_misses() -> tuple:
    """({(form id pattern, reader): its row's number} of .github/shell-reader-misses.tsv, its
    problems). A pattern is an fnmatch pattern of form ids (`*/variable-word`)."""
    rows, problems = {}, []
    if not MISSES.is_file():
        return rows, problems
    for number, line in enumerate(MISSES.read_text().split("\n"), 1):
        if not line.strip() or line.startswith("#"):
            continue
        fields = line.split("\t")
        if len(fields) != 3 or not fields[2].strip() or fields[1] not in CLIENTS:
            problems.append(f"{MISSES.name}:{number}: not a form id, release or guard, and a reason, tab-separated")
            continue
        rows[(fields[0], fields[1])] = number
    return rows, problems


def main() -> int:
    args = sys.argv[1:]
    if args[:1] == ["--read"]:
        read_here(args[1])
        return 0
    base = Path(args[args.index("--base") + 1]) if "--base" in args else None
    generated = forms()
    ran = {form: {reader: bash_runs(text, reader) for reader, text in texts.items()}
           for form, texts in generated.items()}
    head = read_with(HERE, generated)
    based = read_with(base, generated) if base else None
    rows, problems = listed_misses()
    regressions, missed, both, over = [], [], [], []
    used = set()
    for form in generated:
        for reader in CLIENTS:
            runs, finds = ran[form][reader], head[form].get(reader)
            if reader == "stderr" and finds and not runs:
                over.append(form)
            row = next((key for key in rows if key[1] == reader and fnmatch.fnmatchcase(form, key[0])), None)
            if not runs or finds:
                continue
            if row:
                used.add(row)
            base_finds = based[form].get(reader) if based else None
            if base_finds:
                regressions.append((form, reader))
            else:
                both.append((form, reader))
            if not row:
                missed.append((form, reader))
    problems += [f"{MISSES.name}:{number}: {pattern} ({reader}) names no form bash runs and this tree's reader "
                 f"misses: remove the row" for (pattern, reader), number in sorted(rows.items(), key=lambda r: r[1])
                 if (pattern, reader) not in used]
    if "--list" in args:
        for form in generated:
            for reader in CLIENTS:
                if ran[form][reader]:
                    print(f"{form}\t{reader}\tthis tree {'finds' if head[form].get(reader) else 'MISSES'}"
                          + (f"\tbase {'finds' if based[form].get(reader) else 'misses'}" if based and reader in based[form] else ""))
    runs = {reader: sum(ran[form][reader] for form in generated) for reader in CLIENTS}
    for form, reader in regressions:
        print(f"REGRESSION {form} ({reader}): bash runs the client and the base's reader finds it; this tree's does not")
    for form, reader in missed:
        print(f"UNLISTED {form} ({reader}): bash runs the client and this tree's reader misses it, and "
              f"{MISSES.name} does not list it")
    for problem in problems:
        print(problem)
    if "--list" in args:
        for form in over:
            print(f"OVER {form} (stderr): this tree's reader names a loose call where bash keeps the stdout in")
    print(f"shell_reader_forms: {len(generated)} forms for each of {len(CLIENTS)} readers; bash runs the client "
          f"in {runs['release']} (release), {runs['guard']} (guard) and {runs['stderr']} (stderr, the client's stdout outside a block); {len(regressions)} regression(s)"
          + (f" against {base}" if base else "") + f"; {len(both)} missed by this tree"
          + (" and by the base" if base else "") + f", {len(missed)} of them unlisted; the stderr reader names a "
          f"loose call in {len(over)} forms where bash keeps the client's stdout in (--list names them)")
    return 1 if regressions or missed or problems else 0


if __name__ == "__main__":
    sys.exit(main())
