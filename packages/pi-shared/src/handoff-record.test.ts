import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { lstat, mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential";
import { DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { fakeLatestHandoff } from "@legion/envoy-client/test/fake-dispatch";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import {
  versionControlEnvironment,
  versionControlIdentity,
  versionControlNames,
} from "@legion/test-fixtures/version-control-environment";
import {
  findHandoffReport,
  findPushedBranchHead,
  type JjRunner,
  paneLatestHandoff,
  pathJj,
} from "./handoff-record";

const ISSUE = "LEGION-208";
const STANDING = "c0de0000000000000000000000000000000000ff";
const TIP = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
/** The head a handoff record names, the commit its phase's work was done against. It is not the
 * branch's tip `TIP`, so a report carrying it carries what the record says and not what jj
 * answered. */
const HEAD = "9f2c81d4e6b70a35c9d8e1f4a2b6038d5c7e9a14";
const REMOTE = 'remote_bookmarks(exact:"legion/LEGION-208", exact:"origin")';
/** A handoff body as a file-backed role writes it: its stamps and the head its work was done
 * against, which `head` may be left out of or given any shape. */
const body = (head?: unknown) =>
  JSON.stringify({ schemaVersion: 1, issue: ISSUE, ...(head === undefined ? {} : { head }) });
const RECORD = { id: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13", version: 3, body: body(HEAD) };

const env = { LEGION_WORKSPACE: "/workspaces/LEGION-208", LEGION_ISSUE: ISSUE };

/** What the report asks jj of the branch at origin, by the commit each question is about: whether
 * the branch carries the head a record names (`HEAD`, or its tip `TIP`), and whether it carries
 * the commit the workspace stands on. */
const CARRIES = new Map([
  [`${REMOTE} & present(${HEAD})::`, HEAD],
  [`${REMOTE} & present(${TIP})::`, TIP],
  [`${REMOTE} & ${STANDING}::`, STANDING],
]);

/** A jj that answers the report's commands — the commit the workspace stands on, `STANDING`, and
 * the branch's tip at origin, `TIP`, when the branch carries the commit asked about, which it does
 * for every commit but those `unpushed` names — and records each call's args. */
function fakeJj(...unpushed: string[]): { jj: JjRunner; calls: string[][] } {
  const calls: string[][] = [];
  const jj: JjRunner = async (args, cwd) => {
    expect(cwd).toBe(env.LEGION_WORKSPACE);
    calls.push([...args]);
    const revision = args[2];
    if (revision === "@-") return STANDING;
    const commit = CARRIES.get(revision ?? "");
    if (commit !== undefined) return unpushed.includes(commit) ? "" : TIP;
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
  return { jj, calls };
}

/** A Dispatch whose every phase answers `record`, recording each read. */
const recordingDispatch = (record: typeof RECORD = RECORD) => fakeLatestHandoff(async () => record);

test("the implementer's round reports the newest implement record and the head it names", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
  ).resolves.toEqual({ commit: HEAD, handoffId: RECORD.id, handoffVersion: RECORD.version });
  expect(reads).toEqual(["LEGION-208 implement"]);
  // The commit reported is the head the record names, not the tip the branch stands at. jj is
  // asked whether the branch carries that head, then what the workspace stands on and whether the
  // branch carries that too.
  expect(calls).toEqual([
    ["log", "-r", `${REMOTE} & present(${HEAD})::`, "--no-graph", "-T", "commit_id"],
    ["log", "-r", "@-", "--no-graph", "-T", "commit_id"],
    ["log", "-r", `${REMOTE} & ${STANDING}::`, "--no-graph", "-T", "commit_id"],
  ]);
});

test("every file-backed role reports the head its phase's record names", async () => {
  for (const [phase, role, word] of [
    ["planning", "planner", "plan"],
    ["testing", "tester", "test"],
    ["reviewing", "reviewer", "review"],
  ] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      commit: HEAD,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });
    expect(reads).toEqual([`LEGION-208 ${word}`]);
    // The head comes from the record: these three ask the workspace nothing, since a sibling's
    // push moves the branch under a worker whose evidence is older.
    expect(calls).toEqual([]);
  }
});

