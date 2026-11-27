/** A setting an e2e script gives a service must reach it through the environment, never through
 *  any process's arguments: /proc/<pid>/cmdline is world-readable for as long as the process runs
 *  (`ps` prints it), and /proc/<pid>/environ is its own user's and root's. A start written
 *  `start_process NAME env -i DATABASE_URL=… server` puts the URL's password in env's arguments
 *  until env execs, and one written `start_held … -- env -i DATABASE_URL=… server` puts it in
 *  held-port.py's python3 arguments too, for the whole of its listen, port write and exec.
 *
 *  Two rows. The first drives both shapes with a sentinel password through lib/rig.sh and reads
 *  the chain's argument vectors for it: `start_with_settings`, and `start_held` with `--setting`,
 *  against the same start written the old way, which is the control that the reader sees a leak
 *  when there is one. The second reads every e2e script for a start that still writes a secret in
 *  its arguments and names the file and line of each.
 *
 *  What the first row reads, and so what it can see. A `python3` and an `env` first on the
 *  driver's PATH each append their own /proc/self/cmdline to a file and then exec the real program
 *  (`exec -a`), so the real helper and the real env still run and what each was given is kept; and
 *  the service's own /proc/<pid>/cmdline, and that of every process under it, is read while it
 *  runs. That is every program these starts run, but not every program a start could run: a
 *  start that handed its settings to some other program's arguments, a long-lived `bash -c`
 *  among them, would leak where this row does not look. The second row does not see it either:
 *  it reads the starts it knows by name, the rig's wrappers, `held-port.py exec` and `env -i`.
 */

