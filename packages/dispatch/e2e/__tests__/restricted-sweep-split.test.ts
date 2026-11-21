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
  failures,
  fullCookie,
  fullToken,
  type Leak,
  literally,
  memberCookie,
  passed,
  routesFileOf,
  stopFakes,
  sweep,
  tgt,
  verdictOf,
  withoutTools,
} from "./restricted-sweep-fake";
import { proxyReads, sha256Hex } from "./restricted-sweep-index";

afterAll(stopFakes);

describe.skipIf(withoutTools)("the split into fills, a runner and a writer", () => {
  const fillsDirectories = temporaryDirectories("restricted-sweep-fills");
  const someRows = ["/api/v1/inbox", "/api/v1/issues/{key}/comments", appInstallation];
  /** The route index a run of `someRows` reads, as `sweep` writes it, and its SHA-256. */
  const someRowsSha256 = sha256Hex(routesFileOf(someRows));
  /** The shortest live window a runner reads, which leaves the writer's delivery bound (15 s) of
   *  it before its feed closes. */
  const runnerLiveSeconds = 30;

  /** A run of fills mode, and the file it was to write. Its proxy reads as the real one does, a
   *  browser's person alone, so its reads can fail only with the full member's cookie, which the
   *  run is given unless `withoutCookie`. Its live socket is served, for the agent view's full
   *  half. `omit` leaves kinds out of its lookup; `createdAt` is when its fake's OWN was created. */
  async function fills(
    leaks: readonly Leak[] = [],
    {
      withoutCookie = false,
      omit,
      createdAt,
    }: { withoutCookie?: boolean; omit?: ReadonlySet<string>; createdAt?: string | null } = {},
    file = join(fillsDirectories(), "fills.tsv")
  ) {
    const run = await sweep(leaks, {
      env: {
        SWEEP_MODE: "fills",
        SWEEP_MEMBER: "bob@outside.example",
        SWEEP_FILLS_FILE: file,
        SWEEP_FULL_COOKIE: withoutCookie ? undefined : fullCookie,
        SWEEP_RESTRICTED_COOKIE: undefined,
        SWEEP_MEMBER_SESSION: undefined,
        SWEEP_WRITE_ISSUE: undefined,
      },
      feed: { live: true },
      ...(omit === undefined ? {} : { omit }),
      ...(createdAt === undefined ? {} : { createdAt }),
      proxyReadsCookies: true,
      rows: someRows,
    });
    return { ...run, file };
  }

  /** A runner's run with FILE and no full member's credential, over `rows` (`someRows` unless
   *  given), its document socket served, and its live feed served unless `live` is false: a run
   *  that a leak decides, or that stops before its feed, need not wait out a live window.
   *  `createdAt` is when its fake's OWN was created. */
  function runner(
    file: string,
    leaks: readonly Leak[] = [],
    {
      live = true,
      rows = someRows,
      createdAt,
    }: { live?: boolean; rows?: readonly string[]; createdAt?: string | null } = {}
  ) {
    return sweep(leaks, {
      env: {
        SWEEP_FILLS_FILE: file,
        SWEEP_FULL_TOKEN: undefined,
        SWEEP_CONVERSATION_FILE: undefined,
        SWEEP_WRITE_ISSUE: undefined,
        SWEEP_LIVE_SECONDS: String(runnerLiveSeconds),
      },
      ...(createdAt === undefined ? {} : { createdAt }),
      feed: { doc: true, live },
      rows,
    });
  }

  /** A writer's run with FILE's fills, writing every WRITE_EVERY seconds (1 unless given) for
   *  WRITE_SECONDS, with the full member's cookie for its direct messages. */
  function writer(file: string, writeSeconds: string, noise?: string, writeEvery = "1") {
    return sweep([], {
      env: {
        SWEEP_MODE: "writer",
        SWEEP_FILLS_FILE: file,
        SWEEP_FULL_COOKIE: fullCookie,
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
      const { code, file, fullRequests, memberPaths, stdout } = await fills();
      expect(verdictOf(stdout)).toStartWith("FILLS: ");
      expect(code, stdout).toBe(0);
      expect(memberPaths).toEqual([]);
      expect(fullRequests).toBeGreaterThan(0);
      const text = readFileSync(file, "utf8");
      const lines = text.trimEnd().split("\n");
      expect(lines[0]).toBe("restricted-sweep-fills\t3");
      expect(lines).toContain("project\tTGT");
      expect(lines.filter((line) => /^origin\thttp:\/\/127\.0\.0\.1:\d+$/.test(line))).toHaveLength(
        1
      );
      // The route index the fills were made from, and the deployment they were made at.
      expect(lines).toContain(`routes\t${someRowsSha256}`);
      expect(lines.filter((line) => /^deployment\t[0-9a-f]{64}$/.test(line))).toHaveLength(1);
      expect(lines).toContain("member\tbob@outside.example");
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
      for (const shape of proxyReads)
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
  // conversation's file.
  test(
    "a runner with those fills and no full member's credential passes, names no private name, and prints a live window that leaves the writer's delivery bound",
    async () => {
      const { file } = await fills();
      const fillsOrigin = /^origin\t(\S+)$/m.exec(readFileSync(file, "utf8"))?.[1] ?? "";
      expect(fillsOrigin).toMatch(/^http:\/\/127\.0\.0\.1:\d+$/);
      const { code, fullRequests, stdout } = await runner(file);
      expect(fullRequests).toBe(0);
      const digestOf = (text: string) => sha256Hex(text).slice(0, 12);
      const member = "bob@outside.example";
      expect(stdout).toContain(
        `\nmember  a restricted member TGT's access list does not name, whose login's SHA-256 starts ${digestOf(member)}\n`
      );
      expect(stdout).toMatch(
        new RegExp(
          `^fills {3}of TGT for the login whose SHA-256 starts ${digestOf(member)}, made -?\\d+ s ago at the origin whose SHA-256 starts ${digestOf(fillsOrigin)}: \\d+ values, \\d+ rows that can fail$`,
          "m"
        )
      );
      for (const name of [
        fillsOrigin,
        member,
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
        (runnerLiveSeconds - 15) * 1000
      );
      expect(stdout).toMatch(/^ok {6}GET \/api\/v1\/issues\/\{key\}\/comments: 404 \(can fail\)$/m);
      expect(stdout).toContain(
        `\nok      GET ${appInstallation}: 200, as a missing one is answered (can fail)\n`
      );
      expect(stdout).toContain(
        "\nok      /ws/live agent view of {session_id}: no one answers for it (can fail)\n"
      );
      expect(verdictOf(stdout)).toMatch(
        new RegExp(
          `^${literally(passed)}; the live check holds once a writer's delivered write falls inside the live window \\S+ \\S+, which the tailnet leg joins$`
        )
      );
      expect(code, stdout).toBe(0);
    },
    rowLimit("short", "short")
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

  // A proxy read the full member's half could not make fail leaves no fills, and a runner whose
  // fills say a proxy read cannot fail is UNKNOWN: neither is a pass of a read that could not fail.
  test(
    "fills mode whose proxy reads cannot fail, with no full member's cookie, writes no file, and names them",
    async () => {
      const { code, file, stdout } = await fills([], { withoutCookie: true });
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: no fills were written");
      for (const shape of proxyReads) {
        expect(stdout).toContain(
          `\n  GET /api/github/rest/repos/{owner}/{repo}/${shape}: the full member is answered 503 for TGT's repository as for one the App is not installed on (503), so the read could not fail: the proxy reads the browser's person alone, whom SWEEP_FULL_COOKIE names\n`
        );
      }
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
        "\nUNKNOWN GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}: the member is answered 503, but SWEEP_FILLS_FILE holds no full member's read of it that could fail\n"
      );
      expect(verdictOf(stdout)).toBe(
        "UNKNOWN: nothing reached the member, but this run did not prove it, for the 1 reasons above"
      );
      expect(code).toBe(2);
    },
    rowLimit("short", "short")
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
    rowLimit("short", "short")
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
    rowLimit("short", "short")
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

  // Each write is a comment on the write issue and a direct message to the fills' session of the
  // project, and is delivered once the full member's own feed is sent both, within the delivery
  // bound a runner's window leaves.
  test(
    "a writer prints each write delivered to the full member's own feed, and exits 0",
    async () => {
      const { file } = await fills();
      const { code, stdout } = await writer(file, "3");
      const writes = stdout.split("\n").filter((line) => line.startsWith("write "));
      expect(writes.length, stdout).toBeGreaterThan(0);
      for (const line of writes) {
        expect(line).toMatch(
          /^write \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ [0-9a-f-]{36} [0-9a-f-]{36} delivered$/
        );
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
        `\nwriter  a comment on ${tgt.issue} and a direct message to ${tgt.session} every 20 s for 1 s, each held to the full member's own feed for 15 s\n`
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
      expect(stdout).toMatch(/^write \S+ [0-9a-f-]{36} [0-9a-f-]{36} not delivered$/m);
      expect(verdictOf(stdout)).toMatch(/^WRITER: \d+ writes, \d+ not delivered/);
      expect(code).toBe(1);
    },
    rowLimit("short", "short")
  );
});