test("a record naming no head, or one of another shape, leaves those three without a commit", async () => {
  // The record is the worker's own text, so a body this cannot read is no exception: the
  // completion goes without a commit, and the daemon refuses a behind-entry one of theirs.
  const unreadable = [
    body(),
    body(HEAD.slice(0, 39)),
    body(HEAD.toUpperCase()),
    body(`${HEAD} `),
    body(42),
    body(null),
    '["head"]',
    `"${HEAD}"`,
    "not json at all",
    "",
  ];
  for (const text of unreadable) {
    for (const [phase, role] of [
      ["planning", "planner"],
      ["testing", "tester"],
      ["reviewing", "reviewer"],
    ] as const) {
      const { jj, calls } = fakeJj();
      const { latestHandoff } = recordingDispatch({ ...RECORD, body: text });
      await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
        handoffId: RECORD.id,
        handoffVersion: RECORD.version,
      });
      expect(calls).toEqual([]);
    }
  }
});

test("every other completion reports the commit the workspace stands on, and reads no record", async () => {
  // Retro and the merger's READY write no record; a role reporting a phase it does not run reports
  // @- too, and the daemon refuses it naming whose phase it is.
  for (const [phase, role] of [
    ["retro", "implementer"],
    ["merging", "merger"],
    ["implementing", "tester"],
    ["admitted", "architect"],
  ] as const) {
    const { jj, calls } = fakeJj();
    const { latestHandoff, reads } = recordingDispatch();
    await expect(findHandoffReport({ phase, role, env, jj, latestHandoff })).resolves.toEqual({
      commit: STANDING,
    });
    expect(calls).toEqual([["log", "-r", "@-", "--no-graph", "-T", "commit_id"]]);
    expect(reads).toEqual([]);
  }
});

test("no record, or a Dispatch that does not answer, is refused naming what to do", async () => {
  const refusals: ReadonlyArray<readonly [() => Promise<never>, string]> = [
    [
      async () => {
        throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, "no handoff for test");
      },
      "LEGION-208 has no test handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase test --body-file <file>`, then complete again",
    ],
    [
      async () => {
        throw new DispatchServiceError("UNAUTHORIZED", 401, "invalid token");
      },
      "handoff_complete could not read LEGION-208's newest test handoff from Dispatch: 401 UNAUTHORIZED: invalid token; complete again once Dispatch answers",
    ],
    [
      async () => {
        throw new TypeError("Unable to connect. Is the computer able to access the url?");
      },
      "handoff_complete could not read LEGION-208's newest test handoff from Dispatch: Unable to connect. Is the computer able to access the url?; complete again once Dispatch answers",
    ],
  ];
  for (const [answer, message] of refusals) {
    const { jj } = fakeJj();
    const { latestHandoff } = fakeLatestHandoff(answer);
    await expect(
      findHandoffReport({ phase: "testing", role: "tester", env, jj, latestHandoff })
    ).rejects.toThrow(new Error(message));
  }
});

test("an implementer whose record names a head its branch does not carry is refused naming the push and the rewrite", async () => {
  const { jj, calls } = fakeJj(HEAD);
  const { latestHandoff } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
  ).rejects.toThrow(
    new Error(
      `the head the newest implement handoff of LEGION-208 names, ${HEAD}, is not on legion/LEGION-208@origin: when it is new work of yours, push it with \`jj bookmark set legion/LEGION-208 -r ${HEAD} && jj git push --bookmark legion/LEGION-208\`; otherwise write the handoff again with \`head\` set to the commit legion/LEGION-208@origin carries (\`jj log -r legion/LEGION-208@origin --no-graph -T commit_id\` prints it) and \`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\`; then complete again`
    )
  );
  expect(calls).toEqual([
    ["log", "-r", `${REMOTE} & present(${HEAD})::`, "--no-graph", "-T", "commit_id"],
  ]);
});

