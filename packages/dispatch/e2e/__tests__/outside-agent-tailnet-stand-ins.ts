/** The stand-ins `outside-agent-acceptance.test.ts` runs the tailnet leg against: a git checkout
 *  whose commits the pins name, a deployment repository and a workflow behind a stand-in `gh`, an
 *  `agent-secrets` and an `agent-secrets-helper` that act as the broker would, a Dispatch that
 *  names its serving build and answers the full member's browser session, a read sweep and a public
 *  leg. `prepareStandIn` makes one run ready; it starts no leg. The test file runs each leg itself,
 *  through `runBounded`, so `e2e/row-limits.ts` reads every row that waits for one. Each helper that
 *  makes a directory takes the test file's own `scratch`, whose directories that file's hooks
 *  remove. */

import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import {
  closedPort,
  copyLeg,
  type LegRun,
  publicRows,
  rowsOf,
  type Scratch,
  script,
  tailnetLeg,
  textToolPath,
} from "./outside-agent-fixture";

/* The tailnet leg against stand-ins of what it drives, so the rows it judges by its own controls
 * can be read PASS, FAIL and UNKNOWN: a git checkout whose commits the pins name; a deployment
 * repository the pins are read from through a stand-in `gh`; an `agent-secrets` and an
 * `agent-secrets-helper` that act as a broker holding the enrollment refusal would; a Dispatch
 * that names its serving build and answers the full member's browser session, as the private
 * origin does; a read sweep beside the leg; and a public leg that approves the scratch login and
 * writes the TSV and artifacts the workflow would. No listener is named but in the T5 row, so the
 * peer does not start, and P4, P5, P6 and T5 read UNKNOWN in every other run here. */

/** The broker's refusal of a restricted member's enrollment, as the scratch helper prints it. */
export const memberRefusal =
  "403 RESTRICTED_MEMBER_ENROLLS_NOTHING: a restricted member enrolls nothing";
export const fullLogin = "full@example.com";
export const proxyRepository = "example-owner/private-repo";
const deployRepository = "example-owner/example-deploy";
export const workflowRepository = "example-owner/example-workflows";
/** The inputs `outside-edge-e2e.yaml` declares under `workflow_dispatch`, in the deployment
 *  repository at 0b57ecd8ba; GitHub refuses a dispatch carrying any other with a 422. */
export const workflowInputs = [
  "checks",
  "invite_url",
  "project",
  "peer_session",
  "tailnet_user_code",
  "sweep_project",
  "sweep_fills",
];
export const inviteUrl = "https://dispatch.edge.example/enroll#example-invite-token";

export interface StandIns {
  /** Whether the stand-in Dispatch approves the full member's machine login and serves the GitHub
   *  proxy's read of the sweep's repository, or refuses both. */
  readonly controls: "answered" | "refused";
  /** What the scratch helper's canary session, and the canary's request, are refused with. */
  readonly canaryRefusal: string;
  /** Whether Dispatch's build holds every commit OUTSIDE_DISPATCH_COMMITS names. */
  readonly dispatchCommits: "held" | "lacking";
  /** Whether OUTSIDE_FULL_COOKIE_FILE is given, and the proxy read the fills leave out, if any. */
  readonly cookie?: "given" | "absent";
  readonly fillsLeaveOut?: string;
  /** Whether the public leg approves the scratch login; a login nobody approves ends `expired`. */
  readonly approve?: boolean;
  /** The verdict the public leg's revoke row of the scratch login reads (default PASS). */
  readonly revokeVerdict?: string;
  /** The public leg's own last word: its verdict row (none for "absent") and its exit. */
  readonly publicVerdict?: "PASS" | "FAIL" | "absent";
  readonly publicExit?: number;
  /** P6's row as the public leg writes it, where it differs from a PASS. */
  readonly p6?: string;
  /** Where the pins come from: the deployment repository through `gh` (default), or local files.
   *  `moves` has the public leg change Dispatch's pin at that repository's main while it runs. */
  readonly pins?: "repository" | "local" | "moves";
  /** The commit the stand-in Dispatch's /healthz names: its pin's (default), another one, or its
   *  pin's on this process while `fleet_commit` names another. */
  readonly serving?: "pinned" | "other" | "split";
  /** The T5 window: a scripted peer that sees the window open, an owner removal, and a public
   *  T5-agent row whose last answered trade is `lastAnsweredAfter` s after the leg started. */
  readonly window?: { readonly lastAnsweredAfter: number };
  /** How the public leg starts: the local command (default), or the workflow through `gh`, its
   *  other inputs those `OUTSIDE_WORKFLOW_INPUTS` holds (default the invite link alone). */
  readonly workflow?: { readonly inputs?: string };
}

