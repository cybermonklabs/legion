import { afterEach, expect, test } from "bun:test";
import { realpathSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup, untilRunning } from "./own-group";

const skip = skipWithout("perl", "bash", "sleep", "true");

const started: number[] = [];
const work = temporaryDirectory("own-group-test");
afterEach(() => {
  for (const pid of started.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});

// The shell's view of what it was handed: its traps on SIGPIPE and SIGXFSZ (`trap -p` names an
// ignored signal as `trap -- ''`), LC_CTYPE, its blocked and ignored signals, a digest of its whole
// environment, whether it leads its own group, and its parent. The ignored signals are the shell's
// own `trap -p` of each signal from 1 to 64; the blocked set is read by a perl the shell starts,
// which inherits it through its exec (sigprocmask): perl itself ignores SIGFPE when it starts, so
// its %SIG cannot tell an ignored SIGFPE it was handed. /proc/<pid>/status is no source, since a
// Legion pod's kernel (4.19.0-gvisor) writes no Sig* line there, and the row would compare nothing.
const blocked = `use Config; use POSIX ();
my $set = POSIX::SigSet->new;
POSIX::sigprocmask(POSIX::SIG_BLOCK(), undef, $set) or die "sigprocmask: $!\\n";
my @num = split " ", $Config{sig_num};
my %seen;
my @blocked = grep { $_ > 0 && $set->ismember($_) && !$seen{$_}++ } @num;
print "blocked ", join(",", sort { $a <=> $b } @blocked), "\\n";`;
const probe = `trap -p PIPE; trap -p XFSZ; printf 'LC_CTYPE=%s\\n' "\${LC_CTYPE-unset}"
ignored=()
for s in $(seq 1 64); do case $(trap -p "$s" 2>/dev/null) in "trap -- '' "*) ignored+=("$s") ;; esac; done
printf 'ignored %s\\n' "\${ignored[*]-}"
perl -e "$1"; printf 'environment %s\\n' "$(env | sort | cksum)"
read -r s </proc/$$/stat; s=\${s##*) }; set -- $s
printf 'leads its group %s\\nparent %s\\n' "$([ "$3" = $$ ] && echo yes || echo no)" "$2"`;

test.skipIf(skip)(
  "ownGroup's process leads a group of its own, with the runner as its parent, and gets the dispositions and the environment a plain start gets",
  () => {
    const env = { PATH: process.env.PATH ?? "" };
    const plain = Bun.spawnSync(["bash", "-c", probe, "probe", blocked], { env });
    const wrapped = Bun.spawnSync(ownGroup(["bash", "-c", probe, "probe", blocked]), { env });
    expect(plain.stderr.toString()).toBe("");
    expect(wrapped.stderr.toString()).toBe("");
    const lines = (out: Buffer) => out.toString().trimEnd().split("\n");
    const [plainOut, wrappedOut] = [lines(plain.stdout), lines(wrapped.stdout)];
    expect(wrappedOut.slice(-2)).toEqual(["leads its group yes", `parent ${process.pid}`]);
    expect(plainOut.slice(-2)).toEqual(["leads its group no", `parent ${process.pid}`]);
    for (const out of [plainOut, wrappedOut]) {
      expect(out.filter((line) => /^blocked [0-9,]*$/.test(line))).toHaveLength(1);
      expect(out.filter((line) => /^ignored [0-9 ]*$/.test(line))).toHaveLength(1);
    }
    expect(wrappedOut.slice(0, -2)).toEqual(plainOut.slice(0, -2));
    expect(wrappedOut).not.toContain("trap -- '' SIGPIPE");
    expect(wrappedOut).toContain("LC_CTYPE=unset");
  }
);

test.skipIf(skip)("untilRunning resolves once the pid runs the program", async () => {
  const child = Bun.spawn(ownGroup(["sleep", "60"]), { stdout: "ignore", stderr: "ignore" });
  started.push(child.pid);
  await untilRunning(child.pid, "sleep");
  expect(realpathSync(`/proc/${child.pid}/exe`)).toBe(realpathSync(hostTool("sleep") ?? "sleep"));
});

// A wait that never ends fails at its row's deadline, naming the pid, the program and what the pid
// runs: the row runs in a `bun test` of its own under a guard of 2400 ms, as fake-clock.test.ts's
// row of test-fixtures does, with the row-deadline preload, so the deadline is 1000 ms (the least
// margin) before that guard. The verdict is what the child printed: none reads a clock.
test.skipIf(skip)(
  "untilRunning fails at the row's deadline, naming the pid, the program and what the pid runs, when the pid never runs the program",
  () => {
    const want = realpathSync(hostTool("true") ?? "true");
    const sleep = realpathSync(hostTool("sleep") ?? "sleep");
    const stuck = join(work, "stuck.test.ts");
    writeFileSync(
      stuck,
      `import { test } from "bun:test";
import { ownGroup, untilRunning } from ${JSON.stringify(join(import.meta.dir, "own-group.ts"))};
test("a pid that never runs the program", async () => {
  const child = Bun.spawn(ownGroup(["sleep", "60"]), { stdout: "ignore", stderr: "ignore" });
  try {
    await untilRunning(child.pid, "true");
  } finally {
    child.kill("SIGKILL");
  }
});
`
    );
    const run = Bun.spawnSync(
      [
        process.execPath,
        "test",
        "--timeout=2400",
        "--preload",
        Bun.resolveSync("@legion/test-fixtures/row-deadline", import.meta.dir),
        "./stuck.test.ts",
      ],
      { cwd: work, env: process.env }
    );
    const output = `${run.stdout.toString()}${run.stderr.toString()}`;
    expect(output).toMatch(
      new RegExp(
        `until gave up waiting for pid \\d+ to run ${want.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}, 1000 ms before the row's guard of 2400 ms [^\\n]*\\(running ${sleep.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\)`
      )
    );
    expect(output).not.toContain("timed out");
    expect(run.exitCode).toBe(1);
  }
);