import { describe, expect, test } from "bun:test";
import { execFileSync, spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { libRowBoundMs, rowWaits } from "./row-waits";
import { controlPlantMark, sourcedLibrariesIn, unreadableSources } from "./script-functions";

rowWaits(libRowBoundMs);

const repoRoot = join(import.meta.dir, "..", "..", "..");
/** The password no setting of this repository holds, so a hit is this row's own leak. */
const sentinel = "not-a-secret-7Qp2xV9mKd4LsTw8";

/** A program first on PATH that appends its own arguments to `record` and then execs the real
 *  `name`, so the chain runs as it would and what each step was given is kept. */
function recorder(bin: string, name: string, real: string, record: string): void {
  const path = join(bin, name);
  writeFileSync(
    path,
    // `cat /proc/self/cmdline` reads cat's own, so the shell reads its own by pid.
    `#!/bin/bash\ncat "/proc/$$/cmdline" >>${JSON.stringify(record)}\nprintf '\\n' >>${JSON.stringify(record)}\nexec -a "$0" ${JSON.stringify(real)} "$@"\n`
  );
  chmodSync(path, 0o755);
}

/** Every argument vector of `pid` and of each process under it, as /proc holds them now. */
function cmdlines(pid: number): string[] {
  const of = (each: number): string => {
    try {
      return readFileSync(`/proc/${each}/cmdline`, "utf8").replaceAll("\0", " ");
    } catch {
      return "";
    }
  };
  const children = (each: number): number[] => {
    try {
      return readdirSync(`/proc/${each}/task`)
        .flatMap((task) => readFileSync(`/proc/${each}/task/${task}/children`, "utf8").split(/\s+/))
        .filter((word) => word !== "")
        .map(Number);
    } catch {
      return [];
    }
  };
  const seen: string[] = [];
  const walk = (each: number): void => {
    seen.push(of(each));
    for (const child of children(each)) walk(child);
  };
  walk(pid);
  return seen;
}

/** Runs `script` under bash in a directory of its own and answers what the chain was given: every
 *  recorded argument vector, every live one of the service and its children, and the driver's own
 *  output. The driver writes the service's pid to `pid`. */
function drive(label: string, body: string): { argv: string[]; out: string } {
  const dir = mkdtempSync(join(tmpdir(), `start-settings-${label}-`));
  const bin = join(dir, "bin");
  const record = join(dir, "record");
  mkdirSync(bin, { recursive: true });
  mkdirSync(join(dir, "evidence", "logs"), { recursive: true });
  writeFileSync(record, "");
  for (const name of ["python3", "env"] as const) {
    const real = execFileSync("sh", ["-c", `command -v ${name}`], { encoding: "utf8" }).trim();
    recorder(bin, name, real, record);
  }
  const driver = join(dir, "driver.sh");
  writeFileSync(
    driver,
    `#!/usr/bin/env bash
set -euo pipefail
root=${JSON.stringify(repoRoot)}
work=${JSON.stringify(dir)}
evidence=${JSON.stringify(join(dir, "evidence"))}
timeout_hook=""
note() { printf 'note: %s\\n' "$*" >&2; }
fail() { printf 'fail: %s\\n' "$*" >&2; exit 1; }
# shellcheck source=/dev/null
. "$root/scripts/e2e/lib/rig.sh"
sentinel=${JSON.stringify(sentinel)}
database_url="postgres://dispatch:$sentinel@127.0.0.1:5999/dispatch"
# The service: it prints its pid and sleeps, so its arguments and its children's can be read while
# it runs. It is given the descriptor a held start hands over and never binds a port itself.
service="$work/service.sh"
cat >"$service" <<'SERVICE'
#!/usr/bin/env bash
printf '%s\\n' "$$" >"$1"
exec sleep 600
SERVICE
chmod 755 "$service"
${body}
`
  );
  chmodSync(driver, 0o755);
  const ran = spawnSync("bash", [driver], {
    cwd: dir,
    encoding: "utf8",
    env: { HOME: dir, PATH: `${bin}:${process.env.PATH ?? ""}`, TMPDIR: dir },
    timeout: 120_000,
  });
  let out = `${ran.stdout ?? ""}${ran.stderr ?? ""}`;
  try {
    out += `service log: ${readFileSync(join(dir, "evidence", "logs", "service.log"), "utf8")}`;
  } catch {
    out += "service log: none";
  }
  let pid = 0;
  try {
    pid = Number(readFileSync(join(dir, "pid"), "utf8").trim());
  } catch {
    // The driver failed before the service wrote its pid; `out` says why.
  }
  const live = Number.isFinite(pid) && pid > 0 ? cmdlines(pid) : [];
  const recorded = readFileSync(record, "utf8")
    .split("\n")
    .map((line) => line.replaceAll("\0", " "))
    .filter((line) => line !== "");
  try {
    if (pid > 0) process.kill(pid, "SIGKILL");
  } catch {
    // It ended on its own; nothing to stop.
  }
  // The keeper of a held start outlives the driver, since it holds the port for the whole run.
  spawnSync("pkill", ["-f", dir], { timeout: 10_000 });
  rmSync(dir, { force: true, recursive: true });
  return { argv: [...recorded, ...live], out };
}

describe.skipIf(skipWithout("bash", "python3", "env", "sleep"))(
  "a setting an e2e script gives a service is in no process's arguments",
  () => {
    // The old shape of a plain start, which is the control: `env -i NAME=value… command` puts each
    // value in env's own arguments, where the reader must find the sentinel. Without this the
    // row's clean readings below would say nothing about the reader.
    test("control: a start that writes its settings as arguments leaks them, and the reader sees it", () => {
      const { argv, out } = drive(
        "control",
        `start_process service env -i PATH="$PATH" DATABASE_URL="$database_url" "$service" "$work/pid"
until_true 30 "the service to write its pid" test -s "$work/pid"`
      );
      expect({ leaked: argv.some((line) => line.includes(sentinel)), out }).toMatchObject({
        leaked: true,
      });
    });

    test("a plain start gives its settings through the environment alone", () => {
      const { argv, out } = drive(
        "plain",
        `start_with_settings service PATH="$PATH" DATABASE_URL="$database_url" -- "$service" "$work/pid"
until_true 30 "the service to write its pid" test -s "$work/pid"`
      );
      expect({
        leaked: argv.filter((line) => line.includes(sentinel)),
        read: argv.length > 0,
        out,
      }).toMatchObject({ leaked: [], read: true });
    });

    // The held shape, through the real held-port.py: the helper holds the port, writes it and
    // execs the service, and the setting is its environment, named to it alone (--pass).
    test("a held start gives its settings through the environment alone, naming them to held-port.py", () => {
      const { argv, out } = drive(
        "held",
        `start_held service service_port LISTEN_FD --setting PATH="$PATH" \\
  --setting DATABASE_URL="$database_url" -- "$service" "$work/pid"
until_true 30 "the service to write its pid" test -s "$work/pid"`
      );
      const helper = argv.filter((line) => line.includes("held-port.py"));
      expect({
        leaked: argv.filter((line) => line.includes(sentinel)),
        namedToTheHelper: helper.some((line) => line.includes("--pass DATABASE_URL")),
        out,
      }).toMatchObject({ leaked: [], namedToTheHelper: true });
    });

    // The control for the held shape: the same start written the old way, which the reader must
    // find in held-port.py's own arguments, where it stays until the helper execs.
    test("control: a held start that writes its settings after -- leaks them into held-port.py's arguments", () => {
      const { argv, out } = drive(
        "held-control",
        `start_held service service_port LISTEN_FD -- \\
  env -i PATH="$PATH" LISTEN_FD=3 DATABASE_URL="$database_url" "$service" "$work/pid"
until_true 30 "the service to write its pid" test -s "$work/pid"`
      );
      const helper = argv.filter((line) => line.includes("held-port.py"));
      expect({ leaked: helper.some((line) => line.includes(sentinel)), out }).toMatchObject({
        leaked: true,
      });
    });
  }
);

/** A start that hands a command its settings: the rig's wrappers, held-port.py's exec, or `env`
 *  clearing the environment. Each gives them as arguments unless they are --setting or --pass. */
const startsACommand =
  /(?<![\w-])(?:start_held|start_kept|start_process)\s|held-port\.py["']?\s+exec\b|\$\{?(?:HELD_PORT|held_port)\}?["']?\s+exec\b|(?<![\w-])env\s+-i\b/;
/** Every setting written as an argument, with the flag before it where there is one: --setting
 *  and --pass give the name alone, so the value is in no argument and this scan passes them. */
const assignment = /(?:^|\s)(--env|--setting|--pass)?\s*["']?([A-Z][A-Z0-9_]*)=/g;
/** A name whose value is a credential: a URL carrying a password, a bearer, a key or a seed. A
 *  name ending in _FILE is a path to one, which is why the scripts hand those. */
const secretName = /(?:DATABASE_URL|DSN|TOKEN|PASSWORD|SECRET|PEM|PEM_B64|SEED|_KEY)$/;

/** The secret-named settings one logical line gives a command as its arguments, in order. */
function secretArgumentNames(text: string): string[] {
  if (text.trimStart().startsWith("#")) return [];
  const match = startsACommand.exec(text);
  if (match === null) return [];
  const after = text.slice(match.index + match[0].length);
  const names = [...after.matchAll(assignment)]
    .filter((hit) => hit[1] !== "--setting" && hit[1] !== "--pass")
    .map((hit) => hit[2] ?? "")
    .filter((name) => secretName.test(name));
  return [...new Set(names)];
}

/** The logical line starting at `lines[from]`: its backslash continuations joined, and, where it
 *  opens a settings array (`name=(`), every line to the one that closes it, so an array written
 *  over several lines is read as the one command it stands for. Answers the text and the index of
 *  its last line. */
function logicalLine(lines: string[], from: number): { text: string; last: number } {
  let text = lines[from] ?? "";
  let index = from;
  const open = (of: string): number =>
    (of.match(/\(/g) ?? []).length - (of.match(/\)/g) ?? []).length;
  const array = /^\s*[A-Za-z_][A-Za-z0-9_]*(?:\+)?=\(/.test(text);
  while (index + 1 < lines.length) {
    if (text.endsWith("\\")) {
      index++;
      text = `${text.slice(0, -1)} ${lines[index]}`;
      continue;
    }
    if (array && open(text) > 0) {
      index++;
      text = `${text} ${lines[index]}`;
      continue;
    }
    break;
  }
  return { text, last: index };
}

/** Every script of `tree`'s scripts/e2e, and every library one of them sources, wherever in the
 *  tree that reaches: a start that leaks lives as readily in a sourced library as in the script,
 *  so the scan reads what the scripts reach and not only the scripts. The tree is the checkout
 *  unless a control names one of its own. */
function scanned(tree: string = repoRoot): string[] {
  // The files that are there, not the tracked ones: a script added and not yet committed runs
  // like any other, so the scan reads it too.
  const scripts = [join(tree, "scripts", "e2e"), join(tree, "scripts", "e2e", "lib")]
    .flatMap((dir) => readdirSync(dir).map((name) => join(dir, name)))
    .filter((path) => path.endsWith(".sh"));
  return sourcedLibrariesIn(tree, scripts);
}

/** Every line of a scanned file that sources a library the scan cannot name a file of the tree
 *  for, as `file:line: the line`. Such a file is never read, so a start that leaks inside it is
 *  unseen: the scan refuses these rather than passing over them. A variable in place of a path,
 *  or a directory only the running shell knows, is the shape that would quiet the scan, so it is
 *  a refusal and not a skip. */
function unreadable(tree: string = repoRoot): string[] {
  return scanned(tree).flatMap((path) => unreadableSources(path, tree));
}

/** Every start among the scanned files whose own arguments carry a setting named for a secret,
 *  as `file:line: NAMES`. */
function startsWithSecretArguments(tree: string = repoRoot): string[] {
  const found: string[] = [];
  for (const path of scanned(tree)) {
    const name = relative(tree, path);
    const lines = readFileSync(path, "utf8").split("\n");
    for (let index = 0; index < lines.length; index++) {
      const first = index;
      const { text, last } = logicalLine(lines, index);
      index = last;
      const names = secretArgumentNames(text);
      if (names.length > 0) found.push(`${name}:${first + 1}: ${names.join(" ")}`);
    }
  }
  return found;
}

/** A fresh tree for each control, laid out as the checkout is. A control written in the checkout
 *  is read by every scan of it that runs beside it on another worker, as a script of the tree: the
 *  rows above would report its planted leak or its refused source as real ones, or fail on a file
 *  removed between their listing and their read. A name of this run alone does not keep a
 *  directory scan from reading it. */
const controlTree = temporaryDirectories("start-settings-scan");

/** Writes `files`, each a path under a fresh tree and the text of a script, with the mark every
 *  plant carries, and runs `body` on that tree. */
function inTree(
  files: readonly { readonly path: string; readonly text: string }[],
  body: (tree: string) => void
): void {
  const tree = controlTree();
  mkdirSync(join(tree, "scripts", "e2e", "lib"), { recursive: true });
  for (const { path, text } of files) {
    mkdirSync(dirname(join(tree, path)), { recursive: true });
    writeFileSync(join(tree, path), `#!/usr/bin/env bash\n${text}\n${controlPlantMark}\n`);
  }
  body(tree);
}

describe("every start of an e2e script keeps its secrets out of its arguments", () => {
  // A scan that enumerated nothing answers "no start leaks" exactly as a clean tree does, so what
  // it read is checked before what it found: the files it must have reached, by name, and a floor
  // under the count. Without this a wrong directory, a filter that selects nothing or a walk that
  // ends at the first file all read green.
  test("the scan reads the scripts and the libraries they reach", () => {
    const read = scanned().map((path) => relative(repoRoot, path));
    for (const name of [
      "scripts/e2e/lib/rig.sh",
      "scripts/e2e/lib/loopback-dsn.sh",
      "scripts/e2e/dispatch-session-settings.sh",
      "packages/envoy/scripts/lib/exec-env.sh",
      "packages/pi-envoy/scripts/role-takeover/clients.sh",
    ]) {
      expect({ name, read: read.includes(name) }).toEqual({ name, read: true });
    }
    expect(read.length).toBeGreaterThan(80);
  });

  // Each control writes its scripts in a tree of its own and marks every one, so a marked file in
  // the checkout is a plant: one a killed run left behind, or one a later test wrote there. It is
  // named here as a plant, rather than read as a script of the tree whose start or source the rows
  // below then report as real.
  test("no file the scan reads in the checkout is a test's planted control", () => {
    expect(
      scanned()
        .filter((path) => readFileSync(path, "utf8").includes(controlPlantMark))
        .map((path) => relative(repoRoot, path))
    ).toEqual([]);
  });

  test("no start writes a secret-named setting in the arguments it gives", () => {
    expect(startsWithSecretArguments()).toEqual([]);
  });

  test("every library a script of scripts/e2e sources can be read", () => {
    expect(unreadable()).toEqual([]);
  });

  // The first direction: a library sourced by a path the scan resolves is read, so a start that
  // leaks inside it is named. The library is the one #4296 sources, written as it will be there.
  test("a start in a sourced library of another package is scanned and named", () => {
    inTree(
      [
        {
          path: "packages/envoy/scripts/lib/pg-url.sh",
          text: 'start_process dispatch env -i PATH="$PATH" DATABASE_URL="$url" "$bin"',
        },
        {
          path: "scripts/e2e/sourced-scan.sh",
          text: '. "$root/packages/envoy/scripts/lib/pg-url.sh"',
        },
      ],
      (tree) => {
        expect(startsWithSecretArguments(tree)).toEqual([
          "packages/envoy/scripts/lib/pg-url.sh:2: DATABASE_URL",
        ]);
      }
    );
  });

  // The second direction, and the one that would have caught a workaround: a library sourced
  // through a variable is a refusal naming the file and line, never a silent skip. The leak
  // planted behind it is unseen, which is why the line itself must fail.
  test("a library sourced through a variable fails the scan by name, rather than being skipped", () => {
    inTree(
      [
        {
          path: "packages/envoy/scripts/lib/pg-url.sh",
          text: 'start_process dispatch env -i PATH="$PATH" DATABASE_URL="$url" "$bin"',
        },
        {
          path: "scripts/e2e/sourced-scan.sh",
          text: 'pg_url_lib=$root/packages/envoy/scripts/lib/pg-url.sh\n. "$pg_url_lib"',
        },
      ],
      (tree) => {
        expect(unreadable(tree)).toEqual(['scripts/e2e/sourced-scan.sh:3: . "$pg_url_lib"']);
        expect(startsWithSecretArguments(tree)).toEqual([]);
      }
    );
  });

  // A path that reaches a directory is not a file the scan read. Taken for one, the walk queued
  // the directory and its read threw, so each of the scan's rows failed on that throw and none
  // named the line. It is refused by name, and the scan reads on past it.
  test("a library sourced as a directory is refused by name, and the scan reads on", () => {
    inTree(
      [
        {
          path: "packages/envoy/scripts/lib/pg-url.sh",
          text: 'start_process dispatch env -i PATH="$PATH" DATABASE_URL="$url" "$bin"',
        },
        {
          path: "scripts/e2e/sourced-scan.sh",
          text: '. "$root/packages/envoy/scripts/lib"\n. "$root/packages/envoy/scripts/lib/pg-url.sh"',
        },
      ],
      (tree) => {
        expect(
          scanned(tree)
            .map((path) => relative(tree, path))
            .sort()
        ).toEqual(["packages/envoy/scripts/lib/pg-url.sh", "scripts/e2e/sourced-scan.sh"]);
        expect(unreadable(tree)).toEqual([
          'scripts/e2e/sourced-scan.sh:2: . "$root/packages/envoy/scripts/lib"',
        ]);
        expect(startsWithSecretArguments(tree)).toEqual([
          "packages/envoy/scripts/lib/pg-url.sh:2: DATABASE_URL",
        ]);
      }
    );
  });

  // The scan reads what it is pointed at: each shape it must find, and the three it must leave
  // alone, through the same function the scan uses.
  test("control: the scan finds each shape that writes one, and reads the rest as none", () => {
    expect(
      [
        'start_process dispatch env -i PATH="$PATH" DATABASE_URL="$url" "$bin"',
        'start_held dispatch port DISPATCH_LISTEN_FD -- env -i DISPATCH_AGENT_TOKEN="$t" "$bin"',
        'start_kept dispatch keeper DISPATCH_LISTEN_FD -- env -i DISPATCH_SIGNING_KEY="$k" "$bin"',
        'python3 "$held_port" exec --fd-env LISTEN_FD -- "$settings" ANTHROPIC_API_KEY="$k" "$b"',
        'env -i PATH="$PATH" DISPATCH_TOKEN="$(cat "$f")" "$cli"',
      ].map((line) => secretArgumentNames(line))
    ).toEqual([
      ["DATABASE_URL"],
      ["DISPATCH_AGENT_TOKEN"],
      ["DISPATCH_SIGNING_KEY"],
      ["ANTHROPIC_API_KEY"],
      ["DISPATCH_TOKEN"],
    ]);
    expect(
      [
        // The names alone, which is what the fix gives held-port.py.
        'start_held dispatch port DISPATCH_LISTEN_FD --setting DATABASE_URL="$url" -- "$bin"',
        // A path to a credential, not the credential.
        'env -i PATH="$PATH" ENVOY_TOKEN_FILE="$work/envoy-token" "$bin"',
        // A comment naming the shape.
        '# start_process dispatch env -i DATABASE_URL="$url" "$bin"',
      ].map((line) => secretArgumentNames(line))
    ).toEqual([[], [], []]);
  });
});
