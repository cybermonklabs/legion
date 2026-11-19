import { afterEach, expect, test } from "bun:test";
import { realpathSync } from "node:fs";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { ownGroup, untilRunning } from "./own-group";

// ownGroup (lib/own-group.ts) starts a row's process as the leader of a group of its own and hands
// it nothing else: the same signal dispositions, signal mask and environment as a command the runner
// starts itself. untilRunning waits until a pid runs a program, and fails at its bound naming both.
const skip = skipWithout("perl", "bash", "sleep", "true");

const started: number[] = [];
afterEach(() => {
  for (const pid of started.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});

// The shell's view of what it was handed: its traps on SIGPIPE and SIGXFSZ (`trap -p` names an
// ignored signal as `trap -- ''`), LC_CTYPE, its blocked and ignored signals, a digest of its whole
// environment, whether it leads its own group, and its parent. The signals are read by a perl the
// shell starts, which inherits both through its exec: the blocked set through sigprocmask and the
// ignored ones as %SIG's IGNORE. /proc/<pid>/status is no source, since a Legion pod's kernel
// (4.19.0-gvisor) writes no Sig* line there, and the row would compare nothing.
const signals = `use Config; use POSIX ();
my $set = POSIX::SigSet->new;
POSIX::sigprocmask(POSIX::SIG_BLOCK(), undef, $set) or die "sigprocmask: $!\\n";
my @num = split " ", $Config{sig_num};
my %seen;
my @blocked = grep { $_ > 0 && $set->ismember($_) && !$seen{$_}++ } @num;
print "blocked ", join(",", sort { $a <=> $b } @blocked), "\\n";
print "ignored ", join(",", grep { ($SIG{$_} // "") eq "IGNORE" } sort keys %SIG), "\\n";`;
const probe = `trap -p PIPE; trap -p XFSZ; printf 'LC_CTYPE=%s\\n' "\${LC_CTYPE-unset}"
perl -e "$1"; printf 'environment %s\\n' "$(env | sort | cksum)"
read -r s </proc/$$/stat; s=\${s##*) }; set -- $s
printf 'leads its group %s\\nparent %s\\n' "$([ "$3" = $$ ] && echo yes || echo no)" "$2"`;

test.skipIf(skip)(
  "ownGroup's process leads a group of its own, with the runner as its parent, and gets the dispositions and the environment a plain start gets",
  () => {
    const env = { PATH: process.env.PATH ?? "" };
    const plain = Bun.spawnSync(["bash", "-c", probe, "probe", signals], { env });
    const wrapped = Bun.spawnSync(ownGroup(["bash", "-c", probe, "probe", signals]), { env });
    expect(plain.stderr.toString()).toBe("");
    expect(wrapped.stderr.toString()).toBe("");
    const lines = (out: Buffer) => out.toString().trimEnd().split("\n");
    const [plainOut, wrappedOut] = [lines(plain.stdout), lines(wrapped.stdout)];
    expect(wrappedOut.slice(-2)).toEqual(["leads its group yes", `parent ${process.pid}`]);
    expect(plainOut.slice(-2)).toEqual(["leads its group no", `parent ${process.pid}`]);
    // Both read the signals: a row that read nothing would compare nothing.
    for (const out of [plainOut, wrappedOut]) {
      expect(out.filter((line) => /^blocked [0-9,]*$/.test(line))).toHaveLength(1);
      expect(out.filter((line) => /^ignored [A-Z0-9,]*$/.test(line))).toHaveLength(1);
    }
    // Everything else the same, line for line: no trap on SIGPIPE or SIGXFSZ that the plain start
    // has not, no LC_CTYPE it was not given, the same blocked and ignored signals and the same
    // environment.
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

test.skipIf(skip)(
  "untilRunning fails at its bound, naming the pid, the program and what the pid runs, when the pid never runs the program",
  async () => {
    const child = Bun.spawn(ownGroup(["sleep", "60"]), { stdout: "ignore", stderr: "ignore" });
    started.push(child.pid);
    const want = realpathSync(hostTool("true") ?? "true");
    const sleep = realpathSync(hostTool("sleep") ?? "sleep");
    const began = Date.now();
    let failure = "";
    try {
      await untilRunning(child.pid, "true");
    } catch (error) {
      failure = (error as Error).message;
    }
    expect(failure).toBe(`pid ${child.pid} did not run ${want} within 10 s (running ${sleep})`);
    expect(Date.now() - began).toBeGreaterThanOrEqual(10_000);
  },
  60_000
);