test("an implementer whose @- is not on its branch at origin is refused naming the push", async () => {
  // A round that forgot its push: its record names the branch's tip at origin, which the branch
  // carries, and its own commits are under `@-` alone.
  const { jj } = fakeJj(STANDING);
  const { latestHandoff } = recordingDispatch({ ...RECORD, body: body(TIP) });
  await expect(
    findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
  ).rejects.toThrow(
    new Error(
      `the commit this workspace stands on, ${STANDING}, is not on legion/LEGION-208@origin: push it with \`jj bookmark set legion/LEGION-208 -r @- && jj git push --bookmark legion/LEGION-208\`, then complete again`
    )
  );
});

test("an implementer whose record names no readable head is refused naming what to write", async () => {
  for (const text of [body(), body("the tip"), "not json at all"]) {
    const { jj, calls } = fakeJj();
    const { latestHandoff } = recordingDispatch({ ...RECORD, body: text });
    await expect(
      findHandoffReport({ phase: "implementing", role: "implementer", env, jj, latestHandoff })
    ).rejects.toThrow(
      new Error(
        "the newest implement handoff of LEGION-208 (version 3) names no head: write the handoff again with `head` set to the 40-character lower-case commit you pushed as legion/LEGION-208@origin's tip (`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>`), then complete again"
      )
    );
    // Nothing of the workspace is read: the record is what has to change.
    expect(calls).toEqual([]);
  }
});

test("the workspace and a well-formed issue key are required before jj or Dispatch is asked", async () => {
  const { jj, calls } = fakeJj();
  const { latestHandoff, reads } = recordingDispatch();
  await expect(
    findHandoffReport({
      phase: "retro",
      role: "implementer",
      env: { ...env, LEGION_WORKSPACE: undefined },
      jj,
      latestHandoff,
    })
  ).rejects.toThrow("LEGION_WORKSPACE is not set");
  await expect(
    findHandoffReport({
      phase: "implementing",
      role: "implementer",
      env: { ...env, LEGION_ISSUE: "LEGION-208#x" },
      jj,
      latestHandoff,
    })
  ).rejects.toThrow('LEGION_ISSUE "LEGION-208#x" is not a Dispatch issue key like LEGION-1');
  expect(calls).toEqual([]);
  expect(reads).toEqual([]);
});

test("a failing jj surfaces its command and stderr", async () => {
  const jj: JjRunner = async (args) => {
    throw new Error(`jj ${args.join(" ")} failed: Error: There is no jj repo in "."`);
  };
  const { latestHandoff } = recordingDispatch();
  await expect(
    findHandoffReport({ phase: "retro", role: "implementer", env, jj, latestHandoff })
  ).rejects.toThrow(
    'jj log -r @- --no-graph -T commit_id failed: Error: There is no jj repo in "."'
  );
});

// The head `ready_for_update` reports: the issue branch at the remote, read with the pane's jj.
test("the branch head is the issue bookmark at origin, and empty when the remote has no such branch", async () => {
  const calls: string[][] = [];
  const answer = async (head: string) => {
    const jj: JjRunner = async (args, cwd) => {
      expect(cwd).toBe(env.LEGION_WORKSPACE);
      calls.push([...args]);
      return head;
    };
    return findPushedBranchHead({ env, jj });
  };
  await expect(answer(TIP)).resolves.toBe(TIP);
  expect(calls).toEqual([["log", "-r", REMOTE, "--no-graph", "-T", "commit_id"]]);
  // A planner before its first push: no branch at the remote, and no error.
  await expect(answer("")).resolves.toBe("");

  // The issue key names the branch, and a worker sets its own environment.
  await expect(
    findPushedBranchHead({ env: { ...env, LEGION_ISSUE: "../x" }, jj: async () => TIP })
  ).rejects.toThrow(new Error('LEGION_ISSUE "../x" is not a Dispatch issue key like LEGION-1'));
  await expect(
    findPushedBranchHead({ env: { ...env, LEGION_WORKSPACE: "" }, jj: async () => TIP })
  ).rejects.toThrow(
    new Error(
      "LEGION_WORKSPACE is not set, so ready_for_update cannot find the branch head to report"
    )
  );
});

const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});
afterEach(forgetBrokerClients);

