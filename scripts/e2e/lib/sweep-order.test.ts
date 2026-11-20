import { afterAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { command, rowMs, rowOf } from "@legion/test-fixtures/row-waits";
import { noHookLimit, removeScratch } from "../../../packages/dispatch/e2e/__tests__/step-end";
import { type Driver, mark, sweep, sweepOrder } from "./sweep-order";

// R3 of pull request #4236: "The next start of a test stack removes each Postgres of a stack that
// it can show dead. … It leaves a Postgres whose stack it cannot judge, and it names that Postgres
// in one line." A shell stack's start that refuses, because of what a dead run left, before it
// runs remove_leftover_postgres never reaches the removal while those leftovers stay: the dead
// stack's Postgres stays and no line names it. A start that creates its own Postgres before the
// removal starts it beside the dead stacks' (lib/sweep-order.ts reads each refusal by what it does,
// and each mark).
//
// The first rows run the search over this tree and over planted scripts. The rest run each
// driver's own file, in its own order: lib/sweep-order.ts names the drivers, the refusals each one
// reaches and each mark of its own Postgres, and each row makes one refusal refuse, or lets the
// driver reach one create of its Postgres, and requires that the removal ran first.

const root = join(import.meta.dir, "..", "..", "..");
const scratch = mkdtempSync(join(tmpdir(), "sweep-order-"));
// The rows' scratch goes in a process of its own, which may take what the CI step leaves, or the
// limit of the rows that start a driver (`walkBound`), and the hook has no limit of Bun's: 8 such
// removals at once took 3.5 to 5.3 s beside a shared machine's load (step-end.ts says why).
afterAll(() => removeScratch([scratch], walkBound), noHookLimit);

/** The refusals whose leftovers the rows below can plant, each a function of the scripts. */
const named = ["refuse_leftovers", "ports_free", "lock_services"] as const;

/** A planted tree at `name` with the real lib/leftovers.sh and each script of `scripts`, by its
 *  path under scripts/e2e, given as its lines. */
function plant(name: string, scripts: Record<string, string[]>): string {
  const planted = join(scratch, name);
  mkdirSync(join(planted, "scripts", "e2e", "lib"), { recursive: true });
  copyFileSync(
    join(import.meta.dir, "leftovers.sh"),
    join(planted, "scripts", "e2e", "lib", "leftovers.sh")
  );
  copyFileSync(
    join(import.meta.dir, "process-tree.sh"),
    join(planted, "scripts", "e2e", "lib", "process-tree.sh")
  );
  for (const [path, lines] of Object.entries(scripts))
    writeFileSync(join(planted, "scripts", "e2e", path), `${lines.join("\n")}\n`);
  return planted;
}

/** Each refusal and mark a planted search found before a removal, as `NAME at AT from FROM
 *  through VIA`. */
function found(tree: string): string[] {
  return sweepOrder(tree).findings.flatMap((f) =>
    f.before.map((c) => `${c.name} at ${c.at} from ${c.from} through ${c.via.join(",")}`)
  );
}

const head = [
  "#!/usr/bin/env bash",
  '. "$root/scripts/e2e/lib/leftovers.sh"',
  'fail() { echo "$*" >&2; exit 1; }',
];

describe("the search for a start that refuses or creates its Postgres before it removes the Postgres of dead stacks", () => {
  test("no driver of this tree refuses or marks before its removal, and the search reads every driver", () => {
    const { drivers, findings, unread } = sweepOrder(root);
    // A search that read nothing passed nothing: every shell stack that starts a Postgres
    // container runs the removal (leftovers.test.ts's list), so each one is a driver here, and the
    // search parsed every script a driver is or calls.
    expect(drivers.map((each) => each.driver).sort()).toEqual([
      "packages/pi-legion/scripts/skill-scenarios/rig.sh",
      "scripts/e2e/claude-backend-sandbox.sh",
      "scripts/e2e/claude-backend-tmux.sh",
      "scripts/e2e/controller-start-tmux.sh",
      "scripts/e2e/controller-sweep-kind.sh",
      "scripts/e2e/dispatch-user-turns.sh",
      "scripts/e2e/parked-decision-devbox.sh",
      "scripts/e2e/stage1-skeleton.sh",
      "scripts/e2e/stage2-tmux-supervision.sh",
      "scripts/e2e/stage3-devbox-workflow.sh",
      "scripts/e2e/stage4a-sandbox-runtime.sh",
      "scripts/e2e/stage4b-sandbox-tree.sh",
      "scripts/e2e/verifiers-staging-token.sh",
    ]);
    expect(unread.filter((each) => each.matters)).toEqual([]);
    expect(findings).toEqual([]);
  });

  test("every script that names a Postgres image is a driver, or holds a removal a driver reaches", () => {
    // A Postgres image as the drivers name one: the pinned pgvector or PostgreSQL digest, through
    // the mirror or through the repository's own copy, or the session database's image variable.
    const postgresImage =
      /pgvector\/pgvector@sha256:|library\/postgres@sha256:|legion\/ci@sha256:(?:2358fcba|979bccfd)|"\$session_db_image"/;
    const named = [
      ...new Bun.Glob("scripts/e2e/*.sh").scanSync({ cwd: root }),
      ...new Bun.Glob("scripts/e2e/lib/*.sh").scanSync({ cwd: root }),
      "packages/pi-legion/scripts/skill-scenarios/rig.sh",
    ].filter((path) => postgresImage.test(readFileSync(join(root, path), "utf8")));
    const { drivers } = sweepOrder(root);
    const removing = new Set([
      ...drivers.map((each) => each.driver),
      ...drivers.flatMap((each) =>
        each.calls.filter((call) => call.name === sweep).map((call) => call.at.replace(/:\d+$/, ""))
      ),
    ]);
    expect(named.length).toBeGreaterThan(0);
    expect(named.filter((path) => !removing.has(path))).toEqual([]);
  });

  test("a planted driver whose refusal or mark comes first, directly or through a function of a library it sources, is found", () => {
    const planted = plant("planted", {
      "lib/process-tree.sh": [":"],
      "lib/planted.sh": [
        // The shell's own expansion, which this planted library holds verbatim so the search reads
        // its sibling source.
        // biome-ignore lint/suspicious/noTemplateCurlyInString: the shell expands it, not JavaScript
        '. "${BASH_SOURCE[0]%/*}/process-tree.sh"',
        "check_port() {",
        '  [ -z "$(ss -Hltn "sport = :$1")" ] || fail "port $1 is taken"',
        "}",
      ],
      "first.sh": [
        ...head,
        '. "$root/scripts/e2e/lib/planted.sh"',
        "begin() {",
        "  refuse_leftovers legion-planted",
        "}",
        "begin",
        "check_port 1234",
        'pg_mark=$(postgres_mark "$work" "$pg") || fail "no mark"',
        'remove_leftover_postgres || fail "no removal"',
      ],
      "after.sh": [
        ...head,
        'remove_leftover_postgres || fail "no removal"',
        "refuse_leftovers legion-after",
      ],
    });
    const parsed = sweepOrder(planted);
    // The number of the first line of `file` past its first `after` lines that holds `text`.
    const lineOf = (file: string, text: string, after = 0) => {
      const line = readFileSync(join(planted, file), "utf8")
        .split("\n")
        .findIndex((each, index) => index >= after && each.includes(text));
      expect(line).not.toBe(-1);
      return line + 1;
    };
    const first = "scripts/e2e/first.sh";
    const leftovers = "scripts/e2e/lib/leftovers.sh";
    const checkLine = lineOf("scripts/e2e/lib/planted.sh", '[ -z "$(ss -Hltn "sport = :$1")" ]');
    const markLine = lineOf(first, 'pg_mark=$(postgres_mark "$work" "$pg")');
    expect(parsed.drivers.map((each) => each.driver)).toEqual(["scripts/e2e/after.sh", first]);
    expect(parsed.unread).toEqual([]);
    const findings = parsed.findings.flatMap((each) =>
      each.before.map(
        (call) => `${call.name} at ${call.at} from ${call.from} through ${call.via.join(",")}`
      )
    );
    const beginLine = readFileSync(join(planted, first), "utf8").split("\n").indexOf("begin") + 1;
    expect(beginLine).toBeGreaterThan(0);
    // Each refusal of lib/leftovers.sh's refuse_leftovers, which the driver reaches through
    // `begin`, named by its text, so a line that moves still matches and a refusal added to or
    // taken from refuse_leftovers fails here, naming it: each `fail` that names why the walk read
    // nothing, its `return 2`, the `exit` that keeps find's status, and the last `fail`.
    const noProcess = lineOf(leftovers, 'fail "refuse_leftovers: $proc lists no process');
    const noFile = lineOf(leftovers, 'fail "refuse_leftovers: no file to keep');
    const findFailed = lineOf(leftovers, 'fail "refuse_leftovers: find exited');
    const viaBegin = `from ${first}:${beginLine} through begin,refuse_leftovers`;
    // Every finding, exactly and in the order the driver meets them: a reordered, a repeated or a
    // missing one fails here, as a look for each one alone would not see.
    expect(findings).toEqual([
      `fail at ${leftovers}:${noProcess} ${viaBegin}`,
      `return at ${leftovers}:${lineOf(leftovers, "return 2", noProcess)} ${viaBegin}`,
      `fail at ${leftovers}:${noFile} ${viaBegin}`,
      `return at ${leftovers}:${lineOf(leftovers, "return 2", noFile)} ${viaBegin}`,
      `exit at ${leftovers}:${lineOf(leftovers, "|| exit)")} ${viaBegin}`,
      `fail at ${leftovers}:${findFailed} ${viaBegin}`,
      `return at ${leftovers}:${lineOf(leftovers, "return 2", findFailed)} ${viaBegin}`,
      `fail at ${leftovers}:${lineOf(leftovers, 'fail "an earlier $prefix run whose driver is gone')} ${viaBegin}`,
      `fail at scripts/e2e/lib/planted.sh:${checkLine} from ${first}:${lineOf(first, "check_port 1234")} through check_port`,
      `postgres_mark at ${first}:${markLine} from ${first}:${markLine} through `,
      // The mark's own refusal: its check runs the mark's command.
      `fail at ${first}:${markLine} from ${first}:${markLine} through `,
    ]);
  });

  test("the search reads a refusal in each form a command takes on one line, and a removal after `; then`", () => {
    // held refuses where a lock file exists, a file a dead run can leave; each line calls it in
    // one form, before the removal.
    const forms: [string, string][] = [
      ["after ;", 'true; held "$lock"'],
      ["after &&", 'true && held "$lock"'],
      ["after ||", 'false || held "$lock"'],
      ["after |", 'true | held "$lock"'],
      ["after then", 'if true; then held "$lock"; fi'],
      ["after then, a library's refusal", 'if [ -n "$X" ]; then refuse_leftovers legion-then; fi'],
      ["after do", 'for x in 1; do held "$lock"; done'],
      ["after else", 'if false; then :; else held "$lock"; fi'],
      ["after elif", 'if false; then :; elif held "$lock"; then :; fi'],
      ["after if", 'if held "$lock"; then :; fi'],
      ["after while", 'while held "$lock"; do break; done'],
      ["after until", 'until held "$lock"; do break; done'],
      ["after !", '! held "$lock"'],
      ["after {", '{ held "$lock"; }'],
      ["after (", '( held "$lock" )'],
      ["after $(", 'x=$(held "$lock")'],
      ["after a backquote", 'x=`held "$lock"`'],
    ];
    const lines = [
      ...head,
      'held() { [ ! -e "$1" ] || fail "$1 is held"; }',
      "lock=/run/planted.lock",
    ];
    const planted = plant("forms", {
      "forms.sh": [
        ...lines,
        ...forms.map(([, line]) => line),
        'remove_leftover_postgres || fail "no removal"',
      ],
      // The review's planted script: a refusal, then the removal after `; then` on one line.
      "then-sweep.sh": [
        ...head,
        "refuse_leftovers legion-ts",
        'if [ -n "$X" ]; then remove_leftover_postgres || fail "no removal"; fi',
      ],
    });
    const parsed = sweepOrder(planted);
    const { drivers, findings } = parsed;
    expect(parsed.unread).toEqual([]);
    const from = new Set(findings.flatMap((each) => each.before.map((call) => call.from)));
    expect(
      Object.fromEntries(
        forms.map(([form], i) => [form, from.has(`scripts/e2e/forms.sh:${lines.length + i + 1}`)])
      )
    ).toEqual(Object.fromEntries(forms.map(([form]) => [form, true])));
    expect(drivers.find((each) => each.driver === "scripts/e2e/then-sweep.sh")?.sweep).toBe(
      "scripts/e2e/then-sweep.sh:5"
    );
    const leftoversLine =
      readFileSync(join(planted, "scripts/e2e/lib/leftovers.sh"), "utf8")
        .split("\n")
        .findIndex((line) => line.includes('fail "refuse_leftovers: $proc lists no process')) + 1;
    expect(leftoversLine).toBeGreaterThan(0);
    expect(found(planted)).toContain(
      `fail at scripts/e2e/lib/leftovers.sh:${leftoversLine} from scripts/e2e/then-sweep.sh:4 through refuse_leftovers`
    );
  });

  test("a check of an input, a tool or the model route can come before the removal; a check of a file or of a command's output cannot", () => {
    const planted = plant("inputs", {
      "inputs.sh": [
        ...head,
        'need() { [ -n "$1" ] || fail "$2 is empty"; }',
        'pid_ok() { [ -n "$1" ] || fail "no pid"; }',
        "route=$PLANTED_ROUTE",
        'case "$route" in gateway | key-file) ;; *) fail "no route $route" ;; esac',
        '[ -n "$PLANTED_KEY" ] || fail "PLANTED_KEY is unset"',
        'for tool in docker bun; do command -v "$tool" >/dev/null || fail "$tool is required"; done',
        'need "$route" route',
        '[[ $route =~ ^[a-z-]+$ ]] || fail "route $route"',
        '[ -r "$PLANTED_KEY" ] || fail "PLANTED_KEY names no readable file"',
        'taken=$(ss -Hltn "sport = :4242")',
        '[ -z "$taken" ] || fail "port 4242 is taken"',
        'pid_ok "$(cat /run/planted.pid)"',
        '(set -C && : >/run/planted.lock) || fail "a batch holds the lock"',
        'remove_leftover_postgres || fail "no removal"',
      ],
    });
    expect(found(planted)).toEqual([
      "fail at scripts/e2e/inputs.sh:12 from scripts/e2e/inputs.sh:12 through ",
      "fail at scripts/e2e/inputs.sh:14 from scripts/e2e/inputs.sh:14 through ",
      "fail at scripts/e2e/inputs.sh:5 from scripts/e2e/inputs.sh:15 through pid_ok",
      "fail at scripts/e2e/inputs.sh:16 from scripts/e2e/inputs.sh:16 through ",
    ]);
  });

  test("under set -e, or a trap on ERR that exits, each command before the removal whose failure nothing tests is a refusal, unless it reads only the start's inputs", () => {
    const lock = "lock=/run/planted.lock";
    // Each driver: its lines before the removal, and whether the search must find a refusal.
    const drivers: Record<string, { readonly lines: string[]; readonly refuses: boolean }> = {
      flock: { lines: ["set -Eeuo pipefail", lock, 'exec 9>"$lock"; flock -n 9'], refuses: true },
      "no-file": { lines: ["set -euo pipefail", lock, '[ ! -e "$lock" ]'], refuses: true },
      "free-port": {
        lines: ["set -euo pipefail", 'test -z "$(ss -Hltn "sport = :5432")"'],
        refuses: true,
      },
      "lock-dir": { lines: ["set -euo pipefail", lock, 'mkdir "$lock.d"'], refuses: true },
      "held-output": {
        lines: ["set -euo pipefail", lock, 'held=$(flock -n "$lock" true)'],
        refuses: true,
      },
      "a function's last test": {
        lines: ["set -euo pipefail", lock, 'held() { [ ! -e "$lock" ]; }', "held"],
        refuses: true,
      },
      "set -o errexit": { lines: ["set -o errexit", lock, 'mkdir "$lock.d"'], refuses: true },
      "a trap on ERR": { lines: ["trap 'exit 1' ERR", lock, 'mkdir "$lock.d"'], refuses: true },
      "control: no set -e": { lines: [lock, 'mkdir "$lock.d"'], refuses: false },
      "control: set +e after it": {
        lines: ["set -euo pipefail", "set +e", lock, 'mkdir "$lock.d"'],
        refuses: false,
      },
      "control: a status that is tested": {
        lines: [
          "set -euo pipefail",
          lock,
          'mkdir "$lock.d" || true',
          'if [ -e "$lock" ]; then :; fi',
        ],
        refuses: false,
      },
      "control: what reads only the start's inputs": {
        lines: [
          "set -euo pipefail",
          'root=$(cd "$(dirname "$0")/../.." && pwd)',
          'cd "$root/scripts"',
          '[ -n "$PLANTED_KEY" ]',
          'echo "start of $(basename "$0")"',
          "export PLANTED_AT=$(date +%s)",
          "cat <<EOF",
          "usage",
          "EOF",
        ],
        refuses: false,
      },
    };
    const planted = plant(
      "set-e",
      Object.fromEntries(
        Object.entries(drivers).map(([name, { lines }]) => [
          `${name.replace(/[^a-z]+/g, "-")}.sh`,
          [...head, ...lines, 'remove_leftover_postgres || fail "no removal"'],
        ])
      )
    );
    const { drivers: read, findings } = sweepOrder(planted);
    expect(read).toHaveLength(Object.keys(drivers).length);
    const refusing = new Set(findings.map((f) => f.driver));
    expect(
      Object.fromEntries(
        Object.keys(drivers).map((name) => [
          name,
          refusing.has(`scripts/e2e/${name.replace(/[^a-z]+/g, "-")}.sh`),
        ])
      )
    ).toEqual(
      Object.fromEntries(Object.entries(drivers).map(([name, { refuses }]) => [name, refuses]))
    );
  });

  // Each script below runs its cases through one runner (order-search-cases.sh), which bounds each
  // case: two runs of its planted driver, the start of the lock's holder and one search, at most
  // `order_search_case_s` in all. A script's row limit is that bound for each of its cases, read
  // from the runner and the script, with a minute above it for the row's own spawn of bash, so no
  // load the runner's own bounds hold can fail the row before the runner says why.
  const runner = readFileSync(join(import.meta.dir, "order-search-cases.sh"), "utf8");
  const caseS = Number(/^order_search_case_s=(\d+)$/m.exec(runner)?.[1] ?? Number.NaN);
  for (const [script, title] of [
    [
      "order-search-set-e-refusal.test.sh",
      "the acceptance's red test of the refusals that set -e makes passes over this tree's search: each form ends a planted start before its removal, and the search names it",
    ],
    [
      "order-search-errexit-shebang-arith.test.sh",
      "the acceptance's red test of errexit from a script's first line and of a command substitution in (( … )) passes over this tree's search: each form ends a planted start before its removal, and the search names it",
    ],
    [
      "order-search-errexit-scope.test.sh",
      "a set +e that bash does not apply to the start's own shell leaves set -e on for the search: in a branch, a list, a loop or a case arm that does not run, a subshell, a command substitution, a function in parentheses, a pipeline and a list in the background, each form ends a planted start before its removal, and the search names the check that ends it",
    ],
    [
      "order-search-errexit-builtins.test.sh",
      "the search reads each way bash turns errexit on or off as bash does, through set, shopt -o, builtin, command, eval, a quoted name, local - and a trap on ERR in any case, and names as unread each command whose words it cannot know, so none ends a planted start before its removal unseen",
    ],
  ] as const) {
    const cases = readFileSync(join(import.meta.dir, script), "utf8").match(
      /^(order_search_case|on) /gm
    );
    test(
      title,
      () => {
        const red = Bun.spawnSync(["bash", join(import.meta.dir, script), root]);
        expect({ code: red.exitCode, out: red.stdout.toString() }).toEqual({
          code: 0,
          out: expect.stringContaining("PASS: every form is found"),
        });
      },
      // The runner's bound for each case of the script, and a minute for the row's own work.
      (cases?.length ?? Number.NaN) * caseS * 1000 + 60_000
    );
  }

  // What the next row waits for, which its limit is the sum of (`rowMs` of
  // `@legion/test-fixtures/row-waits`): the search's command, a process of its own.
  const theSearch = command("the search's command");
  const oneSearch = [theSearch];
  test(
    "the search's command says UNKNOWN, not clean, where it read no driver",
    () => {
      const empty = mkdtempSync(join(scratch, "empty-"));
      rowOf(oneSearch).charge(theSearch);
      const run = Bun.spawnSync(["bun", join(import.meta.dir, "sweep-order.ts"), empty]);
      expect({ code: run.exitCode, out: run.stdout.toString() }).toEqual({
        code: 2,
        out: expect.stringContaining("UNKNOWN: no script under"),
      });
    },
    rowMs(oneSearch)
  );
});

/** A stand-in for each command a driver runs on its way, and for mktemp, which makes each
 *  directory under $STANDIN_ROOT. Each records its call in $STANDIN_CALLS. `docker ps` of a name
 *  prefix lists a dead run's NATS container where $STANDIN_DEAD_PID is set; `ss` lists a listener
 *  on the port it is asked of where $STANDIN_TAKEN is set, and on the port $STANDIN_TAKEN_PORT
 *  names alone where that is set; `bun … sweep` is the removal, recorded as SWEEP, and `bun … label`
 *  is the real mark command, which only prints a label. A `docker run` or `docker create` is
 *  recorded as CREATE, with its name and its mark, where it carries a mark, else as RUN.
 *  $STANDIN_STOP_AT ends the driver with SIGTERM at the create of that number, or, set to `first`,
 *  at the first of the removal and any container. `flock` is the real one, on a lock file of the
 *  row's own. */
const standIn = `#!/usr/bin/env bash
name=\${0##*/}
printf '%s %s\\n' "$name" "$*" >>"$STANDIN_CALLS"
stop() { kill -TERM "$GATE_DRIVER"; }
case $name in
  mktemp)
    flags=() template=tmp.XXXXXXXXXX
    for a in "$@"; do case $a in -*) flags+=("$a") ;; *) template=$a ;; esac; done
    exec /usr/bin/mktemp "\${flags[@]}" "$STANDIN_ROOT/\${template##*/}" ;;
  docker)
    case \${1:-} in
      ps)
        for a in "$@"; do
          case $a in name=^*-) [ -z "\${STANDIN_DEAD_PID:-}" ] || printf '%snats-%s|\\n' "\${a#name=^}" "$STANDIN_DEAD_PID" ;; esac
        done ;;
      inspect) date -u +%FT%T.%NZ ;;
      run | create)
        label='' container='' previous=''
        for a in "$@"; do
          case $previous in
            --label) case $a in dev.legion.dispatch-e2e.postgres-mark=*) label=\${a#*=} ;; esac ;;
            --name) container=$a ;;
          esac
          previous=$a
        done
        if [ -n "$label" ]; then
          printf 'CREATE %s %s\\n' "$container" "$label" >>"$STANDIN_CALLS"
          [ "\${STANDIN_STOP_AT:-}" != "$(grep -c '^CREATE ' "$STANDIN_CALLS")" ] || stop
        else
          printf 'RUN %s\\n' "$container" >>"$STANDIN_CALLS"
        fi
        [ "\${STANDIN_STOP_AT:-}" != first ] || stop
        printf 'id-%s\\n' "$container" ;;
    esac ;;
  mise)
    case \${1:-} in
      which) printf '%s\n' /bin/true ;;
      where) printf '%s\n' "$STANDIN_ROOT/mise" ;;
    esac ;;
  bun)
    case " $* " in
      *" sweep "*) echo SWEEP >>"$STANDIN_CALLS"; [ "\${STANDIN_STOP_AT:-}" != first ] || stop ;;
      *" label "*) exec "$STANDIN_BUN" "$@" ;;
    esac ;;
  ss)
    case " $* " in *":\${STANDIN_TAKEN_PORT:-none} "*) echo "LISTEN 0 4096 127.0.0.1:\${*##*:} 0.0.0.0:* users:((\\"docker-proxy\\",pid=1,fd=3))" ;; esac
    [ -z "\${STANDIN_TAKEN:-}" ] || echo "LISTEN 0 4096 127.0.0.1:\${*##*:} 0.0.0.0:* users:((\\"stand-in\\",pid=1,fd=3))" ;;
esac
exit 0
`;

const standIns = [
  "agent-secrets",
  "aws",
  "bun",
  "claude",
  "curl",
  "docker",
  "gh",
  "getent",
  "go",
  "hawk-token",
  "jj",
  "jq",
  "kubectl",
  "mise",
  "mktemp",
  "openssl",
  "pgrep",
  "python3",
  "secrets",
  "shellcheck",
  "sleep",
  "ss",
  "sudo",
  "tmux",
];

/** What a row cuts: the functions that run whole, the other names that run, whether the driver's
 *  own `set` runs, and, where a row makes one refusal refuse, the file and line its branch begins
 *  at, from which every command runs. A row that runs the driver to its creates skips `set`, so no
 *  errexit or nounset ends the driver at a line that reads what a skipped command would have made:
 *  the shell still reaches each line in the order the driver's own lines and branches give. */
interface Cut {
  readonly run: readonly string[];
  readonly follow: readonly string[];
  readonly keepSet: boolean;
  readonly from?: string;
}

/** The shell that runs a driver's own file in its own order with what is slow or outside the
 *  machine cut. A DEBUG trap (bash's extdebug: a trap that answers non-zero skips the command)
 *  runs, at the driver's top level and in each function on its way: each assignment, source,
 *  `for`, `case`, test and builtin of the shell's own control, every command in a subshell or a
 *  command substitution, the whole of each function of `run`, each call of a name of `follow`, and
 *  each command from the line `from` names on; it skips every other command, and names each in
 *  $GATE_SKIPPED. The cut cannot change the order of the calls the rows read: it runs every line of
 *  the driver on the way to them and skips only commands that call none of them, so the shell
 *  reaches them in the order the driver's own lines and branches give. What a skipped command
 *  would have refused (a missing tool, an input left unset) is never refused, and the driver's
 *  own traps are skipped too, so a driver the row ends runs no cleanup. */
function gate({ run, follow, keepSet, from }: Cut): string {
  return `shopt -s extdebug
set -T
export GATE_DRIVER=$$
__gate_run=" ${run.join(" ")} "
__gate_follow=" ${[...run, ...follow].join(" ")} "
__gate_from=${JSON.stringify(from === undefined ? "" : join(root, from))}
__gate_skip() {
  [ -z "\${__gate_reached:-}" ] || return 1
  if [ -n "$__gate_from" ] && [ "\${BASH_SOURCE[1]}:$1" = "$__gate_from" ]; then
    __gate_reached=1
    return 1
  fi
  [ "$BASH_SUBSHELL" -eq 0 ] || return 1
  local frame
  for frame in "\${FUNCNAME[@]:1}"; do
    case $__gate_run in *" $frame "*) return 1 ;; esac
  done
  local word=\${BASH_COMMAND%%[[:space:]]*}
  case $word in
    *=* | for | . | source | ${keepSet ? "set | " : ""}shopt | shift | '[[' | '[' | test | true | false | : | return | break | continue | local | declare | typeset | export | readonly | unset | read | mapfile | readarray | case) return 1 ;;
  esac
  case $__gate_follow in *" $word "*) return 1 ;; esac
  printf '%s\\n' "$BASH_COMMAND" >>"$GATE_SKIPPED"
  return 0
}
trap '! __gate_skip "$LINENO"' DEBUG
# A skipped background start leaves $! as it was; one no-op job gives it a pid, of a job that ended.
: &
wait "$!"
. "$0" "$@"`;
}

/** What makes each refusal refuse, and the words of its refusal. */
const refuses: Record<(typeof named)[number], { env: Record<string, string>; said: RegExp }> = {
  refuse_leftovers: {
    env: {},
    said: /an earlier \S+ run whose driver is gone left these running: container \S+-nats-\d+ /,
  },
  ports_free: { env: { STANDIN_TAKEN: "1" }, said: /port \d+ is taken on the devbox/ },
  lock_services: { env: {}, said: /a batch already holds \S+\/services\/up/ },
};

/** The ports a driver's operator gives it, which a driver reads at its top before any refusal. No
 *  row binds one or asks the machine of one: `ss` and `docker` are the stand-ins above, and each
 *  number is only text in a refusal's line. */
const operatorInputs = {
  LEGION_E2E_DAEMON_PORT: "13372",
  LEGION_E2E_WORKER_STREAM_PORT: "13373",
  LEGION_E2E_DISPATCH_PORT: "13374",
  LEGION_E2E_ENVOY_PORT: "13375",
  LEGION_E2E_NATS_PORT: "13376",
  LEGION_E2E_SESSION_DB_PORT: "13377",
};

/** The arguments and inputs a driver needs to reach its refusals at all. */
const startOf: Record<string, { args: string[]; env: Record<string, string> }> = {
  "packages/pi-legion/scripts/skill-scenarios/rig.sh": {
    args: ["batch", "askgate", "0", "on"],
    env: { SKILL_SCENARIOS_ASKGATE_CASE: "facts" },
  },
};

/** A pid no process of this machine has: one this row started and waited for. */
function deadPid(): string {
  return Bun.spawnSync(["bash", "-c", "echo $$"], { stdout: "pipe" }).stdout.toString().trim();
}

// A driver's run here sets no proc_root, so its refuse_leftovers reads every process of its PID
// namespace, and a row's time grows with the machine's processes: 14 s on a dev box of 840, when
// the walk started three programs for each process. It now reads them in its own shell and starts
// one find for them all, and the bound stays the one those rows were given.
const walkBound = 120_000;

const bin = join(scratch, "bin");
mkdirSync(bin);
writeFileSync(join(bin, "stand-in"), standIn);
chmodSync(join(bin, "stand-in"), 0o755);
for (const name of standIns) symlinkSync("stand-in", join(bin, name));
const bearer = join(scratch, "dispatch-token");
writeFileSync(bearer, "stand-in-bearer\n", { mode: 0o600 });

/** One run of `driver`'s own file through the gate, with `env` over the row's own: what it said,
 *  the stand-ins' log, what the cut skipped, its pid and its run directory. */
function start(
  driver: string,
  cut: Cut,
  env: Record<string, string>,
  setUp: (dir: string) => void = () => {}
) {
  const dir = mkdtempSync(join(scratch, "run-"));
  const calls = join(dir, "calls.log");
  const skipped = join(dir, "skipped.log");
  writeFileSync(calls, "");
  writeFileSync(skipped, "");
  for (const each of ["home", "tmp", "xdg", "work/services"])
    mkdirSync(join(dir, each), { recursive: true });
  writeFileSync(join(dir, "kubeconfig"), "apiVersion: v1\n");
  setUp(dir);
  const started = Bun.spawnSync(
    ["bash", "-c", gate(cut), join(root, driver), ...(startOf[driver]?.args ?? [])],
    {
      env: {
        HOME: join(dir, "home"),
        USER: process.env.USER ?? "legion",
        PATH: `${bin}:/usr/bin:/bin`,
        TMPDIR: join(dir, "tmp"),
        XDG_RUNTIME_DIR: join(dir, "xdg"),
        LEGION_E2E_RUN: dir,
        LEGION_E2E_MODEL_GATEWAY_URL: "http://127.0.0.1:13378",
        LEGION_E2E_MODEL_GATEWAY_AUDIENCE: "stand-in",
        LEGION_E2E_POD_SERVICE_ACCOUNT: "stand-in",
        LEGION_E2E_RUNTIME_KUBECONFIG: join(dir, "kubeconfig"),
        SKILL_SCENARIOS_WORK: join(dir, "work"),
        STANDIN_ROOT: join(dir, "tmp"),
        STANDIN_CALLS: calls,
        STANDIN_BUN: process.execPath,
        GATE_SKIPPED: skipped,
        ...operatorInputs,
        LEGION_E2E_DISPATCH_TOKEN_FILE: bearer,
        ...startOf[driver]?.env,
        ...env,
      },
      timeout: walkBound - 10_000,
    }
  );
  return {
    exit: started.exitCode,
    pid: started.pid,
    dir,
    said: `${started.stdout.toString()}${started.stderr.toString()}`,
    log: readFileSync(calls, "utf8").split("\n"),
    // The last commands the cut skipped, for a row that fails: what came before the driver
    // stopped short of what the row waits for.
    cut: readFileSync(skipped, "utf8").trim().split("\n").slice(-4),
  };
}

const { drivers } = sweepOrder(root);

/** The functions of a driver whose calls reach the removal, a mark, or a refusal of `named`: the
 *  functions the rows' gate follows into. */
function reaching(driver: Driver): string[] {
  const on = driver.calls.filter(
    (call) =>
      call.kind !== "refusal" || call.via.some((fn) => (named as readonly string[]).includes(fn))
  );
  return [...new Set(on.flatMap((call) => call.via))];
}

describe("each driver runs the removal of the Postgres of dead stacks before each refusal, in its own order", () => {
  for (const driver of drivers) {
    for (const kind of named.filter((name) =>
      driver.calls.some((call) => call.via.includes(name))
    )) {
      test(
        `${driver.driver}: the removal runs though ${kind} refuses what a dead run left`,
        () => {
          const ran = start(
            driver.driver,
            { run: [...named, sweep], follow: reaching(driver), keepSet: true },
            {
              ...refuses[kind].env,
              ...(kind === "refuse_leftovers" ? { STANDIN_DEAD_PID: deadPid() } : {}),
            },
            // A batch killed outright leaves its lock (rig.sh's lock_services).
            (dir) =>
              kind === "lock_services" && writeFileSync(join(dir, "work", "services", "up"), "")
          );
          expect({
            exit: ran.exit,
            refused: refuses[kind].said.test(ran.said),
            removalRan: ran.log.includes("SWEEP"),
            said: ran.said.trim().split("\n").slice(-4),
            cut: ran.cut,
          }).toEqual({
            exit: 1,
            refused: true,
            removalRan: true,
            said: expect.any(Array),
            cut: expect.any(Array),
          });
        },
        walkBound
      );
    }
  }
});

// The deep audit of round 12 found two refusals that no name of `named` reaches, each before its
// driver's removal: Stage 4a's check of its two ports, which the session database a dead run left
// still publishes, and the box-login lock of Stages 2 and 3, which a pane of a dead run still
// holds. Each row runs the driver's own file through the gate from the line where that refusal's
// branch begins, which the search names, so the refusal's own words and exit run.
describe("each driver runs the removal of the Postgres of dead stacks before a refusal of a port or a lock that a dead run holds", () => {
  const driverOf = (path: string) => drivers.find((each) => each.driver === path);
  const refusalOf = (path: string, reads: RegExp) =>
    driverOf(path)?.calls.find(
      (call) => call.kind === "refusal" && call.reads.some((read) => reads.test(read))
    );
  const stage4a = "scripts/e2e/stage4a-sandbox-runtime.sh";
  const portCut = (): Cut => ({
    run: [sweep, mark],
    follow: ["docker"],
    keepSet: false,
    from: refusalOf(stage4a, /^ss$/)?.begins,
  });
  test(
    "scripts/e2e/stage4a-sandbox-runtime.sh: the removal runs though the start refuses the session database port that a dead run's container still publishes",
    () => {
      expect(portCut().from).toBeDefined();
      const ran = start(stage4a, portCut(), {
        STANDIN_TAKEN_PORT: operatorInputs.LEGION_E2E_SESSION_DB_PORT,
      });
      expect({
        refused: /port 13377 is taken on the devbox/.test(ran.said),
        removalRan: ran.log.includes("SWEEP"),
        said: ran.said.trim().split("\n").slice(-3),
      }).toEqual({ refused: true, removalRan: true, said: expect.any(Array) });
    },
    walkBound
  );
  test(
    "scripts/e2e/stage4a-sandbox-runtime.sh control: with no port taken, the same cut reaches the removal",
    () => {
      const ran = start(stage4a, portCut(), { STANDIN_STOP_AT: "first" });
      expect({
        refused: /is taken on the devbox/.test(ran.said),
        removalRan: ran.log.includes("SWEEP"),
        said: ran.said.trim().split("\n").slice(-3),
      }).toEqual({ refused: false, removalRan: true, said: expect.any(Array) });
    },
    walkBound
  );

  for (const path of [
    "scripts/e2e/stage2-tmux-supervision.sh",
    "scripts/e2e/stage3-devbox-workflow.sh",
  ]) {
    const lockCut = (): Cut => {
      const driver = driverOf(path);
      return {
        run: [...named, sweep, "take_box_login_lock"],
        follow: driver === undefined ? [] : reaching(driver),
        keepSet: true,
        from: refusalOf(path, /^flock in take_box_login_lock$/)?.begins,
      };
    };
    const lockFile = () => join(mkdtempSync(join(scratch, "lock-")), "box-login.lock");
    const boxLogin = (lock: string) => ({
      LEGION_E2E_MODEL_ROUTE: "box-login",
      box_login_lock_file: lock,
    });
    test(
      `${path}: the removal runs though the box-login lock that a process of a dead run still holds refuses the start`,
      async () => {
        expect(lockCut().from).toBeDefined();
        const lock = lockFile();
        writeFileSync(lock, "");
        // A process that holds the lock, as a pane or daemon of a dead run that kept the
        // descriptor. It prints a line once flock gave it the lock.
        const holder = Bun.spawn(["flock", lock, "sh", "-c", "echo held; exec sleep 120"], {
          stdout: "pipe",
          stderr: "ignore",
        });
        try {
          const reader = holder.stdout.getReader();
          await reader.read();
          reader.releaseLock();
          expect(Bun.spawnSync(["flock", "-n", lock, "true"]).exitCode).toBe(1);
          const ran = start(path, lockCut(), boxLogin(lock));
          expect({
            refused: /another box-login proof holds/.test(ran.said),
            removalRan: ran.log.includes("SWEEP"),
            said: ran.said.trim().split("\n").slice(-3),
          }).toEqual({ refused: true, removalRan: true, said: expect.any(Array) });
        } finally {
          holder.kill();
          await holder.exited;
        }
      },
      walkBound
    );
    test(
      `${path} control: with the box-login lock free, the same cut reaches the removal`,
      () => {
        const ran = start(path, lockCut(), { ...boxLogin(lockFile()), STANDIN_STOP_AT: "first" });
        expect({
          refused: /another box-login proof holds/.test(ran.said),
          removalRan: ran.log.includes("SWEEP"),
          said: ran.said.trim().split("\n").slice(-3),
        }).toEqual({ refused: false, removalRan: true, said: expect.any(Array) });
      },
      walkBound
    );
  }
});

// The rows of each create run the driver past its refusals to its creates: they cut the refusals
// themselves, which either pass, calling nothing, or end the start, so the cut cannot move a
// create before or after the removal; and a dead run another program of the machine left cannot
// refuse the row.
const createCut = (driver: Driver): Cut => ({
  run: [sweep, mark, "start_container"],
  follow: [...reaching(driver), "docker", "session_db_start"].filter(
    (name) => !(named as readonly string[]).includes(name)
  ),
  keepSet: false,
});

describe("each driver runs the removal of the Postgres of dead stacks before each create of its own Postgres, in its own order", () => {
  for (const driver of drivers) {
    const marks = driver.calls.filter((call) => call.name === mark);
    marks.forEach((marked, index) => {
      test(
        `${driver.driver}: the removal runs before its Postgres ${index + 1} of ${marks.length} (marked at ${marked.at}) is created, and the create carries its mark`,
        () => {
          const ran = start(driver.driver, createCut(driver), {
            STANDIN_STOP_AT: String(index + 1),
          });
          const creates = ran.log.filter((line) => line.startsWith("CREATE "));
          const created = creates[index] ?? "";
          const [, name = "", label = "{}"] = /^CREATE (\S*) (.*)$/.exec(created) ?? [];
          const marking = JSON.parse(label) as {
            owner?: { pid: number };
            run?: string;
            container?: string;
            sockets?: string;
          };
          expect({
            created: created !== "",
            removalFirst:
              ran.log.indexOf("SWEEP") >= 0 && ran.log.indexOf("SWEEP") < ran.log.indexOf(created),
            owner: marking.owner?.pid,
            underRun: realpathSync(marking.run ?? "/").startsWith(realpathSync(ran.dir)),
            container: marking.container,
            sockets: marking.sockets,
            said: ran.said.trim().split("\n").slice(-4),
            cut: ran.cut,
          }).toEqual({
            created: true,
            removalFirst: true,
            owner: ran.pid,
            underRun: true,
            container: name,
            sockets: undefined,
            said: expect.any(Array),
            cut: expect.any(Array),
          });
        },
        walkBound
      );
    });

    // A start that uses a Postgres it was given (CI's service) creates none, and still removes the
    // Postgres of dead stacks first, before any container of its own.
    if (readFileSync(join(root, driver.driver), "utf8").includes("LEGION_E2E_PG_DSN")) {
      test(
        `${driver.driver}: with LEGION_E2E_PG_DSN set, the removal runs before any container of the start`,
        () => {
          const ran = start(driver.driver, createCut(driver), {
            LEGION_E2E_PG_DSN: "postgres://stand-in@127.0.0.1:1/stand-in",
            STANDIN_STOP_AT: "first",
          });
          const first = ran.log.find((line) => /^(SWEEP|RUN |CREATE )/.test(line));
          expect({ first, said: ran.said.trim().split("\n").slice(-4), cut: ran.cut }).toEqual({
            first: "SWEEP",
            said: expect.any(Array),
            cut: expect.any(Array),
          });
        },
        walkBound
      );
    }
  }
});
