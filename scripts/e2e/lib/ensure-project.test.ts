import { beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// ensure_project, write_dispatch_header and dispatch_agent (lib/workflow.sh) against a fake curl on
// PATH: it logs every invocation, writes FAKE_CURL_BODY to whatever file follows -o, prints
// FAKE_CURL_STATUS to stdout (curl's own -w '%{http_code}' convention, since -o diverts the body),
// and on a nonzero FAKE_CURL_EXIT prints FAKE_CURL_ERROR to stderr and 000 to stdout and writes
// nothing, as real curl does when the connection fails (no response, so -w writes the status as
// 000).
const lib = join(import.meta.dir, "workflow.sh");
const dir = temporaryDirectory("ensure-project-test");
beforeAll(() => {
  writeFileSync(
    join(dir, "curl"),
    `#!/usr/bin/env bash
out=
args=("$@")
for ((i = 0; i < \${#args[@]}; i++)); do [ "\${args[i]}" = -o ] && out=\${args[i + 1]}; done
printf 'curl %s\\n' "$*" >>"$FAKE_CURL_LOG"
if [ "\${FAKE_CURL_EXIT:-0}" != 0 ]; then
  printf '%s\\n' "\${FAKE_CURL_ERROR:-curl: (7) Failed to connect}" >&2
  printf '000'
  exit "$FAKE_CURL_EXIT"
fi
[ -z "$out" ] || printf '%s' "$FAKE_CURL_BODY" >"$out"
printf '%s' "$FAKE_CURL_STATUS"
`,
    { mode: 0o755 }
  );
});

// The stage scripts' shell options, ERR trap, and check helpers, then the lib. note, fail and
// scrub live in stage4b-sandbox-tree.sh, not workflow.sh, so the prelude stands in for them: note
// is stage4b's own, fail matches stage4b's exact wording, and scrub is the identity, which the
// redaction rows below replace with a stand-in that marks what passes through it, or with
// stage4b's own definition.
const prelude = `set -Eeuo pipefail
check=prerequisites
note() { printf '   %s\\n' "$*"; }
fail() { printf 'CHECK %s: FAIL: %s\\n' "$check" "$*" >&2; exit 1; }
scrub() { cat; }
trap 'printf "CHECK %s: FAIL: line %s exited %s: %s\\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
dispatch_base=https://dispatch.example
repo=sjawhar/legion-smoke ptoken=s3test ok=
. "$LIB"
`;

let runs = 0;
// The proof human's session every write names as its actor, as stage4b-sandbox-tree.sh sets it.
const proofActor = "legion-e2e4b-proof-human-4242";
// The person a created project names as its default assignee, which stage4b-sandbox-tree.sh reads
// from LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE; null leaves dispatch_default_assignee unset.
const proofAssignee = "smoke-owner@example.com";
function run(
  script: string,
  fake: { status?: string; body?: string; exit?: number; error?: string } = {},
  actor = proofActor,
  assignee: string | null = proofAssignee
) {
  const work = join(dir, `run-${++runs}`);
  const log = join(dir, `calls-${runs}`);
  const tokenFile = join(dir, `token-${runs}`);
  writeFileSync(log, "");
  writeFileSync(tokenFile, "stage4b-fake-bearer\n", { mode: 0o600 });
  mkdirSync(work);
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      prelude +
        `work=${JSON.stringify(work)} dispatch_token_file=${JSON.stringify(tokenFile)} project=LPROOF2 dispatch_actor=${JSON.stringify(actor)}\n` +
        (assignee === null ? "" : `dispatch_default_assignee=${JSON.stringify(assignee)}\n`) +
        script,
    ],
    {
      env: {
        PATH: `${dir}:/usr/bin:/bin`,
        HOME: dir,
        LIB: lib,
        FAKE_CURL_LOG: log,
        FAKE_CURL_STATUS: fake.status ?? "",
        FAKE_CURL_BODY: fake.body ?? "",
        FAKE_CURL_EXIT: String(fake.exit ?? 0),
        FAKE_CURL_ERROR: fake.error ?? "",
      },
    }
  );
  return {
    status: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    work,
    calls: readFileSync(log, "utf8").split("\n").filter(Boolean),
    bearer: (() => {
      try {
        return readFileSync(join(work, "dispatch-human-header"), "utf8");
      } catch {
        return null;
      }
    })(),
  };
}