export interface StandInRun extends LegRun {
  /** Each request the stand-in Dispatch answered, `METHOD path`. */
  readonly seen: readonly string[];
  readonly state: string;
  readonly repo: Checkout;
}

export interface Checkout {
  readonly dir: string;
  /** A commit every image's commit holds. */
  readonly held: string;
  /** The commit every image is built from, a child of `held`. */
  readonly built: string;
  /** Another child of `held`, which `built` does not hold. */
  readonly apart: string;
}

function git(dir: string, ...args: string[]): string {
  const ran = Bun.spawnSync(["git", "-C", dir, "-c", "init.defaultBranch=main", ...args], {
    env: {
      PATH: process.env.PATH ?? "",
      HOME: dir,
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_AUTHOR_NAME: "example",
      GIT_AUTHOR_EMAIL: "example@example.com",
      GIT_COMMITTER_NAME: "example",
      GIT_COMMITTER_EMAIL: "example@example.com",
    },
  });
  if (!ran.success) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString().trim();
}

function checkout(scratch: Scratch): Checkout {
  const dir = scratch("checkout");
  git(dir, "init", "-q", "--template=");
  git(dir, "commit", "-q", "--allow-empty", "-m", "held");
  const held = git(dir, "rev-parse", "HEAD");
  git(dir, "commit", "-q", "--allow-empty", "-m", "built");
  const built = git(dir, "rev-parse", "HEAD");
  const apart = git(dir, "commit-tree", `${held}^{tree}`, "-p", held, "-m", "apart");
  return { dir, held, built, apart };
}

/** A PATH of the text tools, the few more the stand-in run reads with, and the stand-ins of
 *  `agent-secrets`, `agent-secrets-helper` and `gh`, which keep their state in `state`. The `gh`
 *  stand-in runs `publicCommand` as the workflow's run. */
