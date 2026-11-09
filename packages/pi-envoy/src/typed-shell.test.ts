import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { isShellCommandReport } from "@legion/contracts";
import { procmgr } from "@oh-my-pi/pi-utils";
import {
  runShellCommand,
  SHELL_OUTPUT_MAX,
  shellCommandBusyReport,
  shellCommandFrom,
  shellCommandMessage,
  shellCommandReport,
  shellCommandShutdownReport,
} from "./typed-shell";

/** A directory of this machine's own, so a command that prints its working directory proves the
 *  session's is the one it ran in. */
function scratch(): string {
  return mkdtempSync(join(tmpdir(), "typed-shell-test-"));
}

/** How long a row waits for something of this machine's own: a file a command wrote, a process
 *  of its group leaving the process table. */
const DEADLINE_MS = 20_000;

/** Polls `ready` until it holds or the deadline passes, and answers what it last read. The rows
 *  that use it watch a real child process and a real process group, which no fake clock drives,
 *  so they wait on the thing itself rather than on a delay guessed to cover it. */
async function untilTrue(ready: () => boolean): Promise<boolean> {
  const until = Date.now() + DEADLINE_MS;
  while (!ready() && Date.now() < until) await Bun.sleep(10);
  return ready();
}

/** Whether `pid` is still in this machine's process table. */
function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

describe("the command a person's message says to run", () => {
  test("is the rest of a line whose first character is the mark, trimmed as the CLI trims it", () => {
    expect(shellCommandFrom("!printf hi")).toBe("printf hi");
    expect(shellCommandFrom("!!printf hi")).toBe("!printf hi");
    expect(shellCommandFrom("!   printf hi  ")).toBe("printf hi");
    expect(shellCommandFrom("!cd /tmp\nls")).toBe("cd /tmp\nls");
  });

  test("is nothing for a message that is no command, a bare mark included", () => {
    expect(shellCommandFrom("printf hi")).toBeUndefined();
    expect(shellCommandFrom(" !printf hi")).toBeUndefined();
    expect(shellCommandFrom("Where is the dashboard?")).toBeUndefined();
    expect(shellCommandFrom("")).toBeUndefined();
    // A mark with nothing after it leaves no command at all, so it is the person's prompt and
    // starts no empty shell.
    expect(shellCommandFrom("!")).toBeUndefined();
    expect(shellCommandFrom("!   ")).toBeUndefined();
    expect(shellCommandFrom("!\n")).toBeUndefined();
  });
});

