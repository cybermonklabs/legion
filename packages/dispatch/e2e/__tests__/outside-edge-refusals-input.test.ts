/** What `e2e/acceptance/outside-edge-refusals.sh` refuses before it probes anything: an input, a
 *  list line, a tool missing or too old, and a shell that is not bash. Each refused run must print
 *  its one `UNKNOWN:` line, exit 2, and send the stand-in edge no request. The fixtures, and the
 *  stand-in each row starts, are `outside-edge-refusals-fixtures.ts`'s.
 */

import { describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { rowLimit } from "./bounded-subprocess";
import {
  edgeFixtures,
  passVerdict,
  production,
  routedTable,
  said,
  scriptName,
  standIn,
  withoutTools,
} from "./outside-edge-refusals-fixtures";
import { bash, closedPort, curl, verdict } from "./outside-path-reach-fixtures";

const { check, directoryOf, listFile, planLists } = edgeFixtures(import.meta.url);

// The rows run one after another, not concurrently, for the reason `outside-edge-refusals.test.ts`
// gives: the end of each bounded child reads the whole process table and holds this process's
// event loop meanwhile.
describe(`${scriptName}: the inputs it refuses`, () => {
  /** One input the script refuses before it probes anything: the variables a row sets on top of
   *  the plan's lists and a stand-in address (`null` unsets one, and `list:<text>` is a file
   *  holding that text), and what it prints. In `env`, `{routed}` and `{refusals}` stand for the
   *  plan's two files; in `says`, for the two files the run was given, and `{address}` for the
   *  stand-in's address. */
  const refusedInputs: { name: string; env: Record<string, string | null>; says: string }[] = [
    {
      name: "no OUTSIDE_EDGE_ADDRESS",
      env: { OUTSIDE_EDGE_ADDRESS: null },
      says: "OUTSIDE_EDGE_ADDRESS is unset: set it to the edge's IPv4 address",
    },
    {
      name: "no OUTSIDE_EDGE_ROUTED",
      env: { OUTSIDE_EDGE_ROUTED: null },
      says: "OUTSIDE_EDGE_ROUTED is unset: set it to the file of 'name method sni host path backend' lines, one routed control per backend",
    },
    {
      name: "no OUTSIDE_EDGE_REFUSALS",
      env: { OUTSIDE_EDGE_REFUSALS: null },
      says: "OUTSIDE_EDGE_REFUSALS is unset: set it to the file of 'name method sni host path [405:marker]' lines the edge must refuse",
    },
    ...["edge.example", "010.0.0.1", "192.0.2.1:443", "::1", "https://192.0.2.1"].map(
      (address) => ({
        name: `the address ${address}`,
        env: { OUTSIDE_EDGE_ADDRESS: address },
        says: `OUTSIDE_EDGE_ADDRESS must be an IPv4 address written as four numbers of 0 to 255 with no leading zero, not ${address}`,
      })
    ),
    {
      name: "an http address without OUTSIDE_EDGE_ALLOW_HTTP",
      env: { OUTSIDE_EDGE_ALLOW_HTTP: null },
      says: "OUTSIDE_EDGE_ADDRESS must be the edge's IPv4 address, not {address}: http is for this script's own tests, with OUTSIDE_EDGE_ALLOW_HTTP=1",
    },
    {
      name: "an http address at another host than 127.0.0.1",
      env: { OUTSIDE_EDGE_ADDRESS: "http://10.0.0.1:8080" },
      says: "OUTSIDE_EDGE_ADDRESS may be http only as http://127.0.0.1:<port>, for this script's own tests, not http://10.0.0.1:8080",
    },
    {
      name: "a refusal file that does not exist",
      env: { OUTSIDE_EDGE_REFUSALS: "{refusals}.missing" },
      says: "OUTSIDE_EDGE_REFUSALS names {refusals}, which could not be read: could not open it",
    },
    {
      name: "a refusal line of seven fields",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:a GET dispatch.edge.example dispatch.tailnet.example /x 405:broker extra",
      },
      says: "line 1 of {refusals} is not 'name method sni host path [405:marker]': a GET dispatch.edge.example dispatch.tailnet.example /x 405:broker extra",
    },
    // The sixth field is a method refusal, `405:<marker>`, and nothing else: any other status,
    // a 4xx included, would let a line take an answer the edge forwarded, such as a backend's own
    // 401 or 404, for its refusal; and `none` is the word a probe line prints for no marker.
    ...[
      "405",
      "405:",
      ":broker",
      "40x:broker",
      "099:broker",
      "200:dispatch",
      "302:broker",
      "400:broker",
      "401:dispatch",
      "404:broker",
      "499:listener",
      "500:broker",
      "600:broker",
      "405:br/oker",
      "405:none",
    ].map((field) => ({
      name: `the expected answer ${field}`,
      env: {
        OUTSIDE_EDGE_REFUSALS: `list:a GET dispatch.edge.example dispatch.tailnet.example /x ${field}`,
      },
      says: `line 1 of {refusals}: the expected answer ${field} is not '405:<marker>', the method refusal of a backend whose marker is letters, digits, '.', '_' and '-' and not none`,
    })),
    {
      name: "a routed line of five fields",
      env: {
        OUTSIDE_EDGE_ROUTED: "list:a GET dispatch.edge.example dispatch.edge.example /healthz",
      },
      says: "line 1 of {routed} is not 'name method sni host path backend': a GET dispatch.edge.example dispatch.edge.example /healthz",
    },
    {
      name: "a routed control whose backend is none",
      env: {
        OUTSIDE_EDGE_ROUTED: "list:a GET dispatch.edge.example dispatch.edge.example /healthz none",
      },
      says: "line 1 of {routed}: the backend none is the word a probe line prints for an answer with no marker; name the backend another way",
    },
    {
      name: "an address as the Host",
      env: { OUTSIDE_EDGE_REFUSALS: "list:a GET dispatch.edge.example 10.0.0.1 /x" },
      says: "line 1 of {refusals}: the host 10.0.0.1 is not a host name; no route can name an address, so none could be planted for it",
    },
    {
      name: "an address as the SNI",
      env: { OUTSIDE_EDGE_REFUSALS: "list:a GET 10.0.0.1 dispatch.edge.example /x" },
      says: "line 1 of {refusals}: the sni 10.0.0.1 is not a host name",
    },
    {
      name: "a HEAD line",
      env: {
        OUTSIDE_EDGE_REFUSALS: "list:a HEAD dispatch.edge.example dispatch.tailnet.example /x",
      },
      says: "line 1 of {refusals}: the method HEAD is not one of GET, POST, PUT, PATCH, DELETE or OPTIONS",
    },
    {
      name: "a path without its leading slash",
      env: {
        OUTSIDE_EDGE_REFUSALS: "list:a GET dispatch.edge.example dispatch.tailnet.example x/y",
      },
      says: "line 1 of {refusals}: the path x/y is not an absolute path of URL characters",
    },
    {
      name: "a path holding a fragment",
      env: {
        OUTSIDE_EDGE_REFUSALS: "list:a GET dispatch.edge.example dispatch.tailnet.example /x#y",
      },
      says: "line 1 of {refusals}: the path /x#y is not an absolute path of URL characters",
    },
    // The Gateway API refuses a route whose path holds `//` or an encoded slash, so no route can be
    // planted for such a line: each is refused before any probe.
    ...["/a//b", "/a%2fb", "/a%2Fb"].map((path) => ({
      name: `the path ${path}`,
      env: {
        OUTSIDE_EDGE_REFUSALS: `list:a GET dispatch.edge.example dispatch.tailnet.example ${path}`,
      },
      says: `line 1 of {refusals}: the path ${path} holds '//' or an encoded slash, which no route can name, so none could be planted for it`,
    })),
    // A dot segment before any `?`, `.` or `..` with each `%2e` or `%2E` read as `.`: RFC 3986
    // makes `%2e` the same as `.`, so an edge may read the path as the one it reduces to, and curl
    // without `--path-as-is` sends it so (curl 8.14.1 sends `/v1/..?x=1` as `/?x=1`, `/a/.?q=1` as
    // `/a/?q=1` and `/v1/%2e%2e/admin` as `/admin`). Each is refused before any probe, and the
    // stand-in sees no request.
    ...[
      "/a/../blocked",
      "/a/./b",
      "/a/.",
      "/a/..",
      "/..",
      "/.",
      "/a/..?x",
      "/a/.?x",
      "/a/%2e%2e/b",
      "/a/%2E%2e/b",
      "/a/.%2E/b",
      "/a/%2e?x",
    ].map((path) => ({
      name: `the path ${path}`,
      env: {
        OUTSIDE_EDGE_REFUSALS: `list:a GET dispatch.edge.example dispatch.tailnet.example ${path}`,
      },
      says: `line 1 of {refusals}: the path ${path} holds a dot segment before any '?' ('.' or '..', '%2e' read as '.'), which an edge may read as another path, so none could be planted for it`,
    })),
    {
      name: "two lines of one name",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:a GET dispatch.edge.example dispatch.tailnet.example /x\na GET dispatch.edge.example dispatch.tailnet.example /y",
      },
      says: "line 2 of {refusals}: the name a is another line's; give each line a name of its own",
    },
    {
      name: "a refusal line named as a routed control",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:dispatch-healthz GET dispatch.edge.example dispatch.tailnet.example /x",
      },
      says: "line 1 of {refusals}: the name dispatch-healthz is another line's; give each line a name of its own",
    },
    {
      name: "a byte outside printable ASCII",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:# caf\u00e9\na GET dispatch.edge.example dispatch.tailnet.example /x",
      },
      says: "line 1 of {refusals} holds byte 0xC3, which is not printable ASCII or a tab: write the list in ASCII, with LF or CRLF line endings",
    },
    {
      // bash's `read -d ''` stops at a NUL, so the line holding it is named from the count of the
      // LFs before it.
      name: "a NUL byte",
      env: {
        OUTSIDE_EDGE_REFUSALS:
          "list:# one\na GET dispatch.edge.example dispatch.tailnet.example /x\u0000y",
      },
      says: "line 2 of {refusals} holds byte 0x00, which is not printable ASCII or a tab: write the list in ASCII, with LF or CRLF line endings",
    },
    {
      name: "a connect bound under 2 s",
      env: { OUTSIDE_EDGE_CONNECT_SECONDS: "1" },
      says: "OUTSIDE_EDGE_CONNECT_SECONDS must be a whole number of seconds from 2 to 999999, not 1",
    },
    {
      name: "a read bound that is no number",
      env: { OUTSIDE_EDGE_READ_SECONDS: "ten" },
      says: "OUTSIDE_EDGE_READ_SECONDS must be a whole number of seconds from 2 to 999999, not ten",
    },
  ];
  for (const { name, env, says } of refusedInputs) {
    test.skipIf(withoutTools)(
      `refuses ${name}, before it probes anything`,
      async () => {
        const edge = await standIn(production);
        try {
          const files = planLists();
          const given: Record<string, string | undefined> = {
            OUTSIDE_EDGE_ADDRESS: edge.address,
            ...files,
          };
          for (const [variable, value] of Object.entries(env)) {
            if (value === null) given[variable] = undefined;
            else if (value.startsWith("list:")) given[variable] = listFile([value.slice(5)]);
            else
              given[variable] = value
                .replace("{refusals}", files.OUTSIDE_EDGE_REFUSALS ?? "")
                .replace("{routed}", files.OUTSIDE_EDGE_ROUTED ?? "");
          }
          const expected = says
            .replace("{address}", edge.address)
            .replace("{refusals}", given.OUTSIDE_EDGE_REFUSALS ?? "")
            .replace("{routed}", given.OUTSIDE_EDGE_ROUTED ?? "");
          const run = await check(`refuses ${name}`, given, { bound: "short" });
          expect(run.stdout, said(run)).toBe(`UNKNOWN: ${expected}\n`);
          expect(run.code, said(run)).toBe(2);
          expect(await edge.seen()).toEqual([]);
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the short bound: it stops before any probe.
      rowLimit("short")
    );
  }

  test.skipIf(withoutTools)(
    "refuses a refusal file it cannot read whole, in bash's own words",
    async () => {
      const directory = directoryOf("directory");
      const run = await check(
        "unreadable",
        {
          OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`,
          OUTSIDE_EDGE_ROUTED: listFile(routedTable),
          OUTSIDE_EDGE_REFUSALS: directory,
        },
        { bound: "short" }
      );
      expect(run.stdout, said(run)).toStartWith(
        `UNKNOWN: OUTSIDE_EDGE_REFUSALS names ${directory}, which could not be read whole: `
      );
      expect(run.stdout, said(run)).toContain("Is a directory");
      expect(run.code, said(run)).toBe(2);
    },
    // The script, a bounded child under the short bound: it stops before any probe.
    rowLimit("short")
  );

  // curl 8.4.0 is the oldest the script takes: before it, `--max-filesize` does not bound a body
  // whose length the answer did not give first, so such a body past 1 MiB would be read whole. The
  // row's `curl` answers `--version` as the version it names and hands every other call to the
  // host's curl. 8.10.1 is taken, so the version is compared as numbers, not as text.
  for (const { version, taken } of [
    { version: "7.88.1", taken: false },
    { version: "8.3.0", taken: false },
    { version: "8.4.0", taken: true },
    { version: "8.10.1", taken: true },
  ]) {
    test.skipIf(withoutTools)(
      taken
        ? `takes curl ${version}`
        : `reads UNKNOWN for curl ${version}, older than 8.4.0, before it probes anything`,
      async () => {
        const directory = directoryOf("path");
        writeFileSync(
          join(directory, "curl"),
          [
            `#!${bash}`,
            `case " $* " in *" --version "*) echo 'curl ${version} (x86_64-pc-linux-gnu) libcurl/${version}'; exit 0 ;; esac`,
            `exec ${curl} "$@"`,
            "",
          ].join("\n"),
          { mode: 0o755 }
        );
        const edge = await standIn(production);
        try {
          const run = await check(
            `curl ${version}`,
            { OUTSIDE_EDGE_ADDRESS: edge.address, ...planLists() },
            { path: `${directory}:${process.env.PATH ?? ""}` }
          );
          if (taken) {
            expect(run.stdout.split("\n"), said(run)).toContain(
              `outside-edge-refusals: curl ${version}`
            );
            expect(verdict(run), said(run)).toBe(passVerdict);
            expect(run.code, said(run)).toBe(0);
          } else {
            expect(run.stdout, said(run)).toBe(
              `UNKNOWN: curl missing or too old: this curl is '${version}'; install curl 8.4.0 or later\n`
            );
            expect(run.code, said(run)).toBe(2);
            expect(await edge.seen()).toEqual([]);
          }
        } finally {
          await edge.stop();
        }
      },
      // The script, a bounded child under the bound of a run that probes: a curl it takes is
      // probed with.
      rowLimit("edgeRun")
    );
  }

  for (const shell of ["dash", "zsh"]) {
    test.skipIf(withoutTools || hostTool(shell) === null)(
      `refuses to run under ${shell}, naming bash`,
      async () => {
        const run = await check(
          `shell ${shell}`,
          { OUTSIDE_EDGE_ADDRESS: `http://127.0.0.1:${await closedPort()}`, ...planLists() },
          { shell: hostTool(shell) ?? shell, bound: "short" }
        );
        expect(run.stdout, said(run)).toBe(`UNKNOWN: run this with bash: bash ${scriptName}\n`);
        expect(run.code, said(run)).toBe(2);
      },
      // The script, a bounded child under the short bound: it stops at its first lines.
      rowLimit("short")
    );
  }
});
