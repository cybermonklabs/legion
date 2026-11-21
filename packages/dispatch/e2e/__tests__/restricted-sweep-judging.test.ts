/** The judging rules of `e2e/acceptance/restricted-sweep.sh` for its HTTP rows, each held alone
 *  against the fake Dispatch of `restricted-sweep-fake.ts`, which keeps every route from the
 *  restricted member but the one a row plants a leak in. The sockets' rules are
 *  `restricted-sweep-sockets.test.ts`'s, and the split into fills, a runner and a writer is
 *  `restricted-sweep-split.test.ts`'s. */

import { afterAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit } from "./bounded-subprocess";
import {
  appInstallation,
  at,
  failures,
  type Leak,
  literally,
  passed,
  type SweptRun,
  served,
  stopFakes,
  sweep,
  tgt,
  verdictOf,
  withoutTools,
} from "./restricted-sweep-fake";
import { getRows, proxyReads } from "./restricted-sweep-index";

afterAll(stopFakes);

describe.skipIf(withoutTools)("each judging rule, alone", () => {
  test(
    "a 200 for the project's object where a missing one is refused is a FAIL naming that route",
    async () => {
      const { code, stdout } = await sweep(
        [at(`/api/v1/artifacts/${tgt.projectDocument}/asks`, 200, [])],
        { rows: ["/api/v1/artifacts/{id}/asks"] }
      );
      expect(failures(stdout)).toEqual([
        "GET /api/v1/artifacts/{id}/asks: the member is answered 200 for TGT's object, where a missing one is refused 404",
      ]);
      expect(stdout).toContain(
        "\nFAIL: 1 answers gave the member what TGT holds, the first: GET /api/v1/artifacts/{id}/asks: "
      );
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  test(
    "a 200 holding a row of the project, where a missing one is answered 200 too, is a FAIL",
    async () => {
      const { code, stdout } = await sweep(
        [
          at(`/api/v1/issues/${tgt.issue}/comments`, 200, [{ id: tgt.comment }]),
          at("/api/v1/issues/ZZNONE-1/comments", 200, []),
        ],
        { rows: ["/api/v1/issues/{key}/comments"] }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/issues/{key}/comments: the member is answered 200 holding ${tgt.comment}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  test(
    "a refusal other than 403 or 404 for the project's object, where a missing one is refused 404, is a FAIL",
    async () => {
      // As the server writes it (api/owner.go): the issue in prose, and the route it names. The
      // prose names no row; the refusal itself says the document exists, which a missing one's
      // 404 does not.
      const refusal = {
        code: "ARTIFACT_LINKED",
        error: `artifact belongs to issue ${tgt.issue}; use /api/v1/issues/${tgt.issue}/...`,
      };
      const { code, stdout } = await sweep(
        [at(`/api/v1/artifacts/${tgt.projectDocument}/comments`, 400, refusal)],
        { rows: ["/api/v1/artifacts/{id}/comments"] }
      );
      expect(failures(stdout)).toEqual([
        "GET /api/v1/artifacts/{id}/comments: the member is answered 400 for TGT's object, otherwise than a missing one answered 404",
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  test(
    "a list holding a row of the project is a FAIL naming the list",
    async () => {
      const { code, stdout } = await sweep(
        [at("/api/v1/inbox", 200, [{ id: tgt.ask, issue_key: tgt.issue }])],
        { rows: ["/api/v1/inbox"] }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/inbox: the member is answered 200 holding ${tgt.ask}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The route index's text names example issue keys of any project, in prose (the search route's
  // description names one): an issue key inside prose is no row of the project. The list row
  // above is its control, an issue key the server wrote as a whole value.
  test(
    "a route index whose prose names an issue key of the project is no FAIL",
    async () => {
      const index = {
        routes: [
          {
            path: "/api/v1/search",
            description:
              "A q that is one issue key in any form a person writes it - TGT-1042, tgt-1042, TGT 1042.",
          },
        ],
      };
      const { stdout } = await sweep([at("/api/v1", 200, index)], { rows: ["/api/v1"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain("\nok      GET /api/v1: 200, no row of TGT (blind)\n");
    },
    rowLimit("short")
  );

  // Every form a person or an agent cites the project's items by, each in the member's own row
  // (an OWN item of their inbox). Where a person wrote it - inside prose of a `body`, or as the
  // whole `body` and `title` of an OWN comment whose whole text, or first line, is the link or the
  // bare key - it names no row of the project and the run is no FAIL. As the whole value of a
  // member the server writes (`member`: a `ref`, an `href`, a `url`) each form is a row, which
  // names its key. The rows of the two kinds are each other's controls.
  const forms: { form: string; text: string; marker: string; member: string }[] = [
    { form: "a plain issue key", text: tgt.issue, marker: tgt.issue, member: "ref" },
    { form: "a reference", text: `fake-ref://${tgt.issue}`, marker: tgt.issue, member: "ref" },
    { form: "an issue path", text: `/issues/${tgt.issue}`, marker: tgt.issue, member: "href" },
    { form: "a project path", text: "/projects/TGT", marker: "TGT", member: "href" },
    {
      form: "a dashboard URL",
      text: `https://dispatch.example/issues/${tgt.issue}`,
      marker: tgt.issue,
      member: "url",
    },
  ];
  /** The member's own inbox item, OWN's, with `fields` beside its id and issue key. */
  const ownItem = (fields: Record<string, string>) => [
    { id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", issue_key: "OWN-1", ...fields },
  ];

  test.each(forms)(
    "$form inside prose of the member's own row is no FAIL",
    async ({ text }) => {
      const body = `This follows ${text}, the target plan.`;
      const { stdout } = await sweep([at("/api/v1/inbox", 200, ownItem({ body }))], {
        rows: ["/api/v1/inbox"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain("\nok      GET /api/v1/inbox: 200, no row of TGT (can fail)\n");
    },
    rowLimit("short")
  );

  test.each(forms)(
    "$form as the whole body and title a person wrote is no FAIL",
    async ({ text }) => {
      const { stdout } = await sweep(
        [at("/api/v1/inbox", 200, ownItem({ title: text, body: text }))],
        { rows: ["/api/v1/inbox"] }
      );
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain("\nok      GET /api/v1/inbox: 200, no row of TGT (can fail)\n");
    },
    rowLimit("short")
  );

  test.each(forms)(
    "$form as the whole value of a member the server writes is a FAIL naming it",
    async ({ text, marker, member }) => {
      const item = ownItem({ [member]: text });
      const { code, stdout } = await sweep([at("/api/v1/inbox", 200, item)], {
        rows: ["/api/v1/inbox"],
      });
      expect(failures(stdout)).toEqual([
        `GET /api/v1/inbox: the member is answered 200 holding ${marker}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // A list filtered by the project's key answers the member's own row, whose prose names that key:
  // the same answer for the project and for a missing one, which the comparison must read alike,
  // though the key is a fill on one side only. The search route takes the project as a filter.
  test(
    "a filter by the project that answers the member's own prose naming its key, as a missing one is answered, is no FAIL",
    async () => {
      const own = [{ issue_key: "OWN-1", snippet: "See /projects/TGT for the plan." }];
      const searches: Leak = {
        matches: (seen) => seen.startsWith("/api/v1/search?"),
        status: 200,
        body: own,
      };
      const { stdout } = await sweep([searches], { rows: ["/api/v1/search"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nok      GET /api/v1/search?q=<title>&project=: 200, as a missing one is answered (can fail)\n"
      );
    },
    rowLimit("short")
  );

  test(
    "of two leaks, the verdict names the one whose row comes first in the route index",
    async () => {
      const { code, stdout } = await sweep(
        [
          at("/api/v1/inbox", 200, [{ issue_key: tgt.issue }]),
          at(`/api/v1/artifacts/${tgt.projectDocument}/asks`, 200, []),
        ],
        { rows: ["/api/v1/artifacts/{id}/asks", "/api/v1/inbox"] }
      );
      const found = failures(stdout);
      expect(found.map((line) => line.split(":")[0])).toEqual([
        "GET /api/v1/artifacts/{id}/asks",
        "GET /api/v1/inbox",
      ]);
      expect(stdout).toContain(
        `\nFAIL: 2 answers gave the member what TGT holds, the first: ${found[0]}\n`
      );
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  test(
    "a session that worked in the project alone, in the member's agents list, is a FAIL",
    async () => {
      const { code, stdout } = await sweep(
        [at("/api/v1/agents", 200, [{ session_id: tgt.session, title: "Works on TGT" }])],
        { rows: ["/api/v1/agents"] }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/agents: the member is answered 200 holding ${tgt.session}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the same list with no session of the project's is no FAIL.
  test(
    "the member's agents list with no session of the project is no FAIL",
    async () => {
      const { stdout } = await sweep([], { rows: ["/api/v1/agents"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toMatch(/^ok {6}GET \/api\/v1\/agents: 200, no row of TGT \(can fail\)$/m);
    },
    rowLimit("short")
  );

  test(
    "a row of the project in an answer that also measures its own time is a FAIL",
    async () => {
      const { code, stdout } = await sweep(
        [
          at(`/api/v1/search?q=${tgt.title}`, 200, {
            took_ms: 3,
            results: [{ kind: "document", project: "TGT" }],
          }),
        ],
        { rows: ["/api/v1/search"] }
      );
      expect(failures(stdout)).toEqual([
        "GET /api/v1/search?q=: the member is answered 200 holding TGT, a row of TGT",
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  test(
    "a direct message to the project's session, in the member's list of that session's messages, is a FAIL",
    async () => {
      const listed = [
        { message: { id: tgt.directMessage, body: "A direct message to the worker." } },
      ];
      const { code, stdout } = await sweep(
        [at(`/api/v1/agents/${tgt.session}/messages`, 200, listed)],
        { rows: ["/api/v1/agents/{session_id}/messages"] }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/agents/{session_id}/messages: the member is answered 200 holding ${tgt.directMessage}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the same list refused, and the direct message by its own
  // routes refused too, which a run reads whatever routes it is given.
  test(
    "the member refused the project's session's messages and its direct message is no FAIL",
    async () => {
      const { stdout } = await sweep([], { rows: ["/api/v1/agents/{session_id}/messages"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nok      GET /api/v1/agents/{session_id}/messages: 404 (can fail)\n"
      );
      expect(stdout).toContain(
        "\nok      GET /api/v1/messages/{id} (a direct message): 404 (can fail)\n"
      );
      expect(stdout).toContain(
        "\nok      GET /api/v1/messages/{id}/blocks (a direct message): 404 (can fail)\n"
      );
    },
    rowLimit("short")
  );

  test(
    "a GitHub proxy read of the project's repository that the member is answered is a FAIL",
    async () => {
      const pull = { number: 1, title: "The change of the target reef" };
      const { code, stdout } = await sweep(
        [at(`/api/github/rest/repos/${tgt.repository}/pulls/1`, 200, pull)],
        { rows: ["/api/v1/whoami"] }
      );
      expect(failures(stdout)).toEqual([
        "GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}: the member is answered 200 for TGT's repository, where one the App is not installed on is answered 503",
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: each proxy read of the project's repository answered as one
  // the App is not installed on, as the proxy answers it (503 GITHUB_TOKEN_UNAVAILABLE).
  test(
    "each GitHub proxy read answered as a repository the App is not installed on is no FAIL",
    async () => {
      const { stdout } = await sweep([], { rows: ["/api/v1/whoami"] });
      expect(failures(stdout)).toEqual([]);
      expect(proxyReads.length).toBeGreaterThan(0);
      for (const shape of proxyReads) {
        expect(stdout).toContain(
          `\nok      GET /api/github/rest/repos/{owner}/{repo}/${shape}: 503, as a repository the App is not installed on (can fail)\n`
        );
      }
    },
    rowLimit("short")
  );

  // A proxy that reads only a browser's person, as the real one does: with the full member's
  // cookie the reads can fail, and the run passes; with their token alone the reads cannot fail,
  // and a read that cannot fail leaves the run UNKNOWN, never PASS. Both runs have both sockets
  // served, so each proxy read is all that decides between them.
  test(
    "with the full member's cookie, a proxy that reads no token is read as the full member, and the run passes",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        proxyReadsCookies: true,
        rows: ["/api/v1/whoami"],
      });
      for (const shape of proxyReads) {
        expect(stdout).toContain(
          `\nok      GET /api/github/rest/repos/{owner}/{repo}/${shape}: 503, as a repository the App is not installed on (can fail)\n`
        );
      }
      expect(stdout).not.toContain("the-full-members-cookie");
      expect(verdictOf(stdout)).toBe(passed);
      expect(code, stdout).toBe(0);
    },
    rowLimit("short")
  );

  test(
    "without the full member's cookie, a proxy that reads no token leaves the run UNKNOWN, naming each read",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        env: {},
        proxyReadsCookies: true,
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      for (const shape of proxyReads) {
        expect(stdout).toContain(
          `\nUNKNOWN GET /api/github/rest/repos/{owner}/{repo}/${shape}: the member is answered 503, but the full member is answered 503 for TGT's repository as for one the App is not installed on (503), so the read could not fail: the proxy reads the browser's person alone, whom SWEEP_FULL_COOKIE names\n`
        );
      }
      expect(stdout).not.toContain("PASS");
      expect(verdictOf(stdout)).toBe(
        `UNKNOWN: nothing reached the member, but this run did not prove it, for the ${proxyReads.length} reasons above`
      );
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // A proxy read counts as one that can fail only where the full member's read of the project's
  // repository was answered and one nobody holds was answered as the proxy answers a repository
  // the App is not installed on. An outage of either read, or a rate limit, which the proxy
  // answers every repository alike, is no answer: the run is UNKNOWN, and the member's answers,
  // alike as they are, prove nothing.
  const pulls = "/api/github/rest/repos/{owner}/{repo}/pulls/{number}";
  const outages: readonly (readonly [string, number, unknown])[] = [
    ["an outage", 503, { code: "UPSTREAM_UNAVAILABLE" }],
    [
      "a rate limit",
      503,
      { code: "GITHUB_RATE_LIMITED", error: "GitHub rate-limited the App; retry after 1m0s" },
    ],
    ["an edge's server error", 502, "Bad Gateway"],
  ];

  test.each(outages)(
    "%s of the full member's proxy read of the project's repository leaves the read UNKNOWN, though the member is answered alike",
    async (_what, status, body) => {
      const target = `/api/github/rest/repos/${tgt.repository}/pulls/1`;
      const missing = "/api/github/rest/repos/acme/no-such-repo/pulls/1";
      const { code, stdout } = await sweep([at(target, status, body), at(missing, status, body)], {
        ...served,
        full: [at(target, status, body)],
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        `\nUNKNOWN GET ${pulls}: the full member is answered ${status} for TGT's repository, which no rule judges, so its control was not read\n`
      );
      expect(stdout).not.toMatch(new RegExp(`^ok {6}GET ${literally(pulls)}:`, "m"));
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  test.each(outages)(
    "%s answered to the member for both repositories leaves the proxy read UNKNOWN",
    async (_what, status, body) => {
      const target = `/api/github/rest/repos/${tgt.repository}/pulls/1`;
      const missing = "/api/github/rest/repos/acme/no-such-repo/pulls/1";
      const { code, stdout } = await sweep([at(target, status, body), at(missing, status, body)], {
        ...served,
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        `\nUNKNOWN GET ${pulls}: the member is answered ${status}, and ${status} for a repository the App is not installed on, which no rule judges\n`
      );
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // A failed read of the full member's half is no observation: the row it controls is UNKNOWN,
  // never the `blind` of a route the full member is answered no row of, and a row that can fail
  // elsewhere does not make up for it.
  test.each([
    ["a list", "/api/v1/inbox", "/api/v1/inbox"],
    [
      "a filled row",
      "/api/v1/artifacts/{id}/asks",
      `/api/v1/artifacts/${tgt.projectDocument}/asks`,
    ],
  ])(
    "where the full member's read of %s fails, the row is UNKNOWN, never blind",
    async (_what, row, target) => {
      const { code, stdout } = await sweep([], {
        ...served,
        full: [at(target, 503, { code: "UPSTREAM_UNAVAILABLE" })],
        rows: [row],
      });
      expect(stdout).toContain(
        `\nUNKNOWN GET ${row}: the full member is answered 503, so its control was not read: {"code":"UPSTREAM_UNAVAILABLE"}\n`
      );
      expect(stdout).not.toMatch(new RegExp(`^ok {6}GET ${literally(row)}:`, "m"));
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // A list filtered by a session reads what that session wrote: filtered by the project's session
  // alone, the member must be answered as for a session nobody holds.
  test(
    "a list filtered by the project's session that answers the member a row of the project is a FAIL",
    async () => {
      const { code, stdout } = await sweep(
        [at(`/api/v1/asks/open?author_session=${tgt.session}`, 200, [{ id: tgt.ask }])],
        { rows: ["/api/v1/asks/open"] }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/asks/open?author_session=: the member is answered 200 holding ${tgt.ask}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the same filter answered as for a session nobody holds.
  test(
    "a list filtered by the project's session answered as for a session nobody holds is no FAIL, and can fail",
    async () => {
      const { stdout } = await sweep([], { rows: ["/api/v1/asks/open"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain("\nok      GET /api/v1/asks/open?author_session=: 404 (can fail)\n");
    },
    rowLimit("short")
  );

  // The deployment's own route index lists a GET route the route index the sweep walks lacks:
  // the sweep never called it, so its silence there proves nothing. A test hook is no deployment's.
  test(
    "a GET route the deployment lists and routes.txt lacks leaves the run UNKNOWN, naming it",
    async () => {
      const index = {
        routes: [
          { method: "GET", path: "/api/v1/whoami" },
          { method: "GET", path: "/api/v1/a-route-of-a-newer-server" },
          { method: "POST", path: "/api/v1/a-write-of-a-newer-server" },
          { method: "GET", path: "/api/v1/artifacts/_test/rooms" },
        ],
      };
      const { code, stdout } = await sweep([at("/api/v1", 200, index)], {
        ...served,
        rows: ["/api/v1/whoami"],
      });
      expect(stdout.split("\n").filter((line) => line.startsWith("UNKNOWN GET /api/v1/a"))).toEqual(
        [
          "UNKNOWN GET /api/v1/a-route-of-a-newer-server: the deployment lists it and the route index routes.txt does not, so the sweep did not call it",
        ]
      );
      expect(stdout).not.toContain("_test/rooms: the deployment lists it");
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // A public runner's log is public: a FAIL names the route, the status and the row of the
  // project the answer held, and never copies the answer, whose text the leak would republish.
  test(
    "a FAIL names the row of the project the member was answered, and none of the answer's text",
    async () => {
      const words = "words only a member of TGT may read";
      const { code, stdout } = await sweep(
        [at("/api/v1/inbox", 200, [{ issue_key: tgt.issue, body: words }])],
        { rows: ["/api/v1/inbox"] }
      );
      expect(failures(stdout)).toEqual([
        `GET /api/v1/inbox: the member is answered 200 holding ${tgt.issue}, a row of TGT`,
      ]);
      expect(stdout).not.toContain(words);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // A value the lookup found that is a name - the project's repository, a person of the project
  // alone, a file of its conversation - is private text as well: a FAIL names it by its kind, and
  // never as it is.
  test.each([
    ["the project's repository", { repo: tgt.repository }],
    ["a person of the project", { assignee: tgt.person }],
    ["the project's conversation", { file: `${tgt.session}/${tgt.conversationFile}` }],
  ] as const)(
    "a FAIL names %s by its kind, never by its name",
    async (kind, item) => {
      const { code, stdout } = await sweep([at("/api/v1/inbox", 200, [item])], {
        rows: ["/api/v1/inbox"],
      });
      expect(failures(stdout)).toEqual([
        `GET /api/v1/inbox: the member is answered 200 holding ${kind}, a row of TGT`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // An answer the sweep could not mask was not read for rows: its row is UNKNOWN, never a FAIL
  // whose row is the mask's own error. A jq that refuses to mask one answer, and is jq for every
  // other read, stands for any failure of that read.
  const toolDirectories = temporaryDirectories("restricted-sweep-tools");
  const unmaskable = "an answer no jq masks";
  test(
    "an answer the sweep cannot mask leaves its row UNKNOWN, never a FAIL",
    async () => {
      const tools = toolDirectories();
      const jq = Bun.which("jq");
      if (jq === null) throw new Error("no jq on PATH");
      writeFileSync(
        join(tools, "jq"),
        [
          "#!/usr/bin/env bash",
          `# jq, but for the mask of an answer holding "${unmaskable}" (--rawfile body FILE).`,
          "previous=''",
          'for argument in "$@"; do',
          `  if [ "$previous" = body ] && grep -q -F '${unmaskable}' "$argument" 2>/dev/null; then exit 3; fi`,
          "  previous=$argument",
          "done",
          `exec '${jq}' "$@"`,
          "",
        ].join("\n"),
        { mode: 0o755 }
      );
      const { code, stdout } = await sweep(
        [at(`/api/v1/issues/${tgt.issue}/comments`, 200, [{ note: unmaskable }])],
        {
          env: { PATH: `${tools}:${process.env.PATH ?? ""}` },
          rows: ["/api/v1/issues/{key}/comments"],
        }
      );
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nUNKNOWN GET /api/v1/issues/{key}/comments: the member's answer could not be masked, so it was not read for rows of TGT\n"
      );
      expect(stdout).not.toMatch(/^ok {6}GET \/api\/v1\/issues\/\{key\}\/comments:/m);
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // GET /api/v1/github/app-installation/{owner}/{repo} answers a repository its caller may not see
  // as one the App is not installed on. Filled with TGT's repository, which the App is installed
  // on, the member must be answered exactly as for a missing repository; where the App is installed
  // on none, the full member is answered so too, the row cannot fail, and it proves nothing. Each
  // run has both sockets served, so the row decides it.
  test(
    "a route that answers a hidden repository as a missing one, answering the member that the App is installed on the project's, is a FAIL",
    async () => {
      const { code, stdout } = await sweep(
        [at(`/api/v1/github/app-installation/${tgt.repository}`, 200, { installed: true })],
        { ...served, rows: [appInstallation] }
      );
      expect(failures(stdout)).toEqual([
        `GET ${appInstallation}: the member is answered 200 for TGT's object, otherwise than a missing one answered 200`,
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the same route answering the member as for a missing repository.
  test(
    "a route that answers a hidden repository as a missing one, so answering the member, can fail, and the run passes",
    async () => {
      const { code, stdout } = await sweep([], { ...served, rows: [appInstallation] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        `\nok      GET ${appInstallation}: 200, as a missing one is answered (can fail)\n`
      );
      expect(verdictOf(stdout)).toBe(passed);
      expect(code, stdout).toBe(0);
    },
    rowLimit("short")
  );

  // The member's own project resolves to a repository of the project's issues too: the App is
  // installed on it, and the member may see it, so a correct server answers them installed there.
  // The row is filled with a repository only the project's issues resolve to, as the GitHub proxy
  // reads are, so that answer fails nothing.
  test(
    "a repository the member's own project shares with the project fills no row held to a missing one's answer",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        rows: [appInstallation],
        sharedRepo: true,
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toMatch(/^found {3}proxy-repository acme\/tgt-repo$/m);
      expect(stdout).toContain(
        `\nok      GET ${appInstallation}: 200, as a missing one is answered (can fail)\n`
      );
      expect(verdictOf(stdout)).toBe(passed);
      expect(code, stdout).toBe(0);
    },
    rowLimit("short")
  );

  test(
    "where the App is installed on none of the repositories, a route that answers a hidden repository as a missing one is UNKNOWN, never a pass",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        notInstalled: true,
        rows: [appInstallation],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).not.toContain(`\nok      GET ${appInstallation}:`);
      expect(stdout).toContain(
        `\nUNKNOWN GET ${appInstallation}: the member is answered 200, as a missing one is, but the full member is answered TGT's object as a missing one is (200), so the row could not fail\n`
      );
      expect(verdictOf(stdout)).toBe(
        "UNKNOWN: nothing reached the member, but this run did not prove it, for the 1 reasons above"
      );
      expect(code).toBe(2);
    },
    rowLimit("short")
  );
});

/** The one run of this file over the whole route index, with nothing planted, which two rows read:
 *  it makes about 400 calls. */
let quiet: Promise<SweptRun> | undefined;
function quietRun(): Promise<SweptRun> {
  quiet ??= sweep([], { bound: "wholeIndexSweep" });
  return quiet;
}

describe.skipIf(withoutTools)("a run that proves nothing", () => {
  test(
    "with every route refused but neither socket proved, is UNKNOWN, never PASS",
    async () => {
      const { code, stdout } = await quietRun();
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nUNKNOWN: nothing reached the member, but this run did not prove it, for the "
      );
      expect(stdout).toMatch(/^UNKNOWN \/ws\/doc\/\{room\}: /m);
      expect(stdout).toContain(
        "\nUNKNOWN /ws/live: the full member's feed did not stay subscribed: it was sent http 404\n"
      );
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(2);
    },
    rowLimit("wholeIndexSweep")
  );

  test(
    "where the lookup finds no object of one kind, is UNKNOWN and names the kind",
    async () => {
      const { code, stdout } = await sweep([], {
        omit: new Set(["handoff"]),
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toMatch(/^missing handoff: no issue of the first 1 holds a handoff$/m);
      expect(stdout).toContain("\n  handoff: no issue of the first 1 holds a handoff\n");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  test(
    "where no conversation file is named, is UNKNOWN and names both rows it could not fill",
    async () => {
      const { code, stdout } = await sweep([], {
        env: { SWEEP_CONVERSATION_FILE: undefined },
        rows: [
          "/api/v1/agents/{session_id}/artifacts/{slug}",
          "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}",
        ],
      });
      for (const pattern of [
        "/api/v1/agents/{session_id}/artifacts/{slug}",
        "/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}",
      ]) {
        expect(stdout).toContain(`\n  GET ${pattern} is not filled: {session_id}: `);
      }
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  test(
    "where the named conversation file is missing to the full member too, is UNKNOWN and says so",
    async () => {
      const { code, stdout } = await sweep([], {
        conversationGone: true,
        rows: ["/api/v1/agents/{session_id}/artifacts/{slug}"],
      });
      expect(stdout).toContain(
        `\n  GET /api/v1/agents/{session_id}/artifacts/{slug} is not filled: {session_id}: the full member is answered 404 for SWEEP_CONVERSATION_FILE's ${tgt.session}/${tgt.conversationFile}\n`
      );
      expect(code).toBe(2);
    },
    rowLimit("short")
  );
});

describe.skipIf(withoutTools)("the lookup", () => {
  test(
    "finds an object on any of the 50 issues it lists, the 30th included",
    async () => {
      const { stdout } = await sweep([], {
        issues: 30,
        handoffOn: "TGT-30",
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toMatch(new RegExp(`^found {3}handoff ${tgt.handoff}$`, "m"));
      expect(stdout).not.toMatch(/^missing handoff/m);
    },
    rowLimit("short")
  );
});

describe.skipIf(withoutTools)("the routes it calls", () => {
  test(
    "the member is sent every GET row of the route index",
    async () => {
      const { memberPaths, stdout } = await quietRun();
      expect(stdout).not.toMatch(/^not filled /m);
      expect(stdout).toMatch(/, 0 not filled;/);
      // Each GET pattern as the paths it fills: a wildcard is one path segment.
      const unsent = getRows.filter((pattern) => {
        const shape = new RegExp(`^${pattern.replace(/\{[^}]+\}/g, "[^/]+")}$`);
        return !memberPaths.some((path) => shape.test(decodeURIComponent(path)));
      });
      expect(unsent).toEqual([]);
    },
    rowLimit("wholeIndexSweep")
  );
});
