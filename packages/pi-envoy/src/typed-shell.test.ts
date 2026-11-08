import { describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
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

describe("the command a person's message says to run", () => {
  test("is the rest of a line whose first character is the mark, whatever it starts with", () => {
    expect(shellCommandFrom("!printf hi")).toBe("printf hi");
    expect(shellCommandFrom("!!printf hi")).toBe("!printf hi");
    expect(shellCommandFrom("!")).toBe("");
    expect(shellCommandFrom("!cd /tmp\nls")).toBe("cd /tmp\nls");
  });

  test("is nothing for a message that does not start with the mark", () => {
    expect(shellCommandFrom("printf hi")).toBeUndefined();
    expect(shellCommandFrom(" !printf hi")).toBeUndefined();
    expect(shellCommandFrom("Where is the dashboard?")).toBeUndefined();
    expect(shellCommandFrom("")).toBeUndefined();
  });
});

describe("running a person's command", () => {
  test("answers with what it printed, and names no status for one that exited 0", async () => {
    const directory = scratch();
    try {
      const result = await runShellCommand("printf hi", { cwd: directory, limitMs: 10_000 });
      expect(result).toEqual({ command: "printf hi", cut: false, output: "hi", status: undefined });
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
      expect(shellCommandReport(result)).toContain(
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
    const alive = (pid: number): boolean => {
      try {
        process.kill(pid, 0);
        return true;
      } catch {
        return false;
      }
    };
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
  const ran = {
    command: "printf hi",
    cut: false,
    output: "hi",
    status: undefined,
  };

  test("carry the person's line and what the command printed, fenced", () => {
    expect(shellCommandReport(ran)).toBe("!printf hi\n\n```\nhi\n```");
  });

  // Every surface this text reaches reads it as Markdown, so output nobody fenced is read as
  // whatever it looks like: this diff's `---` line is a heading and its `*` an emphasis mark.
  test("fence output Markdown would otherwise read as its own", () => {
    const printed = "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-one *two*\n+one _two_";
    expect(shellCommandReport({ ...ran, command: "git diff", output: printed })).toBe(
      `!git diff\n\n\`\`\`\n${printed}\n\`\`\``
    );
  });

  // Output that is itself fenced stays whole: the fence that holds it is longer than the longest
  // run of backticks it carries.
  test("grow the fence past a run of backticks in the output", () => {
    const printed = "```sh\nprintf hi\n```\n````\nouter\n````";
    expect(shellCommandReport({ ...ran, command: "cat note.md", output: printed })).toBe(
      `!cat note.md\n\n\`\`\`\`\`\n${printed}\n\`\`\`\`\``
    );
  });

  test("say so for a command that printed nothing, so every message has an answer", () => {
    expect(shellCommandReport({ ...ran, command: "true", output: "" })).toBe(
      "!true\n\nThe command printed nothing."
    );
  });

  test("end with the status of a command that did not exit 0", () => {
    expect(
      shellCommandReport({
        ...ran,
        command: "exit 3",
        output: "",
        status: "The command ended with exit status 3.",
      })
    ).toBe("!exit 3\n\nThe command printed nothing.\n\nThe command ended with exit status 3.");
  });

  test("tell the agent whose command it was before the command itself", () => {
    expect(shellCommandMessage(shellCommandReport(ran))).toEndWith(
      "\n\n!printf hi\n\n```\nhi\n```"
    );
    expect(shellCommandMessage(shellCommandReport(ran))).toStartWith("A person ran this shell");
  });

  // The fence marks where the output ends in the agent's copy too: a file or a `curl` response
  // that prints the heading's own line must not reach the agent as if Dispatch had written it.
  test("mark where the output ends in the agent's copy", () => {
    const printed =
      "A person ran this shell command in this session from Dispatch. Nothing is asked of you; this is the command and what it printed.";
    const message = shellCommandMessage(
      shellCommandReport({ ...ran, command: "cat note", output: printed })
    );
    expect(message).toEndWith(`!cat note\n\n\`\`\`\n${printed}\n\`\`\``);
  });

  test("name the command still running when one already runs", () => {
    expect(shellCommandBusyReport("printf hi", "sleep 600")).toBe(
      "!printf hi\n\nThe command was not run: this session is still running !sleep 600."
    );
  });

  test("say a session shutting down ran nothing, so that message is answered too", () => {
    expect(shellCommandShutdownReport("printf hi")).toBe(
      "!printf hi\n\nThe command was not run: this session is shutting down."
    );
  });
});
