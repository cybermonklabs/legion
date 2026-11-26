/** The split of `e2e/acceptance/restricted-sweep.sh`, against the fake Dispatch of
 *  `restricted-sweep-fake.ts`: fills mode holds the full member's half where a full member's
 *  credential may be, a runner the member's half where none may be, and a writer the live check's
 *  writes. Each fake is alike, so fills one fake answered serve a runner or a writer of another. */

import { afterAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit } from "./bounded-subprocess";
import {
  appInstallation,
  at,
  type FeedShape,
  failures,
  fullCookie,
  fullToken,
  issueMode,
  type Leak,
  literally,
  memberCookie,
  memberLogin,
  ownIssue,
  passed,
  routesFileOf,
  stopFakes,
  sweep,
  tgt,
  verdictOf,
  withoutTools,
} from "./restricted-sweep-fake";
import {
  deliverySeconds,
  proxyReads,
  runnerLiveSeconds,
  sha256Hex,
} from "./restricted-sweep-index";

afterAll(stopFakes);

describe.skipIf(withoutTools)("the split into fills, a runner and a writer", () => {
  const fillsDirectories = temporaryDirectories("restricted-sweep-fills");
  const someRows = ["/api/v1/inbox", "/api/v1/issues/{key}/comments", appInstallation];
  /** The route index a run of `someRows` reads, as `sweep` writes it, and its SHA-256. */
  const someRowsSha256 = sha256Hex(routesFileOf(someRows));
  /** A write's line: when its comment was asked for, when its direct message was answered, and the
   *  ids of both. */
  const writeLine = String.raw`write \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ [0-9a-f-]{36} [0-9a-f-]{36}`;

  /** A run of fills mode, and the file it was to write. Its proxy reads a browser's person alone,
   *  as a build before cybermonklabs/legion#5079 does, unless `tokenProxy`, so its reads can fail
   *  only with the full member's cookie, which the run is given unless `withoutCookie`; `full`
   *  plants answers for the full member's requests.
   *  Its live socket is served, for the agent view's full half, which the full
   *  member's token reads. `omit` leaves kinds out of its lookup; `createdAt` is when its fake's
   *  OWN was created. */
  async function fills(
    leaks: readonly Leak[] = [],
    {
      withoutCookie = false,
      tokenProxy = false,
      full,
      omit,
      createdAt,
    }: {
      withoutCookie?: boolean;
      tokenProxy?: boolean;
      full?: readonly Leak[];
      omit?: ReadonlySet<string>;
      createdAt?: string | null;
    } = {},
    file = join(fillsDirectories(), "fills.tsv")
  ) {
    const run = await sweep(leaks, {
      env: {
        SWEEP_MODE: "fills",
        SWEEP_MEMBER: memberLogin,
        SWEEP_FILLS_FILE: file,
        SWEEP_FULL_COOKIE: withoutCookie ? undefined : fullCookie,
        SWEEP_RESTRICTED_COOKIE: undefined,
        SWEEP_MEMBER_SESSION: undefined,
        SWEEP_WRITE_ISSUE: undefined,
      },
      feed: { live: true },
      ...(omit === undefined ? {} : { omit }),
      ...(createdAt === undefined ? {} : { createdAt }),
      proxyReadsCookies: !tokenProxy,
      ...(full === undefined ? {} : { full }),
      rows: someRows,
    });
    return { ...run, file };
  }

  /** A runner's run with FILE and no full member's credential, over `rows` (`someRows` unless
   *  given), its document socket served, and its live feed served unless `live` is false: a run
   *  that a leak decides, or that stops before its feed, need not wait out a live window.
   *  `createdAt` is when its fake's OWN was created; `env` changes its inputs, and `feed` its
   *  fake's sockets. A runner whose feed is served reads it for the 45 s live window, so it is
   *  stopped at the `runnerSweep` bound; one that is not stays under `short`. */
  function runner(
    file: string,
    leaks: readonly Leak[] = [],
    {
      live = true,
      rows = someRows,
      createdAt,
      env = {},
      feed = {},
    }: {
      live?: boolean;
      rows?: readonly string[];
      createdAt?: string | null;
      env?: Readonly<Record<string, string | undefined>>;
      feed?: FeedShape;
    } = {}
  ) {
    return sweep(leaks, {
      env: {
        SWEEP_FILLS_FILE: file,
        SWEEP_FULL_TOKEN: undefined,
        SWEEP_CONVERSATION_FILE: undefined,
        SWEEP_WRITE_ISSUE: undefined,
        SWEEP_LIVE_SECONDS: String(runnerLiveSeconds),
        ...env,
      },
      ...(createdAt === undefined ? {} : { createdAt }),
      bound: live ? "runnerSweep" : "short",
      feed: { doc: true, live, ...feed },
      rows,
    });
  }

  /** A writer's run with FILE's fills, writing every WRITE_EVERY seconds (1 unless given) for
   *  WRITE_SECONDS, with the full member's token alone, which writes its direct messages. */
  function writer(file: string, writeSeconds: string, noise?: string, writeEvery = "1") {
    return sweep([], {
      env: {
        SWEEP_MODE: "writer",
        SWEEP_FILLS_FILE: file,
        SWEEP_WRITE_SECONDS: writeSeconds,
        SWEEP_WRITE_EVERY: writeEvery,
        SWEEP_RESTRICTED_COOKIE: undefined,
        SWEEP_MEMBER_SESSION: undefined,
        SWEEP_CONVERSATION_FILE: undefined,
      },
      feed: noise === undefined ? { live: true } : { live: true, noise },
    });
  }

  /** FILE's text without the lines `drop` matches, written back; it throws where none did. */
  function without(file: string, drop: (line: string) => boolean): void {
    const lines = readFileSync(file, "utf8").split("\n");
    const kept = lines.filter((line) => !drop(line));
    expect(kept.length).toBeLessThan(lines.length);
    writeFileSync(file, kept.join("\n"));
  }

  test(
    "fills mode writes the full member's half, makes no request as the member, and writes no credential",
    async () => {
      const { code, file, fullRequests, memberPaths, origin, stdout } = await fills();
      expect(verdictOf(stdout)).toStartWith("FILLS: ");
      expect(code, stdout).toBe(0);
      expect(memberPaths).toEqual([]);
      expect(fullRequests).toBeGreaterThan(0);
      const text = readFileSync(file, "utf8");
      const lines = text.trimEnd().split("\n");
      expect(lines[0]).toBe("restricted-sweep-fills\t4");
      expect(lines).toContain("project\tTGT");
      // The origin the fills were made at, a private one where they are made apart, by its SHA-256
      // alone: the fills may travel as a workflow's input.
      expect(lines).toContain(`origin_sha256\t${sha256Hex(origin)}`);
      expect(text).not.toContain(origin);
      // The route index the fills were made from, and the deployment they were made at.
      expect(lines).toContain(`routes\t${someRowsSha256}`);
      expect(lines.filter((line) => /^deployment\t[0-9a-f]{64}$/.test(line))).toHaveLength(1);
      expect(lines).toContain(`member\t${memberLogin}`);
      expect(lines).toContain("own\tOWN");
      expect(lines).toContain(`going_run\t${tgt.goingRun}`);
      expect(lines).toContain(`issue_title\t${tgt.title}`);
      expect(lines).toContain(`proxy_repo\t${tgt.repository}`);
      expect(lines).toContain(`dm_session\t${tgt.session}`);
      expect(lines).toContain(`dm_message\t${tgt.directMessage}`);
      // The found values a FAIL line names by their kind, never as they are.
      expect(lines).toContain(`named\trepository ${tgt.repository}`);
      expect(lines).toContain(`named\tperson ${tgt.person}`);
      expect(lines).toContain(`named\tconversation ${tgt.session}/${tgt.conversationFile}`);
      expect(lines).toContain("can_fail\tGET /api/v1/issues/{key}/comments");
      for (const shape of proxyReads())
        expect(lines).toContain(`can_fail\tGET /api/github/rest/repos/{owner}/{repo}/${shape}`);
      expect(lines).toContain(`can_fail\tGET ${appInstallation}`);
      expect(lines).toContain("can_fail\t/ws/live agent view of {session_id}");
      for (const secret of [fullToken, fullCookie, memberCookie])
        expect(text).not.toContain(secret);
      expect(/^[\t\n\x20-\x7e]*$/.test(text)).toBe(true);
    },
    rowLimit("short")
  );

  // A runner's log is public: it names the fills' origin, a private one, and the member's login by
  // digests, and none of the names the fills hold - the project's repository, its people, its
  // conversation's file. Whether the agent view can fail was read when the fills were made, and
  // the session may answer otherwise since, so its line says when.
  test(
    "a runner with those fills and no full member's credential passes, names no private name, and prints a live window that leaves the writer's delivery bound",
    async () => {
      const { file, origin: fillsOrigin } = await fills();
      const { code, fullRequests, stdout } = await runner(file);
      expect(fullRequests).toBe(0);
      const digestOf = (text: string) => sha256Hex(text).slice(0, 12);
      expect(stdout).toContain(
        `\nmember  a restricted member TGT's access list does not name, whose login's SHA-256 starts ${digestOf(memberLogin)}\n`
      );
      expect(stdout).toMatch(
        new RegExp(
          `^fills {3}of TGT for the login whose SHA-256 starts ${digestOf(memberLogin)}, made -?\\d+ s ago at the origin whose SHA-256 starts ${digestOf(fillsOrigin)}: \\d+ values, \\d+ rows that can fail$`,
          "m"
        )
      );
      for (const name of [
        fillsOrigin,
        memberLogin,
        tgt.repository,
        tgt.person,
        `${tgt.session}/${tgt.conversationFile}`,
      ]) {
        expect(stdout).not.toContain(name);
      }
      const window =
        /^live window (\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ) (\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ)$/m.exec(
          stdout
        );
      expect(window, stdout).not.toBeNull();
      expect(Date.parse(window?.[2] ?? "") - Date.parse(window?.[1] ?? "")).toBe(
        (runnerLiveSeconds - deliverySeconds) * 1000
      );
      expect(stdout).toMatch(/^ok {6}GET \/api\/v1\/issues\/\{key\}\/comments: 404 \(can fail\)$/m);
      expect(stdout).toContain(
        `\nok      GET ${appInstallation}: 200, as a missing one is answered (can fail)\n`
      );
      expect(stdout).toContain(
        "\nok      /ws/live agent view of {session_id}: no one answers for it (could fail when the fills were made)\n"
      );
      expect(verdictOf(stdout)).toMatch(
        new RegExp(
          `^${literally(passed)}; the live check holds once a writer's delivered write falls inside the live window \\S+ \\S+, which the tailnet leg joins$`
        )
      );
      expect(code, stdout).toBe(0);
    },
    rowLimit("short", "runnerSweep")
  );

  // The control of the row above: the same runner, with a leak planted, fails.
  test(
    "the same runner fails where the member is answered a row of the project",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await runner(
        file,
        [at(`/api/v1/issues/${tgt.issue}/comments`, 200, [{ id: tgt.comment }])],
        { live: false }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/issues/{key}/comments: the member is answered 200 holding ${tgt.comment}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short", "short")
  );

  // The same runner, where the route that answers a hidden repository as a missing one answers the
  // member that the App is installed on the project's.
  test(
    "the same runner fails where a route that answers a hidden repository as a missing one answers the member otherwise",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await runner(
        file,
        [at(`/api/v1/github/app-installation/${tgt.repository}`, 200, { installed: true })],
        { live: false }
      );
      expect(failures(stdout)).toEqual([
        `GET ${appInstallation}: the member is answered 200 for TGT's object, otherwise than a missing one answered 200`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short", "short")
  );

  // A rate limit answers every repository alike, the hidden one and one nobody holds: two equal
  // answers of it say nothing of what the member may see, whatever the fills said of the full
  // member's half.
  test(
    "the same runner, its member rate-limited by the GitHub proxy for both repositories, is UNKNOWN",
    async () => {
      const { file } = await fills();
      const limited = {
        code: "GITHUB_RATE_LIMITED",
        error: "GitHub rate-limited the App; retry after 1m0s",
      };
      const { code, stdout } = await runner(
        file,
        [
          at(`/api/github/rest/repos/${tgt.repository}/pulls/1`, 503, limited),
          at("/api/github/rest/repos/acme/no-such-repo/pulls/1", 503, limited),
        ],
        { live: false }
      );
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nUNKNOWN GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}: the member is answered 503, and 503 for a repository the App is not installed on, which no rule judges\n"
      );
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short", "short")
  );

  // With no cookie on a build before cybermonklabs/legion#5079, whose proxy refuses the full
  // member's token, no proxy read can fail: fills mode keeps each with no can_fail line, so the
  // rest of the split still runs, and a runner reads each UNKNOWN, never a pass of a read that
  // could not fail. The agent view's half is the full member's token's.
  test(
    "fills mode with no full member's cookie, against a proxy that refuses the token, writes the fills, no proxy read among the rows that can fail, and a runner reads each UNKNOWN",
    async () => {
      const { code, file, stdout } = await fills([], { withoutCookie: true });
      expect(verdictOf(stdout)).toStartWith("FILLS: ");
      expect(code, stdout).toBe(0);
      const lines = readFileSync(file, "utf8").trimEnd().split("\n");
      for (const shape of proxyReads()) {
        const label = `GET /api/github/rest/repos/{owner}/{repo}/${shape}`;
        expect(stdout).toContain(
          `\nfill    ${label} (cannot fail: the proxy refused the full member's token (401 NO_IDENTITY): a build before cybermonklabs/legion#5079 reads the proxy as the browser's person alone; give SWEEP_FULL_COOKIE)\n`
        );
        expect(lines).not.toContain(`can_fail\t${label}`);
        expect(lines).toContain(`full_status\t401 ${label}`);
      }
      expect(lines).toContain("can_fail\t/ws/live agent view of {session_id}");
      const run = await runner(file);
      for (const shape of proxyReads()) {
        expect(run.stdout).toContain(
          `\nUNKNOWN GET /api/github/rest/repos/{owner}/{repo}/${shape}: the member is answered 503, as a repository the App is not installed on, but SWEEP_FILLS_FILE holds no full member's read of it that could fail\n`
        );
      }
      expect(run.stdout).toContain(
        "\nok      /ws/live agent view of {session_id}: no one answers for it (could fail when the fills were made)\n"
      );
      expect(run.stdout).toMatch(/^live window \S+ \S+$/m);
      expect(verdictOf(run.stdout)).toBe(
        `UNKNOWN: nothing reached the member, but this run did not prove it, for the ${proxyReads().length} reasons above`
      );
      expect(run.code).toBe(2);
    },
    rowLimit("short", "runnerSweep")
  );

  // A build from #5079 on reads the proxy with the full member's token: with no cookie at all,
  // the fills hold each proxy read as one that can fail, and a runner of them passes.
  test(
    "fills mode with no full member's cookie, against a proxy that reads the token, holds each proxy read as one that can fail, and a runner passes",
    async () => {
      const { code, file, stdout } = await fills([], { withoutCookie: true, tokenProxy: true });
      expect(verdictOf(stdout)).toStartWith("FILLS: ");
      expect(code, stdout).toBe(0);
      const lines = readFileSync(file, "utf8").trimEnd().split("\n");
      for (const shape of proxyReads()) {
        expect(lines).toContain(`can_fail\tGET /api/github/rest/repos/{owner}/{repo}/${shape}`);
      }
      const run = await runner(file);
      expect(verdictOf(run.stdout)).toMatch(
        new RegExp(
          `^${literally(passed)}; the live check holds once a writer's delivered write falls inside the live window \\S+ \\S+, which the tailnet leg joins$`
        )
      );
      expect(run.code, run.stdout).toBe(0);
    },
    rowLimit("short", "runnerSweep")
  );

  // With the cookie, a proxy read the full member's half could not make fail still leaves no
  // fills: the proxy answers the full member TGT's repository as one the App is not installed on.
  test(
    "fills mode with the full member's cookie, where a proxy read cannot fail, writes no file, and names it",
    async () => {
      const notInstalled = {
        code: "GITHUB_TOKEN_UNAVAILABLE",
        error: `the GitHub App is not installed on ${tgt.repository}`,
      };
      const { code, file, stdout } = await fills([], {
        full: [at(`/api/github/rest/repos/${tgt.repository}/pulls/1`, 503, notInstalled)],
      });
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: no fills were written");
      expect(stdout).toContain(
        "\n  GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}: the full member is answered 503 for TGT's repository as for one the App is not installed on (503), so the read could not fail\n"
      );
      expect(code).toBe(2);
      expect(await Bun.file(file).exists()).toBe(false);
    },
    rowLimit("short")
  );

  test(
    "a runner whose fills hold no proxy read that could fail is UNKNOWN, naming it",
    async () => {
      const { file } = await fills();
      without(
        file,
        (line) => line === "can_fail\tGET /api/github/rest/repos/{owner}/{repo}/pulls/{number}"
      );
      const { code, stdout } = await runner(file);
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nUNKNOWN GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}: the member is answered 503, as a repository the App is not installed on, but SWEEP_FILLS_FILE holds no full member's read of it that could fail\n"
      );
      expect(verdictOf(stdout)).toBe(
        "UNKNOWN: nothing reached the member, but this run did not prove it, for the 1 reasons above"
      );
      expect(code).toBe(2);
    },
    rowLimit("short", "runnerSweep")
  );

  // A row the fills hold no line of was never asked of the full member: neither whether it can
  // fail nor what the full member was answered. Its member's half is read all the same, so a leak
  // there FAILs, but its silence proves nothing, and it is never the `blind` of a row the full
  // member was answered nothing of.
  const comments = "GET /api/v1/issues/{key}/comments";
  const ofComments = (line: string) =>
    line === `can_fail\t${comments}` || line.endsWith(` ${comments}`);

  test(
    "a runner whose fills hold no line of a row is UNKNOWN, naming it, never blind",
    async () => {
      const { file } = await fills();
      without(file, ofComments);
      const { code, stdout } = await runner(file, [], { live: false });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        `\nUNKNOWN ${comments}: SWEEP_FILLS_FILE holds no full member's read of it: the fills were made from another API document\n`
      );
      expect(stdout).not.toMatch(new RegExp(`^ok {6}${literally(comments)}:`, "m"));
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short", "short")
  );

  test(
    "a runner whose fills hold no line of a row still fails where the member is answered a row of the project there",
    async () => {
      const { file } = await fills();
      without(file, ofComments);
      const { code, stdout } = await runner(
        file,
        [at(`/api/v1/issues/${tgt.issue}/comments`, 200, [{ id: tgt.comment }])],
        { live: false }
      );
      expect(failures(stdout)).toEqual([
        `${comments}: the member is answered 200 holding ${tgt.comment}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short", "short")
  );

  // Fills made from another route index judged rows the runner's lacks, and lack rows the
  // runner's holds: the runner refuses them before its first request.
  test(
    "a runner whose route index is not the one the fills were made from is UNKNOWN, naming both digests, before its first request",
    async () => {
      const { file } = await fills();
      const { code, memberPaths, stdout } = await runner(file, [], {
        rows: [...someRows, "/api/v1/agents", "/api/v1/comments/{id}"],
      });
      expect(verdictOf(stdout)).toMatch(
        new RegExp(
          `^UNKNOWN: SWEEP_FILLS_FILE was made from another route index: its routes.txt's SHA-256 is ${someRowsSha256}, and this run's is [0-9a-f]{64}$`
        )
      );
      expect(memberPaths).toEqual([]);
      expect(code).toBe(2);
    },
    rowLimit("short", "runnerSweep")
  );

  // Fills made at another deployment fill every row with objects that name nothing here, each of
  // which a correct server refuses as missing: the runner refuses them before it calls a row.
  test(
    "a runner at another deployment than the fills were made at is UNKNOWN before it calls a row",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await runner(file, [], {
        createdAt: "2026-02-02T00:00:00.654321Z",
        live: false,
      });
      expect(verdictOf(stdout)).toMatch(
        /^UNKNOWN: SWEEP_FILLS_FILE was made at another deployment: the member's own projects were created at other times there \(fills [0-9a-f]{12}, here [0-9a-f]{12}\)$/
      );
      expect(stdout).not.toMatch(/^ok {6}GET /m);
      expect(code).toBe(2);
    },
    rowLimit("short", "short")
  );

  // A list that gives no creation time of the member's own project tells no deployment: read as
  // it is, it would read alike at every deployment, and the fills and a runner would compare
  // nothing. Fills mode writes no file from it, and a runner stops before it calls a row.
  test(
    "fills mode at a deployment that gives no creation time of the member's own project writes no file, and is UNKNOWN",
    async () => {
      const { code, file, stdout } = await fills([], { createdAt: null });
      expect(verdictOf(stdout)).toBe(
        "UNKNOWN: GET /api/v1/projects with SWEEP_FULL_TOKEN gives no creation time of each of the member's own projects, which tells the deployment, so no fills were written"
      );
      expect(code).toBe(2);
      expect(await Bun.file(file).exists()).toBe(false);
    },
    rowLimit("short")
  );

  test(
    "a runner at a deployment that gives no creation time of the member's own project is UNKNOWN before it calls a row, though the fills name that deployment",
    async () => {
      const { file } = await fills();
      // The deployment a list with no creation time read as before such a list was refused:
      // `OWN null`, alike at every deployment.
      const text = readFileSync(file, "utf8");
      const timeless = text.replace(
        /^deployment\t[0-9a-f]{64}$/m,
        `deployment\t${sha256Hex("OWN null\n")}`
      );
      expect(timeless).not.toBe(text);
      writeFileSync(file, timeless);
      const { code, stdout } = await runner(file, [], { createdAt: null, live: false });
      expect(verdictOf(stdout)).toBe(
        "UNKNOWN: GET /api/v1/projects as the member gives no creation time of each of their own projects, so the run cannot tell the deployment SWEEP_FILLS_FILE was made at from this one"
      );
      expect(stdout).not.toMatch(/^ok {6}GET /m);
      expect(code).toBe(2);
    },
    rowLimit("short", "short")
  );

  // Whole fills hold every fill: a runner given fills without one would leave the rows that read
  // it out, the proxy reads or the direct message's, and pass on the rest.
  test.each(["proxy_repo", "dm_message"])(
    "a runner whose fills hold no %s line is UNKNOWN, naming it, before its first request",
    async (name) => {
      const { file } = await fills();
      without(file, (line) => line.startsWith(`${name}\t`));
      const { code, memberPaths, stdout } = await runner(file);
      expect(verdictOf(stdout)).toBe(
        `UNKNOWN: SWEEP_FILLS_FILE lacks fills that whole fills hold: ${name}`
      );
      expect(memberPaths).toEqual([]);
      expect(code).toBe(2);
    },
    rowLimit("short", "runnerSweep")
  );

  test(
    "fills mode that cannot prove a row writes no file, and is UNKNOWN",
    async () => {
      const first = await fills();
      // A second run at the same path, where the lookup finds no handoff, must remove the first.
      const { code, file, stdout } = await fills([], { omit: new Set(["handoff"]) }, first.file);
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: no fills were written");
      expect(stdout).toContain("\n  handoff: no issue of the first 1 holds a handoff\n");
      expect(code).toBe(2);
      expect(await Bun.file(file).exists()).toBe(false);
    },
    rowLimit("short", "short")
  );

  // The browser leg's runner: its person holds no session, so the member's own control is their
  // comment and message on an issue of their own project, in place of a direct message to a
  // session, and the runner makes those two writes alone.
  test(
    "a runner whose member's control is their own issue passes, writes on that issue alone, and prints a live window",
    async () => {
      const { file } = await fills();
      const { code, fullRequests, memberWrites, stdout } = await runner(file, [], {
        env: issueMode,
      });
      expect(fullRequests).toBe(0);
      expect(memberWrites).toEqual([
        `POST /api/v1/issues/${ownIssue}/comments`,
        `POST /api/v1/issues/${ownIssue}/messages`,
      ]);
      expect(stdout).toMatch(
        new RegExp(
          `^ok {6}/ws/live: the member is sent 2 events in ${runnerLiveSeconds} s, none of TGT, and their own comment and message on ${ownIssue}; its control is a writer's write delivered inside the live window$`,
          "m"
        )
      );
      expect(stdout).toContain(
        `\nok      GET /api/v1/messages/{id} (the member's own message on ${ownIssue}): 200 (control)\n`
      );
      expect(stdout).toMatch(/^live window \S+ \S+$/m);
      expect(verdictOf(stdout)).toMatch(
        new RegExp(
          `^${literally(passed)}; the live check holds once a writer's delivered write falls inside the live window \\S+ \\S+, which the tailnet leg joins$`
        )
      );
      expect(code, stdout).toBe(0);
    },
    rowLimit("short", "runnerSweep")
  );

  // The control of the row above: the same runner, its member's own writes cut from their feed.
  test(
    "the same runner, its member's feed sent neither of their own writes on the issue, is UNKNOWN",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await runner(file, [], {
        env: issueMode,
        feed: { ownIssueLost: ["comment", "message"] },
      });
      expect(stdout).toContain(
        `\nUNKNOWN /ws/live: the control did not hold: the member was not sent their own comment and message on ${ownIssue}\n`
      );
      expect(stdout).not.toMatch(/^live window /m);
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short", "runnerSweep")
  );

  // Each write is a comment on the write issue and a direct message to the fills' session of the
  // project, and is delivered once the full member's own feed is sent both, within the delivery
  // bound a runner's window leaves. Its line names when it began, before its comment was asked
  // for, and when it ended, once its direct message was answered: the join holds the whole write
  // inside a runner's window, so a comment committed before the member's feed subscribed counts
  // for no window.
  test(
    "a writer prints each write, from before its comment to after its direct message, delivered to the full member's own feed, and exits 0",
    async () => {
      const { file } = await fills();
      const { code, directBy, stdout } = await writer(file, "3");
      const writes = stdout.split("\n").filter((line) => line.startsWith("write "));
      expect(writes.length, stdout).toBeGreaterThan(0);
      // Its direct messages are the full member's token's: the writer is given no cookie.
      expect(directBy).toEqual(writes.map(() => "token"));
      for (const line of writes) {
        expect(line).toMatch(new RegExp(`^${writeLine} delivered$`));
        const [, began = "", ended = ""] = line.split(" ");
        expect(began <= ended, line).toBe(true);
      }
      expect(stdout).toContain(
        `\nwriter  a comment on ${tgt.issue} and a direct message to ${tgt.session} every 1 s for 3 s, each held to the full member's own feed for 5 s\n`
      );
      expect(verdictOf(stdout)).toBe(
        `WRITER: ${writes.length} writes, each delivered to the full member's own feed`
      );
      expect(code).toBe(0);
    },
    rowLimit("short", "short")
  );

  // However seldom it writes, a writer waits no longer than the delivery bound a runner's window
  // leaves for a write to reach the feeds: a write delivered later could reach the member's feed
  // after it closed.
  test(
    "a writer that writes less often than the delivery bound holds each write to that bound",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await writer(file, "1", undefined, "20");
      expect(stdout).toContain(
        `\nwriter  a comment on ${tgt.issue} and a direct message to ${tgt.session} every 20 s for 1 s, each held to the full member's own feed for ${deliverySeconds} s\n`
      );
      expect(code, stdout).toBe(0);
    },
    rowLimit("short", "short")
  );

  // The control of the row above: a feed that is sent the project's events but not the comment.
  test(
    "a writer whose comment the full member's feed is not sent prints it not delivered, and exits 1",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await writer(file, "1", "another event of the project");
      expect(stdout).toMatch(new RegExp(`^${writeLine} not delivered$`, "m"));
      expect(verdictOf(stdout)).toMatch(/^WRITER: \d+ writes, \d+ not delivered/);
      expect(code).toBe(1);
    },
    rowLimit("short", "short")
  );
});