function standInPath(scratch: Scratch, state: string, publicCommand: string): string {
  const bin = textToolPath(scratch, ["git", "base64", "sh", "printenv", "cp", "bun"]);
  // The helper's one duty here is its socket, which the leg waits for; Bun makes it.
  script(
    join(bin, "agent-secrets-helper"),
    `#!/usr/bin/env bash\nexec '${process.execPath}' -e 'Bun.listen({ unix: process.env.AGENT_SECRETS_HELPER_SOCK, socket: { data() {} } });'\n`
  );
  // `gh api repos/<repo>/contents/<path>?ref=main --jq .content` reads the deployment
  // repository's main: the file under state/deploy, base64. `gh workflow run` keeps its words in
  // state/workflow-args and its stdin in state/workflow-inputs.json, refuses an input the
  // workflow does not declare as GitHub does (422), and otherwise starts the public command as the
  // run, with the code the inputs carry; `gh run view`, `watch` and `download` read that run.
  script(
    join(bin, "gh"),
    `#!/usr/bin/env bash
state='${state}'
case $1 in
  api)
    path=\${2#repos/${deployRepository}/contents/}
    path=\${path%%\\?ref=main}
    [ -f "$state/deploy/$path" ] || exit 1
    base64 <"$state/deploy/$path" | tr -d '\\n'
    echo
    ;;
  workflow)
    printf '%s\\n' "$@" >"$state/workflow-args"
    cat >"$state/workflow-inputs.json"
    unexpected=$(DECLARED='${workflowInputs.join(" ")}' '${process.execPath}' -e 'const declared = new Set(process.env.DECLARED.split(" ")); const inputs = JSON.parse(await Bun.stdin.text()); console.log(Object.keys(inputs).filter((name) => !declared.has(name)).join(","));' <"$state/workflow-inputs.json")
    if [ -n "$unexpected" ]; then
      echo "could not create workflow dispatch event: HTTP 422: Unexpected inputs provided: [\\"$unexpected\\"]" >&2
      exit 1
    fi
    inputs=$(cat "$state/workflow-inputs.json")
    code=\${inputs#*\\"tailnet_user_code\\":\\"}
    code=\${code%%\\"*}
    mkdir -p "$state/run"
    (
      OUTSIDE_TAILNET_USER_CODE=$code OUTSIDE_PUBLIC_RESULTS="$state/run" bash '${publicCommand}'
      echo "$?" >"$state/run-exit"
    ) >/dev/null 2>&1 </dev/null &
    echo "https://github.com/${workflowRepository}/actions/runs/4242"
    ;;
  run)
    case "$2 $*" in
      'view '*'--json status'*) if [ -f "$state/run-exit" ]; then echo completed; else echo in_progress; fi ;;
      'view '*'--json conclusion'*) if [ "$(cat "$state/run-exit")" = 0 ]; then echo success; else echo failure; fi ;;
      'watch '*) while [ ! -f "$state/run-exit" ]; do sleep 0.2; done ;;
      'download '*)
        name='' dir=''
        while [ "$#" -gt 0 ]; do
          case $1 in -n) name=$2 && shift ;; -D) dir=$2 && shift ;; esac
          shift
        done
        for file in "$state/run/$name".*; do [ ! -f "$file" ] || cp "$file" "$dir/"; done
        ;;
    esac
    ;;
  *) exit 1 ;;
esac
`
  );
  // A session of the box's own (no operator file) or of the full member's helper is enrolled and
  // granted the canary; the test person's scratch helper's is refused, the first register for the
  // member kind and the canary's session and request with what canary-refusal holds. Each register
  // of T3's session leaves the operator it ran under in state/t3-operator. A machine login
  // completes once the stand-in Dispatch or the public leg marks its code approved, and where the
  // state holds login-expires it ends as the real one ends a code nobody approved.
  script(
    join(bin, "agent-secrets"),
    `#!/usr/bin/env bash
state='${state}'
operator=''
[ -z "\${AGENT_SECRETS_OPERATOR_FILE:-}" ] || operator=$(cat "$AGENT_SECRETS_OPERATOR_FILE")
case $operator in ${fullLogin}) code=FULL-0001 ;; *) code=TEST-0001 ;; esac
granted=no
case $operator in '' | ${fullLogin}) granted=yes ;; esac
case $1 in
  machine)
    if [ "$2" = login ]; then
      echo "machine login code: $code"
      if [ -f "$state/login-expires" ] && [ "$code" = TEST-0001 ]; then
        sleep 1
        echo 'agent-secrets machine login: expired' >&2
        exit 1
      fi
      while [ ! -f "$state/approved-$code" ] && [ -e "$AGENT_SECRETS_HELPER_SOCK" ]; do sleep 0.1; done
      [ -f "$state/approved-$code" ] || exit 1
      printf '{"credential_id":"cred-%s"}\\n' "$(printf '%s' "$code" | tr 'A-Z' 'a-z')" >"$AGENT_SECRETS_MACHINE_FILE"
    elif [ "\${3:-}" = --json ]; then
      printf '[{"credential_id":"cred-%s","approved_by":"%s"}]\\n' "$(printf '%s' "$code" | tr 'A-Z' 'a-z')" "$operator"
    elif [ -f "$state/revoked" ]; then
      echo 'agent-secrets: LAUNCHER_INVALID' >&2
      exit 1
    fi
    ;;
  register)
    session=$3
    case $session in *-t3-*) printf '%s\\n' "$operator" >"$state/t3-operator" ;; esac
    while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
    shift
    [ "$granted" = no ] || exec "$@"
    case $session in
      *-t2b-*) echo "agent-secrets: register $session: $(cat "$state/canary-refusal")" >&2 ;;
      *) echo "agent-secrets: register $session: ${memberRefusal}" >&2 ;;
    esac
    "$@"
    exit 1
    ;;
  whoami) [ "$granted" = yes ] || exit 1 ;;
  request) [ "$granted" = yes ] || { cat "$state/canary-refusal" >&2; exit 1; } ;;
  *)
    [ "$granted" = yes ] || exit 1
    name=$1
    shift 2
    export "$name=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    exec "$@"
    ;;
esac
`
  );
  return bin;
}

export interface StandInDispatch {
  readonly origin: string;
  /** Each request it answered, `METHOD path`. */
  readonly seen: readonly string[];
  stop(): void;
}

/** A Dispatch whose /healthz names the commit `serving` says, which answers the full member's
 *  browser session (cookie `session=full`, with the origin's own Origin) on the routes the leg's
 *  controls read, and 401 to anything else. Its access list names the test person until
 *  `POST /_remove`, the owner's removal. */