test("the pane's Dispatch reads the phase's newest record as the session's config names it", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-record-"));
  scratch.push(root);
  const file = { path: path.join(root, "envoy.json"), error: null } as const;
  const requests: Array<{ url: string; authorization: string | null }> = [];
  const answers: Response[] = [];
  const dispatch = (async (input, init) => {
    requests.push({
      url: input.toString(),
      authorization: new Headers(init?.headers).get("authorization"),
    });
    return answers.shift() ?? Response.json({ error: "unexpected" }, { status: 500 });
  }) as typeof fetch;
  const paneEnv = {
    ...env,
    DISPATCH_URL: "http://dispatch.test",
    DISPATCH_TOKEN: "pane-dispatch-token",
  };
  const latestHandoff = paneLatestHandoff(file, paneEnv, root, "pane-session", dispatch);
  const report = () =>
    findHandoffReport({ phase: "reviewing", role: "reviewer", env: paneEnv, latestHandoff });

  answers.push(
    Response.json({
      ...RECORD,
      issue_key: ISSUE,
      phase: "review",
      author: { kind: "agent", session_id: "ses_reviewer" },
      created_at: "2026-10-26T09:00:00Z",
      body: JSON.stringify({ schemaVersion: 1, phase: "review", decision: "approved", head: HEAD }),
    })
  );
  // The head travels in the record's body, from Dispatch's own answer to the completion.
  await expect(report()).resolves.toEqual({
    commit: HEAD,
    handoffId: RECORD.id,
    handoffVersion: RECORD.version,
  });
  expect(requests).toEqual([
    {
      url: "http://dispatch.test/api/v1/issues/LEGION-208/handoffs/review/latest",
      authorization: "Bearer pane-dispatch-token",
    },
  ]);

  // Dispatch's own 404 for a phase with none is the missing-record refusal.
  answers.push(
    Response.json(
      { error: "no review handoff on LEGION-208", code: "HANDOFF_NOT_FOUND" },
      { status: 404 }
    )
  );
  await expect(report()).rejects.toThrow(
    "LEGION-208 has no review handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase review --body-file <file>`, then complete again"
  );

  // A pane whose Dispatch is off says so, and asks nothing.
  const off = paneLatestHandoff(file, env, root, "pane-session", dispatch);
  await expect(
    findHandoffReport({ phase: "reviewing", role: "reviewer", env, latestHandoff: off })
  ).rejects.toThrow(
    /^handoff_complete could not read LEGION-208's newest review handoff from Dispatch: Dispatch is off for this pane: .*Dispatch is off; complete again once Dispatch answers$/
  );
  expect(requests).toHaveLength(2);
});

// The route the reviewer's `handoff_complete` failure came from: a pane whose Dispatch has no
// bearer at all and trades its own identity at the secrets broker. There is one credential field,
// so this pane presents the DPoP token the trade minted rather than an empty bearer.
test("a pane whose Dispatch is the broker's reads the record under its own session's token", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-broker-"));
  scratch.push(root);
  const file = { path: path.join(root, "envoy.json"), error: null } as const;
  const trades: URLSearchParams[] = [];
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      trades.push(new URLSearchParams(await request.text()));
      return Response.json({
        access_token: "broker-access-token",
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 3600,
      });
    },
  });
  const identityFile = path.join(root, "projected-token");
  await Bun.write(identityFile, "projected.service.account.token\n");
  const asked: Array<{ authorization: string | null; proof: string | null }> = [];
  const dispatch = (async (_input, init) => {
    const headers = new Headers(init?.headers);
    asked.push({
      authorization: headers.get("authorization"),
      proof: headers.get("dpop"),
    });
    return Response.json(RECORD);
  }) as typeof fetch;
  const paneEnv = {
    ...env,
    DISPATCH_URL: "http://dispatch.test",
    AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: identityFile,
  };
  try {
    const latest = paneLatestHandoff(file, paneEnv, root, "pane-session", dispatch);
    await latest("LEGION-208", "review");
  } finally {
    broker.stop(true);
  }

  expect(trades.map((form) => form.get("session"))).toEqual(["pane-session"]);
  expect(asked.map((one) => one.authorization)).toEqual(["DPoP broker-access-token"]);
  expect(asked[0]?.proof).toBeString();
});

