import { afterEach, expect, test } from "bun:test";
import { realpathSync } from "node:fs";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { ownGroup, untilRunning } from "./own-group";

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