function standInDispatch(
  state: string,
  controls: StandIns["controls"],
  repo: Checkout,
  serving: NonNullable<StandIns["serving"]>
): StandInDispatch {
  const seen: string[] = [];
  let listed = true;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      const path = url.pathname;
      seen.push(`${request.method} ${path}`);
      if (path === "/healthz") {
        const commit = serving === "other" ? repo.apart : repo.built;
        return Response.json({
          ok: true,
          commit,
          fleet_commit: serving === "split" ? repo.apart : commit,
        });
      }
      if (path === "/_remove") {
        listed = false;
        return Response.json({});
      }
      if (
        request.headers.get("cookie") !== "session=full" ||
        request.headers.get("origin") !== url.origin
      ) {
        return Response.json({ code: "UNAUTHENTICATED" }, { status: 401 });
      }
      const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
      const code = typeof body.code === "string" ? body.code : "";
      const route = `${request.method} ${path}`;
      if (route === "GET /api/v1/whoami") {
        return Response.json({ kind: "user", login: fullLogin, scopes: ["person", "approve"] });
      }
      if (route === "GET /api/v1/projects/EXAMPLE/access") {
        return Response.json({ people: listed ? ["outside-e2e@example.com"] : [] });
      }
      if (route === "POST /api/v1/me/agent-tokens") {
        return Response.json(
          { id: "token-1", name: body.name, token: "dsp_plaintext" },
          { status: 201 }
        );
      }
      if (route === "DELETE /api/v1/me/agent-tokens/token-1")
        return new Response(null, { status: 204 });
      if (route === "POST /api/v1/credential-requests/machine-lookup") {
        return Response.json({ record_id: `record-${code}` });
      }
      if (route === `POST /api/v1/credential-requests/record-${code}/approve`) {
        if (controls === "refused") return Response.json({ code: "FORBIDDEN" }, { status: 403 });
        writeFileSync(join(state, `approved-${code}`), "");
        return Response.json({ credential_id: `cred-${code.toLowerCase()}` });
      }
      if (route.startsWith("POST /api/v1/machine-logins/cred-")) return Response.json({});
      if (route === `GET /api/github/rest/repos/${proxyRepository}/actions/runs`) {
        return controls === "answered"
          ? Response.json({ total_count: 0, workflow_runs: [] })
          : Response.json({ code: "GITHUB_TOKEN_UNAVAILABLE" }, { status: 503 });
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  });
  return { origin: server.url.origin, seen, stop: () => server.stop(true) };
}

/** A copy of the tailnet leg ready to run against the stand-ins, with a stand-in read sweep beside
 *  it and a public leg that approves the scratch login and writes what the workflow's would. Every
 *  synchronous step (the checkout's git, the files, the links) is here, before any leg starts: the
 *  stand-in Dispatch answers on this process's event loop, which a `spawnSync` would hold while a
 *  leg running beside it waits for an answer. */
export interface PreparedStandIn {
  readonly leg: string;
  readonly env: Record<string, string>;
  readonly dispatch: StandInDispatch;
  readonly state: string;
  readonly repo: Checkout;
}

/** The stand-in probe beside the tailnet leg for the T5 row: its scripted peer is ready at once
 *  and is shown the public leg's `t5-open`. */
const standInPeerProbe = `
const [command] = Bun.argv.slice(2);
if (command === "peer") {
  process.stdout.write(JSON.stringify({ ready: true }) + "\\n");
  process.stdout.write("outside-e2e t5-open example at 1\\n");
  await Bun.sleep(600000);
} else {
  process.stdout.write(JSON.stringify({ unknown: "the stand-in probe has no " + command }) + "\\n");
}
`;