// The revsets are jj's, so a fake cannot prove them: one real repository with a bare origin, the
// round's change committed and the branch pushed, run through PATH's jj, as a pane is. A host
// with no jj runs neither row and says so, once for the file. The jj the rows below run is the one
// this walk found, so no spawn of theirs walks PATH again.
const jjProgram = hostTool("jj");
const withoutJj = skipWithout("jj");

// `isolatedHome` puts these on the test process as well, so the jj that the code under test
// (`pathJj`) starts reads the test's home rather than the developer's.
restoreEnvironment(versionControlNames);

/**
 * A stand-in shared public leak check under `root`, whose path {@link isolatedHome} names: a
 * script that reads its standard input, as the real check does, so nothing is left in a pipe when
 * it exits, and clears the write it was asked about. It answers what a passing check answers, and
 * nothing else.
 *
 * The first of the two rows below pushes, and a push runs under whatever `git` is first on
 * `PATH`. In an issue pod that is Legion's wrapper (`/opt/legion/bin/git`), which points a push at
 * a hook that runs the leak guard, and the guard refuses a push it can resolve no shared check
 * for. With no check named, that push stops with `LEAK CHECK SETUP ERROR` and the row fails
 * before it tests anything, while the same row passes on a runner whose git has no such wrapper.
 * The stand-in makes the two one run.
 */
function passingLeakCheck(root: string): string {
  const check = path.join(root, "public-leak-check.sh");
  writeFileSync(check, "#!/bin/sh\ncat >/dev/null 2>&1\nexit 0\n", { mode: 0o700 });
  return check;
}

/**
 * The environment this file's real jj and git run in, and the one it puts on the test process: the
 * shared isolation fixture over a home under `root`, so neither the developer's
 * `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor the system gitconfig decides a result. It
 * reaches the test process too, because `pathJj` — the code under test — starts jj with the
 * process's own environment.
 *
 * The leak check is named for the children alone, not put on this process: it is a file under
 * `root`, which this file's own cleanup removes, and a name left on the process would outlive it
 * for whatever test file runs next.
 */
function isolatedHome(root: string): Record<string, string> {
  const isolated = versionControlEnvironment(path.join(root, "home"));
  Object.assign(process.env, isolated);
  return {
    PATH: process.env.PATH ?? "",
    ...isolated,
    PUBLIC_LEAK_CHECK: passingLeakCheck(root),
  };
}