describe("write_dispatch_header", () => {
  test("writes dispatch-auth-header and dispatch-human-header identically from the token file", () => {
    const r = run("write_dispatch_header");
    expect(r.status).toBe(0);
    expect(r.bearer).toBe("Authorization: Bearer stage4b-fake-bearer\n");
  });
});

describe("ensure_project", () => {
  // Production Dispatch answers a bearer that names no session 400 ACTOR_KIND before it reads the
  // key, so a body without the actor would fail every run against it; and it answers a bearer's
  // create naming no person 400 INVALID_PROJECT, since that bearer acts for nobody, so the body
  // names the run's default assignee too.
  test("201: a created project is ready; the body posts {key, name}, the default assignee and the proof human's actor", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "201",
      body: '{"key":"LPROOF2","name":"LPROOF2","created_at":"2026-10-11T00:00:00Z"}',
    });
    expect(r.status).toBe(0);
    expect(r.stdout).toContain("created project LPROOF2");
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain("-X POST");
    expect(r.calls[0]).toContain("/api/v1/projects");
    // The bearer goes by the header file write_dispatch_header wrote, never in argv.
    expect(r.calls[0]).toContain(`-H @${r.work}/dispatch-human-header `);
    expect(r.bearer).toBe("Authorization: Bearer stage4b-fake-bearer\n");
    expect(r.calls[0]).toContain(
      `{"key":"LPROOF2","name":"LPROOF2","default_assignee":"${proofAssignee}","actor":{"kind":"session","id":"${proofActor}"}}`
    );
  });

  // The create resolves the person before it reads the key, so a body naming none is refused
  // 400 INVALID_PROJECT for a project that already exists as well; ensure_project refuses it
  // first, naming the operator's input, and makes no call.
  test("an unset default assignee fails before any call, naming LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE", () => {
    const r = run("write_dispatch_header\nensure_project", { status: "201" }, proofActor, null);
    expect(r.status).toBe(1);
    expect(r.calls).toHaveLength(0);
    expect(r.stderr.trimEnd().split("\n")).toEqual([
      "CHECK prerequisites: FAIL: LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE is unset (dispatch_default_assignee): Dispatch refuses a project the agents' bearer creates naming no default assignee, so LPROOF2 cannot be ensured",
    ]);
  });

  test("an empty default assignee is refused as unset", () => {
    const r = run("write_dispatch_header\nensure_project", { status: "201" }, proofActor, "");
    expect(r.status).toBe(1);
    expect(r.calls).toHaveLength(0);
    expect(r.stderr).toContain("LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE is unset");
  });

  test("the body carries the person the caller names, and only that person", () => {
    const r = run(
      "write_dispatch_header\nensure_project",
      {
        status: "201",
        body: '{"key":"LPROOF2","name":"LPROOF2","default_assignee":"lane-two@example.com"}',
      },
      proofActor,
      "lane-two@example.com"
    );
    expect(r.status).toBe(0);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain('"default_assignee":"lane-two@example.com"');
    expect(r.calls[0]).not.toContain(proofAssignee);
  });

  test("an empty dispatch_actor posts {key, name, default_assignee} with no actor, as a scratch Dispatch's human header writes", () => {
    const r = run(
      "write_dispatch_header\nensure_project",
      { status: "201", body: '{"key":"LPROOF2","name":"LPROOF2"}' },
      ""
    );
    expect(r.status).toBe(0);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain(
      `{"key":"LPROOF2","name":"LPROOF2","default_assignee":"${proofAssignee}"}`
    );
    expect(r.calls[0]).not.toContain("actor");
  });

  test("a refused actor (400 ACTOR_KIND) fails loudly, naming the status and the body", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "400",
      body: '{"error":"bearer callers require actor.kind session","code":"ACTOR_KIND"}',
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("answered 400");
    expect(r.stderr).toContain('"code":"ACTOR_KIND"');
  });

  test("409 PROJECT_EXISTS: an existing project is also ready", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "409",
      body: '{"error":"a project with this key already exists","code":"PROJECT_EXISTS"}',
    });
    expect(r.status).toBe(0);
    expect(r.stdout).toContain("project LPROOF2 already exists");
    expect(r.calls[0]).toContain(`"default_assignee":"${proofAssignee}"`);
  });

  // A refused create's body reaches the FAIL line through the caller's scrub, the one filter that
  // keeps the run's private hosts and its default assignee out of what it prints, on both lines
  // that print a body: here a stand-in that brackets whatever passes through it.
  for (const [status, body, line] of [
    [
      "400",
      `{"error":"\\"${proofAssignee}\\" has not signed in to Dispatch","code":"ASSIGNEE_NOT_ALLOWED"}`,
      "Dispatch answered 400 creating project LPROOF2: ",
    ],
    [
      "409",
      '{"error":"something else entirely","code":"SOMETHING_ELSE"}',
      "Dispatch answered 409 creating LPROOF2, not PROJECT_EXISTS: ",
    ],
  ] as const) {
    test(`${status}: the refusal's body is printed through scrub`, () => {
      const r = run(
        `scrub() { printf 'scrubbed['; cat; printf ']'; }\nwrite_dispatch_header\nensure_project`,
        { status, body }
      );
      expect(r.status).toBe(1);
      expect(r.stderr.trimEnd().split("\n")).toEqual([
        `CHECK prerequisites: FAIL: ${line}scrubbed[${body}]`,
      ]);
    });
  }

  // A person nobody signed in as is refused before the key is read, so even an existing project
  // answers it rather than 409. Dispatch's body names the email it refused as the operator spelled
  // it (allowedLogin, packages/envoy/internal/dispatch/api/issue_assignee.go), and stage4b's own
  // scrub, taken from its text, prints that spelling as the variable's name and the rest of the
  // line as it was, so a run's log carries no person's email.
  const stage4bScrub = readFileSync(
    join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"),
    "utf8"
  ).match(/^scrub\(\) \{\n[\s\S]*?\n\}$/m)?.[0];
  const operatorSpelling = "Smoke.Owner+e2e@Example.com";

  test("400 ASSIGNEE_NOT_ALLOWED through stage 4b's scrub names the variable, never the email", () => {
    expect(stage4bScrub).toBeDefined();
    const r = run(
      `${stage4bScrub}\nservice_hosts=(dispatch.example:443)\nwrite_dispatch_header\nensure_project`,
      {
        status: "400",
        body: `{"error":"\\"${operatorSpelling}\\" has not signed in to Dispatch","code":"ASSIGNEE_NOT_ALLOWED"}`,
      },
      proofActor,
      operatorSpelling
    );
    expect(r.status).toBe(1);
    expect(r.stderr.trimEnd().split("\n")).toEqual([
      'CHECK prerequisites: FAIL: Dispatch answered 400 creating project LPROOF2: {"error":"\\"<LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE>\\" has not signed in to Dispatch","code":"ASSIGNEE_NOT_ALLOWED"}',
    ]);
  });

  // Dispatch writes its error bodies with Go's encoding/json, which escapes `&`, `<` and `>` as
  // `\u0026`, `\u003c` and `\u003e`: measured against a local Dispatch at 4198dff4, a create
  // naming "a&b@example.com" answered `"\"a\u0026b@example.com\" has not signed in to Dispatch"`.
  // An email may hold `&`, so the body spells it differently from the variable, and the FAIL line
  // must still name the variable and never the email.
  test("400 ASSIGNEE_NOT_ALLOWED for an email Dispatch's JSON escapes still names the variable, never the email", () => {
    expect(stage4bScrub).toBeDefined();
    const person = "Smoke&Owner@Example.com";
    const r = run(
      `${stage4bScrub}\nservice_hosts=(dispatch.example:443)\nwrite_dispatch_header\nensure_project`,
      {
        status: "400",
        body: '{"code":"ASSIGNEE_NOT_ALLOWED","error":"\\"Smoke\\u0026Owner@Example.com\\" has not signed in to Dispatch"}',
      },
      proofActor,
      person
    );
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("<LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE>");
    expect(r.stderr).not.toContain("Owner@Example.com");
  });

  // The email is matched in any case, as the hosts are, and literally: each character sed's regular
  // expression or its `#` delimiter reads as special stands for itself, so a line that differs only
  // where the email holds `.` keeps it, and a host still names its own variable.
  test("stage 4b's scrub matches the default assignee in any case and literally, beside the hosts", () => {
    expect(stage4bScrub).toBeDefined();
    const person = "a.b*c[d]^e$f#g\\h+i@Example.com";
    const line = `${person.toUpperCase()} aXb*c[d]^e$f#g\\h+i@Example.com DISPATCH.example:443 ${person}`;
    const result = Bun.spawnSync(
      [
        "bash",
        "-c",
        `${stage4bScrub}\nservice_hosts=(dispatch.example:443)\ndispatch_default_assignee=$PERSON\nprintf '%s\\n' "$LINE" | scrub`,
      ],
      { env: { PATH: "/usr/bin:/bin", PERSON: person, LINE: line } }
    );
    expect(result.exitCode).toBe(0);
    expect(result.stdout.toString()).toBe(
      "<LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE> aXb*c[d]^e$f#g\\h+i@Example.com <LEGION_E2E_DISPATCH_URL>:443 <LEGION_E2E_DISPATCH_DEFAULT_ASSIGNEE>\n"
    );
  });

  // A 409 could in principle mean something other than this key's own conflict; ensure_project
  // must not read any 409 as success, only the one the code names.
  test("409 with another code fails loudly, naming the status and the body", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "409",
      body: '{"error":"something else entirely","code":"SOMETHING_ELSE"}',
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("409");
    expect(r.stderr).toContain("not PROJECT_EXISTS");
    expect(r.stderr).toContain('"code":"SOMETHING_ELSE"');
  });

  test("500 fails loudly, naming the status and the body", () => {
    const r = run("write_dispatch_header\nensure_project", {
      status: "500",
      body: '{"error":"internal error"}',
    });
    expect(r.status).toBe(1);
    expect(r.stderr).toContain("500");
    expect(r.stderr).toContain('{"error":"internal error"}');
  });

  // Real curl prints 000 on stdout when the connection fails; the FAIL line carries curl's error
  // alone, on one line.
  test("a connection failure fails loudly as unreachable, in one line, never as a refusal", () => {
    const r = run("write_dispatch_header\nensure_project", {
      exit: 7,
      error: "curl: (7) Failed to connect to dispatch.example port 443",
    });
    expect(r.status).toBe(1);
    expect(r.stderr.trimEnd().split("\n")).toEqual([
      "CHECK prerequisites: FAIL: Dispatch could not be reached to create LPROOF2: curl: (7) Failed to connect to dispatch.example port 443",
    ]);
  });

  // ensure_project is the run's first write anywhere: it creates the project in production
  // Dispatch when the identity has none. A development switch is read from the operator's
  // environment alone, so a mistyped one must cost that write nothing — a run refused for its
  // value leaves production as it found it.
  test("stage 4b refuses every development switch above the call that creates the project", () => {
    const lines = readFileSync(
      join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"),
      "utf8"
    ).split("\n");
    const creates = lines.indexOf("ensure_project");
    expect(creates).toBeGreaterThan(-1);
    for (const refusal of [
      '    fail "STAGE4B_UNTIL=$until names no checkpoint of this driver"',
      '  [ -n "$until" ] || fail "STAGE4B_SKIP_CONTROLLER is for a development run: set STAGE4B_UNTIL too"',
      '  [ "$design_gate" = root-issues ] || fail "STAGE4B_DESIGN_GATE=$design_gate: the one policy it arms is root-issues"',
      '  [ "$until" = deployment-pod-block ] || fail "LEGION_E2E_POD_FILE is for a development run stopped at it: set STAGE4B_UNTIL=deployment-pod-block"',
    ]) {
      const at = lines.indexOf(refusal);
      expect(at).toBeGreaterThan(-1);
      expect(at).toBeLessThan(creates);
    }
  });
});