export function prepareStandIn(scratch: Scratch, standIns: StandIns): PreparedStandIn {
  const state = scratch("state");
  writeFileSync(join(state, "canary-refusal"), `${standIns.canaryRefusal}\n`);
  if (standIns.approve === false) writeFileSync(join(state, "login-expires"), "");
  // The fills the sweep's fills mode writes: the repository, and a `can_fail` line for each proxy
  // read, but only with the full member's cookie, as restricted-sweep.sh does.
  const canFail = [
    "GET /api/github/rest/repos/{owner}/{repo}/pulls/{number}",
    "GET /api/github/rest/repos/{owner}/{repo}/issues/{number}",
    "GET /api/github/rest/repos/{owner}/{repo}/commits/{sha}/check-runs",
    "GET /api/github/rest/repos/{owner}/{repo}/actions/runs?head_sha={sha}",
    "GET /api/v1/github/app-installation/{owner}/{repo}",
  ].filter((read) => read !== standIns.fillsLeaveOut);
  writeFileSync(
    join(state, "fills"),
    [
      "restricted-sweep-fills\t1",
      `proxy_repo\t${proxyRepository}`,
      ...canFail.map((read) => `can_fail\t${read}`),
      "",
    ].join("\n")
  );
  // The public leg's TSV: P1-P10 PASS but where a row is given, its revocations, its verdict, and
  // in the T5 row a T5-agent row whose last answered trade the public command stamps.
  const tab = "\t";
  const publicTsv = [
    ["row", "want", "got", "verdict"],
    ...publicRows.map((name) =>
      name === "P6" && standIns.p6 !== undefined
        ? [name, `the row ${name}`, standIns.p6, standIns.p6.split(" ")[0] as string]
        : [name, `the row ${name}`, `what ${name} read`, "PASS"]
    ),
    ["revocations", "every login revoked", "the revoke command exited 0", "PASS"],
    ...(standIns.publicVerdict === "absent"
      ? []
      : [["verdict", "every row PASS", "as read", standIns.publicVerdict ?? "PASS"]]),
  ]
    .map((fields) => fields.join(tab))
    .join("\n");
  writeFileSync(join(state, "public.tsv"), `${publicTsv}\n`);
  const repo = checkout(scratch);
  const inputs = scratch("inputs");
  const leg = copyLeg(scratch, tailnetLeg);
  const legDir = leg.slice(0, leg.lastIndexOf("/"));
  if (standIns.window !== undefined) {
    writeFileSync(join(legDir, "outside-agent-probe.ts"), standInPeerProbe);
  }
  // The writer is stopped by the leg when the public leg ends: on a TERM it leaves a mark and
  // ends. A writer the leg failed to stop is killed later by the bounded child's own stop, with
  // a SIGKILL, which leaves no mark.
  script(
    join(legDir, "restricted-sweep.sh"),
    `#!/usr/bin/env bash
case $SWEEP_MODE in
  fills)
    [ -n "\${SWEEP_FULL_COOKIE:-}" ] || { echo 'UNKNOWN: SWEEP_FULL_COOKIE is required'; exit 2; }
    : >'${state}/fills-read-the-cookie'
    cat '${state}/fills' >"$SWEEP_FILLS_FILE"
    ;;
  writer)
    echo 'write 2026-01-01T00:00:05Z delivered'
    sleep 600 &
    child=$!
    trap 'kill "$child"; : >"${state}/writer-stopped"; exit 0' TERM
    wait "$child"
    ;;
esac
`
  );
  // The pins at the deployment repository's main (state/deploy), or local files.
  mkdirSync(join(state, "deploy"), { recursive: true });
  const pinsLocal = standIns.pins === "local";
  const pins: string[] = [];
  for (const name of ["broker", "dispatch", "edge-listener"]) {
    writeFileSync(
      join(state, "deploy", `${name}.yaml`),
      `example:image: registry.example/${name}@sha256:0\n`
    );
    pins.push(
      `${name} ${pinsLocal ? join(state, "deploy", `${name}.yaml`) : `${name}.yaml`} example:image`
    );
  }
  writeFileSync(join(inputs, "pins.txt"), `${pins.join("\n")}\n`);
  const approve =
    standIns.approve === false ? "" : `: >'${state}/approved-'"$OUTSIDE_TAILNET_USER_CODE"`;
  const moves =
    standIns.pins === "moves"
      ? `echo 'example:image: registry.example/dispatch@sha256:1' >'${state}/deploy/dispatch.yaml'`
      : "";
  const lastAnswered =
    standIns.window === undefined
      ? ""
      : `printf 'T5-agent\\tthe agent part\\tno trade refused; last_answered_at=%s refused_at=none\\tUNKNOWN\\n' "$(( $(date -u +%s) + ${standIns.window.lastAnsweredAfter} ))" >>"$out/outside-agent-public.tsv"`;
  const publicLegCommand = script(
    join(inputs, "public.sh"),
    `#!/usr/bin/env bash
${approve}
${moves}
out=$OUTSIDE_PUBLIC_RESULTS
cp '${state}/public.tsv' "$out/outside-agent-public.tsv"
${lastAnswered}
printf 'revoke-cred-%s\\trevoked\\tstatus 200\\t%s\\n' "$(printf '%s' "$OUTSIDE_TAILNET_USER_CODE" | tr 'A-Z' 'a-z')" '${standIns.revokeVerdict ?? "PASS"}' >"$out/outside-edge-revocations.tsv"
: >'${state}/revoked'
printf 'the workflow note\\nlive window 2026-01-01T00:00:00Z 2026-01-01T00:00:09Z\\nPASS: every read refused\\n' >"$out/restricted-sweep.log"
echo 0 >"$out/restricted-sweep.exit"
${standIns.window === undefined ? "" : "sleep 3"}
exit ${standIns.publicExit ?? 0}
`
  );
  const revision = script(join(inputs, "revision.sh"), `#!/bin/sh\necho ${repo.built}\n`);
  writeFileSync(join(inputs, "cookie"), "session=full\n");
  writeFileSync(join(inputs, "token"), "example-token\n");
  const dispatch = standInDispatch(state, standIns.controls, repo, standIns.serving ?? "pinned");
  const remove = script(
    join(inputs, "remove.sh"),
    `#!/bin/sh\nexec curl -q -sS -X POST '${dispatch.origin}/_remove' -o /dev/null\n`
  );
  const env = {
    PATH: standInPath(scratch, state, publicLegCommand),
    HOME: scratch("home"),
    ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
    OUTSIDE_ALLOW_HTTP: "1",
    OUTSIDE_PINS: join(inputs, "pins.txt"),
    ...(pinsLocal ? {} : { OUTSIDE_PINS_REPO: deployRepository }),
    OUTSIDE_IMAGE_REVISION_COMMAND: `sh ${revision}`,
    OUTSIDE_LEGION_CHECKOUT: repo.dir,
    OUTSIDE_ENROLL_REFUSAL_COMMIT: repo.held,
    OUTSIDE_DISPATCH_COMMITS:
      standIns.dispatchCommits === "held" ? repo.held : `${repo.held} ${repo.apart}`,
    OUTSIDE_TAILNET_BROKER_URL: `http://127.0.0.1:${closedPort()}`,
    OUTSIDE_TAILNET_DISPATCH_URL: dispatch.origin,
    ...(standIns.cookie === "absent" ? {} : { OUTSIDE_FULL_COOKIE_FILE: join(inputs, "cookie") }),
    OUTSIDE_PERSON: "outside-e2e@example.com",
    OUTSIDE_CANARY: "EXAMPLE_CANARY",
    OUTSIDE_REPOSITORY: "example-owner/example-repo",
    OUTSIDE_SWEEP_PROJECT: "PRIV",
    OUTSIDE_SWEEP_FULL_TOKEN_FILE: join(inputs, "token"),
    OUTSIDE_SWEEP_CONVERSATION_FILE: "example-session/example-slug",
    OUTSIDE_SWEEP_WRITE_ISSUE: "PRIV-1",
    ...(standIns.workflow === undefined
      ? { OUTSIDE_PUBLIC_COMMAND: `bash ${publicLegCommand}` }
      : {
          OUTSIDE_WORKFLOW_REPO: workflowRepository,
          OUTSIDE_WORKFLOW_INPUTS: standIns.workflow.inputs ?? `invite_url=${inviteUrl}`,
        }),
    ...(standIns.window === undefined
      ? {}
      : {
          OUTSIDE_PEER: "probe",
          OUTSIDE_LISTENER_URL: `http://127.0.0.1:${closedPort()}`,
          OUTSIDE_OWNER_REMOVE_COMMAND: `sh ${remove}`,
        }),
  };
  return { leg, env, dispatch, state, repo };
}

/** The number of UNKNOWN rows the gate counted, from its `got`. */
export function unknownsCounted(stdout: string): number {
  const counted = /(\d+) UNKNOWN/.exec(rowsOf(stdout).get("gate")?.got ?? "");
  return counted === null ? -1 : Number(counted[1]);
}

export const passingControls: StandIns = {
  controls: "answered",
  canaryRefusal: memberRefusal,
  dispatchCommits: "held",
};