/** Runs jj in `cwd` with the test's own environment, failing on a non-zero exit. */
async function jj(cwd: string, env: Record<string, string>, ...args: string[]): Promise<string> {
  const child = Bun.spawn([jjProgram ?? "jj", ...args], {
    cwd,
    env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`jj ${args.join(" ")} exited ${code}:\n${stderr}`);
  return stdout.trim();
}

test.skipIf(withoutJj)(
  "against a real jj workspace, the report refuses a head or an @- the branch does not carry and names the record's head once both are pushed",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-record-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const childEnv = isolatedHome(root);
    const paneEnv = { ...versionControlIdentity, LEGION_WORKSPACE: workspace, LEGION_ISSUE: ISSUE };
    await Bun.spawn(["git", "init", "--bare", "--quiet", path.join(root, "origin.git")], {
      env: childEnv,
    }).exited;
    await jj(root, childEnv, "git", "init", workspace);
    await jj(workspace, childEnv, "git", "remote", "add", "origin", path.join(root, "origin.git"));
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    await jj(workspace, childEnv, "describe", "-m", "implement: the change");
    await jj(workspace, childEnv, "new");
    const change = await jj(
      workspace,
      childEnv,
      "log",
      "-r",
      "@-",
      "--no-graph",
      "-T",
      "commit_id"
    );
    expect(change).toMatch(/^[0-9a-f]{40}$/);
    // The head the implementer's record names, as that round wrote it.
    let head = change;
    const { latestHandoff } = fakeLatestHandoff(async () => ({ ...RECORD, body: body(head) }));
    const report = () =>
      findHandoffReport({
        phase: "implementing",
        role: "implementer",
        env: paneEnv,
        jj: pathJj,
        latestHandoff,
      });

    // Committed but not pushed: the branch does not exist at origin yet.
    await expect(report()).rejects.toThrow(
      `the head the newest implement handoff of ${ISSUE} names, ${change}, is not on legion/${ISSUE}@origin`
    );

    // Pushed: the head the record names is reported with the record.
    await jj(workspace, childEnv, "bookmark", "set", `legion/${ISSUE}`, "-r", "@-");
    await jj(workspace, childEnv, "git", "push", "--bookmark", `legion/${ISSUE}`);
    await expect(report()).resolves.toEqual({
      commit: change,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });

    // A head this repository has never seen is that same refusal, not a jj error about a
    // revision: `present` answers nothing for it.
    head = "0123456789abcdef0123456789abcdef01234567";
    await expect(report()).rejects.toThrow(
      `the head the newest implement handoff of ${ISSUE} names, ${head}, is not on legion/${ISSUE}@origin`
    );

    // A later commit the record names and the branch does not carry is refused again, and so is
    // a round that forgot that push and names the branch's tip at origin in its record: the head
    // is on the branch, the commit the workspace stands on is not. Once the branch has moved past
    // it, an older head the branch still carries is reported as it was, so a push that moves the
    // branch ahead of a worker's evidence decides nothing here.
    await writeFile(path.join(workspace, "main.py"), 'print("bye")\n');
    await jj(workspace, childEnv, "describe", "-m", "implement: the fix");
    await jj(workspace, childEnv, "new");
    const fix = await jj(workspace, childEnv, "log", "-r", "@-", "--no-graph", "-T", "commit_id");
    head = fix;
    await expect(report()).rejects.toThrow(
      `the head the newest implement handoff of ${ISSUE} names, ${fix}, is not on legion/${ISSUE}@origin`
    );
    head = change;
    await expect(report()).rejects.toThrow(
      `the commit this workspace stands on, ${fix}, is not on legion/${ISSUE}@origin`
    );
    await jj(workspace, childEnv, "bookmark", "set", `legion/${ISSUE}`, "-r", "@-");
    await jj(workspace, childEnv, "git", "push", "--bookmark", `legion/${ISSUE}`);
    await expect(report()).resolves.toEqual({
      commit: change,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    });

    // Retro reports the commit the workspace stands on, whatever the record names.
    await expect(
      findHandoffReport({
        phase: "retro",
        role: "implementer",
        env: paneEnv,
        jj: pathJj,
        latestHandoff,
      })
    ).resolves.toEqual({ commit: fix });
  },
  60_000
);

// A git lock an interrupted git or jj left in the workspace fails every jj that snapshots a change
// into the working copy (jj rewrites the git index then). In a pod the roles container's launcher
// sweeps such a lock once it is more than 15 minutes old and no process of the container holds it
// open, so the note tells the role to run the command again; this test removes nothing either.
// The failure names the lock and its age, and the lock stays.
test.skipIf(withoutJj)(
  "a jj that fails on a stale git lock names it, its age, and when it goes",
  async () => {
    const root = await mkdtemp(path.join(tmpdir(), "legion-handoff-lock-"));
    scratch.push(root);
    const workspace = path.join(root, "workspace");
    const childEnv = isolatedHome(root);
    await jj(root, childEnv, "git", "init", "--colocate", workspace);
    await writeFile(path.join(workspace, "main.py"), 'print("hi")\n');
    const lock = path.join(workspace, ".git", "index.lock");
    await writeFile(lock, "");
    const written = new Date(Date.now() - 31 * 60 * 1000);
    await utimes(lock, written, written);

    const failure = pathJj(["diff", "-r", "@", "--name-only"], workspace);
    await expect(failure).rejects.toThrow("Could not acquire lock for index file");
    await expect(failure).rejects.toThrow(
      new RegExp(
        `git lock ${lock.replaceAll(/[.*+?^${}()|[\]\\]/g, "\\$&")} was last written 31m\\d+s ago and blocks this command\\. The launcher of the issue's pod removes a git lock .* Under the tmux runtime nothing removes it: .* Run the command again in about 16 minutes\\. If the lock is still there after that time, tell the issue's architect\\.`
      )
    );
    expect((await lstat(lock)).isFile()).toBe(true);
  },
  60_000
);