describe("running a person's command", () => {
  // The host builds its shell configuration once and keeps it until it refreshes it
  // (`procmgr.refreshShellConfigCache()`, which `extensions/envoy.ts` calls at session start), so
  // a row that changes this process's environment must drop what was built before it, and leave
  // nothing of its own behind for the next row or the next file.
  beforeEach(() => {
    procmgr.__resetShellConfigCacheForTests();
  });
  afterEach(() => {
    procmgr.__resetShellConfigCacheForTests();
  });

  test("answers with what it printed, and names no status for one that exited 0", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("printf hi", { cwd: directory, limitMs: 10_000 });
      expect(result).toEqual({ cut: false, output: "hi", status: undefined });
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("runs in the directory it was given", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("pwd -P", { cwd: directory, limitMs: 10_000 });
      expect(result.output).toBe(directory);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("runs with the session's own environment", async () => {
    const directory = scratch();
    process.env.TYPED_SHELL_TEST_VARIABLE = "from the session";
    try {
      const result = await runShellCommand('printf %s "$TYPED_SHELL_TEST_VARIABLE"', {
        cwd: directory,
        limitMs: 10_000,
      });
      expect(result.output).toBe("from the session");
    } finally {
      delete process.env.TYPED_SHELL_TEST_VARIABLE;
      rmSync(directory, { recursive: true, force: true });
    }
  });

  // The command runs under the shell the host resolved, which this module takes from the host's
  // own configuration rather than resolving again: `SHELL` when it names a shell this machine
  // can run, and not the `/bin/sh` this module used to start. The shell the command really ran
  // under is the one it reads back out of its own `SHELL`, which that configuration sets to it.
  test("starts the shell the host's own configuration resolved", async () => {
    const directory = scratch();
    const named = Bun.which("bash") ?? Bun.which("zsh");
    if (named === null) throw new Error("this machine has neither a bash nor a zsh to name");
    const before = process.env.SHELL;
    process.env.SHELL = named;
    try {
      expect(procmgr.getShellConfig().shell).toBe(named);
      const result = await runShellCommand('printf %s "$SHELL"', {
        cwd: directory,
        limitMs: 10_000,
      });
      expect(result.output).toBe(named);
    } finally {
      if (before === undefined) delete process.env.SHELL;
      else process.env.SHELL = before;
      rmSync(directory, { recursive: true, force: true });
    }
  });

  // No login shell, which the host's own args would start (`-l -c`): a login shell reads the
  // profile of the HOME it runs with, and a profile that prints a banner and runs a `cd` of its
  // own would answer the person in place of their command and move it out of the session's
  // directory. bash falls back to `.profile` with no `.bash_profile` beside it, dash reads
  // `.profile`, and zsh reads `.zprofile`, so a profile under each name covers the shells this
  // machine resolves.
  test("starts no login shell, so a profile answers nothing in the command's place", async () => {
    const directory = scratch();
    const home = scratch();
    const profile = `printf 'banner\\n'\ncd ${home}\nTYPED_SHELL_TEST_PROFILE=read\n`;
    await Bun.write(join(home, ".profile"), profile);
    await Bun.write(join(home, ".zprofile"), profile);
    const before = process.env.HOME;
    process.env.HOME = home;
    try {
      const result = await runShellCommand('pwd -P; printf %s "$TYPED_SHELL_TEST_PROFILE"', {
        cwd: directory,
        limitMs: 10_000,
      });
      expect(result.output).toBe(realpathSync(directory));
    } finally {
      if (before === undefined) delete process.env.HOME;
      else process.env.HOME = before;
      rmSync(home, { recursive: true, force: true });
      rmSync(directory, { recursive: true, force: true });
    }
  });

  // The environment is the host's own for a child shell, not this process's: the host drops the
  // git location variables from it (`filterChildShellEnv`), because forwarding them makes `git`
  // ignore the command's `cwd` and write to the worktree or index they name. A session launched
  // under a `GIT_DIR` hands the command none.
  test("hands the command the host's environment, which carries no `GIT_DIR`", async () => {
    const directory = scratch();
    const before = process.env.GIT_DIR;
    process.env.GIT_DIR = "/elsewhere/.git";
    try {
      const result = await runShellCommand(
        'if [ -z "$GIT_DIR" ]; then printf unset; else printf %s "$GIT_DIR"; fi',
        { cwd: directory, limitMs: 10_000 }
      );
      expect(result.output).toBe("unset");
      expect(procmgr.getShellConfig().env.GIT_DIR).toBeUndefined();
    } finally {
      if (before === undefined) delete process.env.GIT_DIR;
      else process.env.GIT_DIR = before;
      rmSync(directory, { recursive: true, force: true });
    }
  });

  // The host's shell prefix goes before the command on the same line, which is how the host's own
  // helper applies it.
  test("puts `PI_SHELL_PREFIX` before the command", async () => {
    const directory = scratch();
    process.env.PI_SHELL_PREFIX = "printf before:;";
    try {
      const result = await runShellCommand("printf after", { cwd: directory, limitMs: 10_000 });
      expect(result.output).toBe("before:after");
    } finally {
      delete process.env.PI_SHELL_PREFIX;
      rmSync(directory, { recursive: true, force: true });
    }
  });

  // The five the host adds over the session's environment for every shell command it runs.
  test("adds the five variables the host adds to a shell command's environment", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand(
        'printf \'%s|%s|%s|%s|%s\' "$GIT_EDITOR" "$GPG_TTY" "$OMPCODE" "$CLAUDECODE" "$CI"',
        { cwd: directory, limitMs: 10_000 }
      );
      expect(result.output).toBe("true|not a tty|1|1|true");
      // The fifth is `SHELL`, which the host sets to the shell it started.
      const shell = await runShellCommand('printf %s "$SHELL"', {
        cwd: directory,
        limitMs: 10_000,
      });
      expect(statSync(shell.output).isFile()).toBe(true);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  // `CI` is the one of the five the host leaves out when it is told to; the session's own `CI`,
  // if it has one, still reaches the command, so this row runs with none.
  test("leaves `CI` out when `PI_BASH_NO_CI` is set", async () => {
    const directory = scratch();
    const before = process.env.CI;
    process.env.PI_BASH_NO_CI = "1";
    delete process.env.CI;
    try {
      const result = await runShellCommand(
        'if [ -z "$CI" ]; then printf unset; else printf %s "$CI"; fi',
        { cwd: directory, limitMs: 10_000 }
      );
      expect(result.output).toBe("unset");
    } finally {
      delete process.env.PI_BASH_NO_CI;
      if (before !== undefined) process.env.CI = before;
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("names the exit status of a command that failed", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("exit 3", { cwd: directory, limitMs: 10_000 });
      expect(result.status).toBe("The command ended with exit status 3.");
      expect(result.output).toBe("");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("keeps what the shell itself printed on its error stream", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("!printf hi", { cwd: directory, limitMs: 10_000 });
      expect(result.output).toContain("!printf");
      expect(result.status).toBe("The command ended with exit status 127.");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  });

  test("stops a command past its limit and keeps what it printed", async () => {
    const directory = scratch();
    const started = Date.now();
    try {
      const result = await runShellCommand("printf early; sleep 600", {
        cwd: directory,
        limitMs: 1_000,
      });
      expect(result.output).toBe("early");
      expect(result.status).toBe("The command was stopped after 1000 ms.");
      expect(Date.now() - started).toBeLessThan(10_000);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 20_000);

  test("cuts output past the bound and says so in each text", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("printf 'a%.0s' $(seq 1 20000)", {
        cwd: directory,
        limitMs: 20_000,
      });
      expect(result.cut).toBe(true);
      expect(result.output).toHaveLength(SHELL_OUTPUT_MAX);
      expect(shellCommandReport("!printf 'a%.0s' $(seq 1 20000)", result)).toContain(
        `The output was cut at ${SHELL_OUTPUT_MAX} characters.`
      );
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 30_000);

  // The module reads back `4 * SHELL_OUTPUT_MAX` bytes and says a command that prints without end
  // "costs the session this much memory and no more". It costs the session that much memory and
  // the whole of its disk: nothing bounds what the shell writes into the file those bytes are read
  // from, so `!yes`, `!cat /dev/urandom` or an ordinary `!grep -r … /` writes until the limit
  // (120 s by default) or until the filesystem is full. Measured at c974ed1e653a on the issue pod:
  // 376,043,850 bytes in 1.5 s, which the default limit scales to about 28 GiB. The bound here is
  // sixteen times the character cut, far above anything a fix that caps the writer would leave.
  // The watcher samples the real filesystem while a real child process writes to it, so no fake
  // clock can drive it: the bytes come from the shell, not from a timer this test owns.
  test("bounds what an endless command writes to disk, not only what it reads back", async () => {
    const directory = scratch();
    const bound = 16 * SHELL_OUTPUT_MAX;
    let peak = 0;
    const watch = setInterval(() => {
      for (const name of readdirSync(tmpdir())) {
        if (!name.startsWith("envoy-shell-")) continue;
        try {
          peak = Math.max(peak, statSync(join(tmpdir(), name, "output")).size);
        } catch {
          // The command's directory is removed as soon as it ends.
        }
      }
    }, 20);
    try {
      const result = await runShellCommand("yes legion", { cwd: directory, limitMs: 400 });
      expect(result.cut).toBe(true);
      expect(peak).toBeLessThanOrEqual(bound);
    } finally {
      clearInterval(watch);
      rmSync(directory, { recursive: true, force: true });
    }
  }, 30_000);

  // Dispatch cannot store U+0000 and refuses a whole reply that holds one (400 NUL_CHARACTER),
  // so `!head -c 64 /dev/urandom` or `!find . -print0` would leave the person with no answer at
  // all. Every other control character goes with it, the line break and the tab apart.
  test("drops the control characters Dispatch cannot store, keeping line breaks and tabs", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("printf 'a\\0b\\rc\\td\\ne\\07f'", {
        cwd: directory,
        limitMs: 10_000,
      });
      expect(result.output).toBe("abc\td\nef");
      expect(result.output).not.toContain("\u0000");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 20_000);

  // The session holds the kill of a running command's group, which is what a shutdown stops it
  // with: nothing it started is left running, and what it printed is still its answer. A real
  // child process and a real process group are what this row is about, so no fake clock can
  // drive it; it waits on the command's own marker rather than on a guessed delay.
  test("stops a command, and everything it started, when the caller's `stop` is called", async () => {
    const directory = scratch();
    const ready = join(directory, "ready");
    try {
      const result = await runShellCommand(`printf early; touch ${ready}; sleep 600`, {
        cwd: directory,
        limitMs: 60_000,
        started: (stop) => {
          void untilTrue(() => existsSync(ready)).then(stop);
        },
      });
      expect(result.output).toBe("early");
      expect(result.status).toBe("The command was stopped.");
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 60_000);

  // A `stop` the session hands in after the command has already ended is nothing: a command that
  // ended well is never reported as stopped, whichever moment a shutdown lands in. The row puts
  // the stop in that moment: the command leaves a process of its own session holding its pipe,
  // which the kill of the command's group cannot reach, so the reads run on to the drain grace
  // after the command's own shell is gone, and the stop is handed in once this machine's process
  // table no longer holds that shell.
  test("ignores a `stop` that arrives after the command has already ended", async () => {
    const directory = scratch();
    const setsid = Bun.which("setsid");
    if (setsid === null) throw new Error("this machine has no setsid to leave a group with");
    const pidFile = join(directory, "pid");
    const escaped = join(directory, "escaped");
    const shellPid = (): number => {
      try {
        return Number(readFileSync(pidFile, "utf8").trim());
      } catch {
        return 0;
      }
    };
    try {
      const result = await runShellCommand(
        `echo $$ > ${pidFile}; ${setsid} sh -c 'printf gone > ${escaped}; sleep 5' &` +
          ` while [ ! -f ${escaped} ]; do sleep 0.02; done; printf hi`,
        {
          cwd: directory,
          limitMs: 60_000,
          started: (stop) => {
            // The shell leaves the process table when it is reaped, which is what resolves the
            // wait on it, so by the time a poll of this machine's table can see it gone the
            // command has ended here too: the stop below lands after that end and inside the
            // reads that follow it, which is the moment this row is about.
            void untilTrue(() => {
              const pid = shellPid();
              return pid > 0 && !alive(pid);
            }).then(stop);
          },
        }
      );
      expect(result.output).toBe("hi");
      expect(result.status).toBeUndefined();
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 60_000);

  // The command's group is its own, so nothing but this module's own kill ends it: an `/exit` or
  // a crash during `!while :; do :; done` must leave no process behind. Proved as the reviewer
  // proved the defect - a process that ends well before the command's limit, which is the case
  // the limit cannot cover - and read off this machine's own process table, which again no fake
  // clock can drive.
  test("leaves nothing running when the process that started the command exits first", async () => {
    const directory = scratch();
    const pidFile = join(directory, "pid");
    const runner = join(directory, "runner.ts");
    await Bun.write(
      runner,
      [
        `import { runShellCommand } from ${JSON.stringify(join(import.meta.dir, "typed-shell.ts"))};`,
        `void runShellCommand(${JSON.stringify(`echo $$ > ${pidFile}; exec sleep 600`)}, {`,
        `  cwd: ${JSON.stringify(directory)},`,
        "  limitMs: 600_000,",
        "});",
        "// Gone as soon as the command is really running, with six hundred seconds of its limit",
        "// left and nothing but this process holding a handle on its group.",
        `while (!(await Bun.file(${JSON.stringify(pidFile)}).text().catch(() => "")).trim()) {`,
        "  await Bun.sleep(10);",
        "}",
        "process.exit(0);",
      ].join("\n")
    );
    try {
      const runs = Bun.spawn([process.execPath, runner], { stderr: "pipe", stdout: "ignore" });
      await runs.exited;
      expect(await new Response(runs.stderr).text()).toBe("");
      expect(runs.exitCode).toBe(0);
      const pid = Number(readFileSync(pidFile, "utf8").trim());
      expect(Number.isInteger(pid)).toBe(true);
      expect(await untilTrue(() => !alive(pid))).toBe(true);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  }, 60_000);
});

describe("the texts a command becomes", () => {
  /** The person's line, as they sent it and as Dispatch stored it. */
  const line = "!printf hi";
  const ran = {
    cut: false,
    output: "hi",
    status: undefined,
  };

  test("carry the person's line and what the command printed, fenced", () => {
    expect(shellCommandReport(line, ran)).toBe("!printf hi\n\n```\nhi\n```");
  });

  // Every surface this text reaches reads it as Markdown, so output nobody fenced is read as
  // whatever it looks like: this diff's `---` line is a heading and its `*` an emphasis mark.
  test("fence output Markdown would otherwise read as its own", () => {
    const printed = "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-one *two*\n+one _two_";
    expect(shellCommandReport("!git diff", { ...ran, output: printed })).toBe(
      `!git diff\n\n\`\`\`\n${printed}\n\`\`\``
    );
  });

  // Output that is itself fenced stays whole: the fence that holds it is longer than the longest
  // run of backticks it carries.
  test("grow the fence past a run of backticks in the output", () => {
    const printed = "```sh\nprintf hi\n```\n````\nouter\n````";
    expect(shellCommandReport("!cat note.md", { ...ran, output: printed })).toBe(
      `!cat note.md\n\n\`\`\`\`\`\n${printed}\n\`\`\`\`\``
    );
  });

  test("say so for a command that printed nothing, so every message has an answer", () => {
    expect(shellCommandReport("!true", { ...ran, output: "" })).toBe(
      "!true\n\nThe command printed nothing."
    );
  });

  test("end with the status of a command that did not exit 0", () => {
    expect(
      shellCommandReport("!exit 3", {
        ...ran,
        output: "",
        status: "The command ended with exit status 3.",
      })
    ).toBe("!exit 3\n\nThe command printed nothing.\n\nThe command ended with exit status 3.");
  });

  test("tell the agent whose command it was before the command itself", () => {
    expect(shellCommandMessage(shellCommandReport(line, ran))).toEndWith(
      "\n\n!printf hi\n\n```\nhi\n```"
    );
    expect(shellCommandMessage(shellCommandReport(line, ran))).toStartWith(
      "A person ran this shell"
    );
  });

  // The fence marks where the output ends in the agent's copy too: a file or a `curl` response
  // that prints the heading's own line must not reach the agent as if Dispatch had written it.
  test("mark where the output ends in the agent's copy", () => {
    const printed =
      "A person ran this shell command in this session from Dispatch. Nothing is asked of you; this is the command and what it printed.";
    const message = shellCommandMessage(
      shellCommandReport("!cat note", { ...ran, output: printed })
    );
    expect(message).toEndWith(`!cat note\n\n\`\`\`\n${printed}\n\`\`\``);
  });

  test("name the command still running when one already runs", () => {
    expect(shellCommandBusyReport(line, "sleep 600")).toBe(
      "!printf hi\n\nThe command was not run: this session is still running !sleep 600."
    );
  });

  test("say a session shutting down ran nothing, so that message is answered too", () => {
    expect(shellCommandShutdownReport(line)).toBe(
      "!printf hi\n\nThe command was not run: this session is shutting down."
    );
  });

  // The space a person typed after the mark is theirs, and so is a tab after the command: the
  // live view reads each of these texts back against the message's stored body
  // (`isShellCommandReport`), so one opening with the trimmed command matches nothing there and
  // the person's line is drawn a second time, once as their message and once as the report.
  test("open with the person's line as they sent it, the spacing around the mark kept", () => {
    for (const sent of ["!ls", "! ls", "!  git status", "!ls\t"]) {
      const report = shellCommandReport(sent, { ...ran, output: "one.txt" });
      const busy = shellCommandBusyReport(sent, "sleep 600");
      const shutdown = shellCommandShutdownReport(sent);
      expect(report).toBe(`${sent}\n\n\`\`\`\none.txt\n\`\`\``);
      expect(busy).toBe(
        `${sent}\n\nThe command was not run: this session is still running !sleep 600.`
      );
      expect(shutdown).toBe(`${sent}\n\nThe command was not run: this session is shutting down.`);
      expect(isShellCommandReport(report, sent)).toBe(true);
      expect(isShellCommandReport(busy, sent)).toBe(true);
      expect(isShellCommandReport(shutdown, sent)).toBe(true);
    }
    // What ran is still the trimmed command, which is what the CLI's own bang handler runs.
    expect(shellCommandFrom("! ls")).toBe("ls");
    expect(shellCommandFrom("!ls\t")).toBe("ls");
  });
});