describe("dispatch_agent", () => {
  // A bearer caller must name its session in the body (400 ACTOR_KIND otherwise), and the session
  // dispatch_agent names is the stand-in architect's, never the proof human's dispatch_actor: the
  // write is an agent's, over the agents' bearer rather than the human header.
  test("sends the bearer header file, JSON, the method, the path and the body plus the stand-in session's actor", () => {
    const r = run(
      `write_dispatch_header\ndispatch_agent POST "artifacts/doc-1/edits" '{"ops":[{"op":"insert","after":"end","markdown":"Hello."}]}'`
    );
    expect(r.status).toBe(0);
    expect(r.calls).toHaveLength(1);
    expect(r.calls[0]).toContain("-X POST");
    expect(r.calls[0]).toContain(`-H @${r.work}/dispatch-auth-header `);
    expect(r.calls[0]).not.toContain("dispatch-human-header");
    expect(readFileSync(join(r.work, "dispatch-auth-header"), "utf8")).toBe(
      "Authorization: Bearer stage4b-fake-bearer\n"
    );
    expect(r.calls[0]).toContain("-H content-type: application/json");
    expect(r.calls[0]).toContain(" https://dispatch.example/api/v1/artifacts/doc-1/edits");
    expect(r.calls[0]).toContain(
      '--data {"ops":[{"op":"insert","after":"end","markdown":"Hello."}],"actor":{"kind":"session","id":"stage3-architect-stand-in"}}'
    );
    expect(r.calls[0]).not.toContain(proofActor);
  });
});
