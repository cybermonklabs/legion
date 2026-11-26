/** The stand-ins `outside-agent-acceptance.test.ts` runs the tailnet leg against: a git checkout
 *  whose commits the pins name, a deployment repository and a workflow behind a stand-in `gh`, an
 *  `agent-secrets` and an `agent-secrets-helper` that act as the broker would, a Dispatch that
 *  names its serving build and answers the full member's browser session, a read sweep and a public
 *  leg. `prepareStandIn` makes one run ready; it starts no leg. The test file runs each leg itself,
 *  through `runBounded`, so `e2e/row-limits.ts` reads every row that waits for one. Each helper that
 *  makes a directory takes the test file's own `scratch`, whose directories that file's hooks
 *  remove. */

import { randomUUID } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  copyLeg,
  type LegRun,
  publicRows,
  rowsOf,
  type Scratch,
  script,
  tailnetLeg,
  textToolPath,
} from "./outside-agent-fixture";
import { closedPort } from "./outside-path-reach-fixtures";

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
 *  repository at 0b57ecd8ba, with the browser gate's three, `routed_controls`,
 *  `sweep_member_issue` and `grants_probe_id`, which that repository's pull requests for the
 *  browser gate add; GitHub refuses a dispatch carrying any other with a 422. */
export const workflowInputs = [
  "checks",
  "invite_url",
  "project",
  "peer_session",
  "tailnet_user_code",
  "sweep_project",
  "sweep_fills",
  "routed_controls",
  "sweep_member_issue",
  "grants_probe_id",
];
/** The full member's personal token the stand-in Dispatch takes (inputs/token). */
export const fullToken = "example-token";
/** The grant of the stand-in full member's list that no person may end: a pod's, got without
 *  asking, with no approver and no operator; the probe grant the browser gate passes. */
export const serviceGrant = "5f0c3a9b-1d2e-4f3a-8b4c-6d7e8f9a0b1c";
export const inviteUrl = "https://dispatch.edge.example/enroll#example-invite-token";
/** The ids of the stand-in writer's comments (`c1`, `c2`) and direct messages (`m1`, `m2`), in the
 *  lowercase 8-4-4-4-12 form of the ids Dispatch gives them. */
export const writeIds = {
  c1: "3f1c2a9e-8b7d-4c11-9e2a-5d6f7a8b9c0d",
  m1: "7a2b9c1d-3e4f-4a5b-8c6d-1e2f3a4b5c6d",
  c2: "1b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b",
  m2: "9e8d7c6b-5a49-4382-9716-05f4e3d2c1b0",
} as const;

/** The full member's credential-grants lists the stand-in Dispatch answers (`grants`), each a
 *  plant of the browser gate's grants control: `default`, the service grant beside the full
 *  member's own; `no-everyone`, their own alone, so no grant every signed-in person is listed;
 *  `judged-only`, their own, one the model judge approved for `anyone` on another person's session,
 *  and an automatic grant on the full member's own session, so no grant has no approver and no
 *  operator. */
export type FullGrants = "default" | "no-everyone" | "judged-only";

/** The stand-in full member's list of credential grants, as the broker's compact JSON. */
export function fullGrantsList(kind: FullGrants): string {
  const grant = (id: string, granted: string, operator: string, approver: string | null) => ({
    grant_id: id,
    granted,
    record_id: approver === null ? null : `record-${id.slice(0, 8)}`,
    enrollment: { kind: "pod", runtime_id: `pod-${id.slice(0, 8)}`, operator, slot: null },
    names: ["EXAMPLE_SECRET"],
    approver,
    decided_by: approver,
    expires_at: null,
    created_at: "2026-01-01T00:00:00Z",
  });
  const own = grant("0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "approval", fullLogin, fullLogin);
  const grants = {
    default: [grant(serviceGrant, "automatic", "", null), own],
    "no-everyone": [own],
    "judged-only": [
      own,
      grant("1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f", "approval", "other@example.com", "anyone"),
      grant("2d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a", "automatic", fullLogin, null),
    ],
  }[kind];
  return JSON.stringify({ grants });
}

export interface StandIns {
  /** Whether the stand-in Dispatch approves the full member's machine login and serves the GitHub
   *  proxy's read of the sweep's repository, or refuses both. */
  readonly controls: "answered" | "refused";
  /** What the scratch helper's canary session, and the canary's request, are refused with. */
  readonly canaryRefusal: string;
  /** What OUTSIDE_DISPATCH_COMMITS holds: commits Dispatch's build holds, one it lacks too, only
   *  blanks, or a word that is no commit id beside a held one. */
  readonly dispatchCommits: "held" | "lacking" | "blank" | "malformed";
  /** Whether OUTSIDE_FULL_COOKIE_FILE is given, and the proxy read the fills leave out, if any. */
  readonly cookie?: "given" | "absent";
  readonly fillsLeaveOut?: string;
  /** The stand-in Dispatch's GitHub proxy refuses the full member's token: `no-identity` 401
   *  NO_IDENTITY, as a build before cybermonklabs/legion#5079 answers every token, its proxy
   *  reading a browser's person alone; `no-person-scope` 403 OAUTH_SCOPE_REQUIRED, as a build from
   *  #5079 on answers a token that holds no person scope. Fills mode with no cookie then writes no
   *  `can_fail` line. */
  readonly proxyRefusesToken?: "no-identity" | "no-person-scope";
  /** A change made to the copy of the leg, as a plant of its own text. */
  readonly legChange?: (text: string) => string;
  /** Whether the public leg approves the scratch login; a login nobody approves ends `expired`. */
  readonly approve?: boolean;
  /** The verdicts of the public leg's revoke rows of the scratch login, in order: more than one is
   *  a retried revoke (default one PASS). */
  readonly revokeVerdicts?: readonly string[];
  /** The public leg's own last word: its verdict row (none for "absent") and its exit. */
  readonly publicVerdict?: "PASS" | "FAIL" | "absent";
  readonly publicExit?: number;
  /** P6's row as the public leg writes it, where it differs from a PASS. */
  readonly p6?: string;
  /** Where the pins come from: the deployment repository through `gh` (default), or local files.
   *  `moves` has the public leg change Dispatch's pin at that repository's main while it runs;
   *  `duplicate` gives the pins file a second `dispatch` line. */
  readonly pins?: "repository" | "local" | "moves" | "duplicate";
  /** The commit the stand-in Dispatch's /healthz names: its pin's (default), another one, or its
   *  pin's on this process while `fleet_commit` names another. */
  readonly serving?: "pinned" | "other" | "split";
  /** The T5 window: a scripted peer that sees the window open, an owner removal, and a public
   *  T5-agent row whose last answered trade is `lastAnsweredAfter` s after the leg started. */
  readonly window?: { readonly lastAnsweredAfter: number };
  /** How the public leg starts: the local command (default), or the workflow through `gh`, its
   *  other inputs those `OUTSIDE_WORKFLOW_INPUTS` holds (default the invite link alone), its
   *  repository's visibility as `gh repo view` reads it (default PRIVATE; INTERNAL and PUBLIC as
   *  GitHub names them; empty, unreadable), and that view's exit status (default 0). */
  readonly workflow?: {
    readonly inputs?: string;
    readonly visibility?: string;
    readonly viewExit?: number;
  };
  /** The read sweep beside the leg and on the runner: the writer's one line (default a write
   *  delivered inside the runner's window, in the `write <began> <ended> <comment id> <message
   *  id> delivered` form); fills mode refusing to write a file, as it refuses one that lacks a
   *  fill; the fills' version (default 4, #4608's contract); and the runner's last line and exit
   *  (default PASS, 0). */
  readonly sweep?: {
    readonly writer?: string;
    readonly fillsRefused?: boolean;
    readonly fillsVersion?: number;
    readonly verdict?: string;
    readonly exit?: number;
  };
  /** A P row the public TSV holds twice, first with `first`'s verdict, then with `second`'s. */
  readonly duplicate?: { readonly name: string; readonly first: string; readonly second: string };
  /** The verdict of a P or C row of the public TSV, by name, where it is not PASS. */
  readonly verdicts?: Readonly<Record<string, string>>;
  /** With the T5 window's scripted peer: it is shown P4's ping and reply stamped this many ms ahead
   *  of this box's clock, and the public TSV's P4 row is a PASS of that exchange. */
  readonly peerAheadMs?: number;
  /** T3's register sends the leg a TERM, as an operator's Ctrl-C or a killed job would, after the
   *  full member's helper was approved. */
  readonly t3Kills?: boolean;
  /** The preparation throws once its stand-in Dispatch runs, as a failed step after it would. */
  readonly failAfterDispatch?: boolean;
  /** The browser gate (OUTSIDE_GATE=browser): the workflow through `gh`, whose run writes the
   *  browser checks' artifacts and runs its step `The delisting window`, the owner commands, which
   *  keep the access list in the state, and a `dispatch` that finds the scratch project's issue.
   *  Each field plants one fault; with none, every row passes. */
  readonly browser?: BrowserStandIns;
}

export interface BrowserStandIns {
  /** What the owner's invite answers: a link for an address on the list and 409
   *  PERSON_NOT_LISTED for any other (default), as Dispatch answers the owner's browser and a
   *  personal token of the owner's that holds approve and lists the project; or 403
   *  OAUTH_SCOPE_REQUIRED for every address, as Dispatch answers a token of the owner's that holds
   *  no approve, every broker-issued token among them. */
  readonly invite?: "listed" | "no-approve";
  /** OUTSIDE_UNLISTED_PERSON names an address the list already holds, so the owner's invite makes
   *  it a link. */
  readonly unlistedListed?: boolean;
  /** The owner's remove command exits 0 and takes nobody off the list. */
  readonly removeNothing?: boolean;
  /** The run never runs its step `The delisting window`: the step is skipped. */
  readonly noWindow?: boolean;
  /** The seconds after the removal the run stamps the `at=` of T5-cookie and of T5-passkey
   *  (default 1 and 2). */
  readonly cookieAfter?: number;
  readonly passkeyAfter?: number;
  /** The exit each check records, by its name (default 0 each), and the checks whose `.code` the
   *  run leaves out of their artifacts. */
  readonly codes?: Readonly<Record<string, number>>;
  readonly noCode?: readonly string[];
  /** The verdicts of the driver's limit rows, in order (default one PASS); empty writes none. */
  readonly limits?: readonly string[];
  /** OUTSIDE_PUBLIC_COMMAND set beside the workflow. */
  readonly publicCommand?: boolean;
  /** No owner command is given: the leg makes each owner act with the owner's browser session,
   *  which the stand-in Dispatch answers from the list the state keeps. */
  readonly ownerByCookie?: boolean;
  /** The full member's list of credential grants the stand-in Dispatch answers (default
   *  `default`). */
  readonly fullGrants?: FullGrants;
  /** The verdicts of the driver's rows grants-list and grants-revoke (default both PASS); `none`
   *  writes no outside-edge-grants.tsv. */
  readonly grantRows?: readonly [string, string] | "none";
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
 *  `agent-secrets`, `agent-secrets-helper` and `gh`, which keep their state in `state`, and for the
 *  browser gate of `dispatch`. The `gh` stand-in runs `publicCommand` as the workflow's run. */
function standInPath(
  scratch: Scratch,
  state: string,
  publicCommand: string,
  browser: boolean
): string {
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
  // run, with the code the inputs carry; `gh run view`, `watch` and `download` read that run. `gh
  // repo view` prints the visibility state/visibility holds, or nothing where there is none. A
  // browser gate's run (state/browser) runs its step `The delisting window` at once, kept in
  // state/window, unless state/no-window says the run skips it; `gh run view --json status,jobs`
  // prints `<run status> <that step's status>`, as the leg's jq asks, and counts each read in
  // state/window-reads.
  script(
    join(bin, "gh"),
    `#!/usr/bin/env bash
state='${state}'
case $1 in
  repo)
    [ ! -f "$state/visibility" ] || cat "$state/visibility"
    exit "$(cat "$state/view-exit" 2>/dev/null || echo 0)"
    ;;
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
    if [ -f "$state/browser" ]; then
      if [ -f "$state/no-window" ]; then echo pending >"$state/window"; else echo in_progress >"$state/window"; fi
    fi
    (
      OUTSIDE_TAILNET_USER_CODE=$code OUTSIDE_PUBLIC_RESULTS="$state/run" bash '${publicCommand}'
      echo "$?" >"$state/run-exit"
    ) >/dev/null 2>&1 </dev/null &
    echo "https://github.com/${workflowRepository}/actions/runs/4242"
    ;;
  run)
    case "$2 $*" in
      'view '*'--json status,jobs'*)
        echo read >>"$state/window-reads"
        printf '%s %s\\n' "$(if [ -f "$state/run-exit" ]; then echo completed; else echo in_progress; fi)" "$(cat "$state/window" 2>/dev/null || echo absent)"
        ;;
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
  // state holds login-expires it ends as the real one ends a code nobody approved. Each call's
  // words go to state/agent-secrets-calls.
  script(
    join(bin, "agent-secrets"),
    `#!/usr/bin/env bash
state='${state}'
printf '%s\\n' "$*" >>"$state/agent-secrets-calls"
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
    case $session in *-t3-*)
      printf '%s\\n' "$operator" >"$state/t3-operator"
      # The leg runs this register in the foreground, so its TERM trap runs once this exits.
      [ ! -f "$state/t3-kills" ] || { kill -TERM "$PPID"; exit 1; }
      ;;
    esac
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
  // The browser gate's `dispatch`: the scratch project's issue the issue row finds, by the title
  // the leg gives it, which names the repository the row reads; any other command fails.
  if (browser) {
    script(
      join(bin, "dispatch"),
      `#!/usr/bin/env bash
case $1 in
  issues) printf 'EXAMPLE-7\\topen\\tOutside acceptance: the repository the edge relays to the project\\n' ;;
  read) printf 'EXAMPLE-7: Outside acceptance\\nRepository: example-owner/example-repo\\n' ;;
  *) exit 1 ;;
esac
`
    );
  }
  return bin;
}

export interface StandInDispatch {
  readonly origin: string;
  /** Its process, whose environment a row can read as the kernel holds it. */
  readonly pid: number;
  /** Each request it answered so far, `METHOD path`. */
  seen(): string[];
  stop(): void;
}

const standInDispatchFile = fileURLToPath(
  new URL("./outside-agent-stand-in-dispatch.ts", import.meta.url)
);
/** Every stand-in Dispatch started and not yet stopped. */
const liveDispatches = new Set<number>();

/** How many stand-in Dispatches run now: none once every run's preparation and run have ended,
 *  however they ended. */
export function liveStandInDispatches(): number {
  return liveDispatches.size;
}

/** The stand-in Dispatch (`outside-agent-stand-in-dispatch.ts`) as a process of its own, its
 *  /healthz naming the commit `serving` says: no event loop of a test worker holds its answers.
 *  The child binds a port of the
 *  kernel's choosing and writes it to `<state>/dispatch-port`, so no port chosen here and closed
 *  can be taken by another process before the child binds it; and it answers `/_ready` with a
 *  nonce made here, so another run's stand-in on that port can never pass for this one. It
 *  answers before this returns. */
function standInDispatch(
  state: string,
  controls: StandIns["controls"],
  repo: Checkout,
  serving: NonNullable<StandIns["serving"]>
): StandInDispatch {
  const nonce = randomUUID();
  const portFile = join(state, "dispatch-port");
  const commit = serving === "other" ? repo.apart : repo.built;
  const child = Bun.spawn([process.execPath, standInDispatchFile], {
    env: {
      PATH: process.env.PATH ?? "",
      OUTSIDE_STAND_IN_DISPATCH: JSON.stringify({
        nonce,
        state,
        controls,
        commit,
        fleetCommit: serving === "split" ? repo.apart : commit,
        fullLogin,
        fullToken,
        linkToken,
        proxyRepository,
      }),
    },
    stdin: "ignore",
    stdout: "ignore",
    stderr: "inherit",
  });
  liveDispatches.add(child.pid);
  // Its readiness, read as the rest of the preparation is, before any leg starts: the port the
  // child bound, answering this nonce at /_ready, read for at most 10 s by the clock, plus the 1 s
  // of the last read, so a port that takes connections and never answers holds it 11 s at most.
  const ready = Bun.spawnSync([
    "bash",
    "-c",
    'deadline=$((SECONDS + 10)); while [ "$SECONDS" -lt "$deadline" ]; do if [ -s "$0" ]; then p=$(cat "$0"); [ "$(curl -q -sf --max-time 1 "http://127.0.0.1:$p/_ready")" = "$1" ] && { echo "$p"; exit 0; }; fi; sleep 0.05; done; exit 1',
    portFile,
    nonce,
  ]);
  const port = ready.stdout.toString().trim();
  if (!ready.success || !/^[0-9]+$/.test(port)) {
    child.kill();
    liveDispatches.delete(child.pid);
    throw new Error(
      "the stand-in Dispatch did not answer its own nonce at the port it wrote within 10 s"
    );
  }
  const origin = `http://127.0.0.1:${port}`;
  const seenFile = join(state, "dispatch-seen");
  return {
    origin,
    pid: child.pid,
    seen: () =>
      existsSync(seenFile)
        ? readFileSync(seenFile, "utf8")
            .split("\n")
            .filter((line) => line !== "")
        : [],
    stop: () => {
      child.kill();
      liveDispatches.delete(child.pid);
    },
  };
}

/** A copy of the tailnet leg ready to run against the stand-ins, with a stand-in read sweep beside
 *  it and a public leg that approves the scratch login and writes what the workflow's would. Every
 *  step that waits (the checkout's git, the files, the links, the stand-in Dispatch's start) is
 *  here, before any leg starts. */
export interface PreparedStandIn {
  readonly leg: string;
  readonly env: Record<string, string>;
  readonly dispatch: StandInDispatch;
  readonly state: string;
  readonly repo: Checkout;
}

/** The stand-in probe beside the tailnet leg for the T5 row: its scripted peer is ready at once
 *  and is shown the public leg's `t5-open`, and, where `aheadMs` is given, P4's ping and reply of
 *  nonce `example`, stamped that many ms ahead of this box's clock. */
function standInPeerProbe(aheadMs: number | undefined): string {
  const exchange =
    aheadMs === undefined
      ? ""
      : `for (const word of ["ping", "reply"]) process.stdout.write("outside-e2e " + word + " example at " + (Date.now() + ${aheadMs}) + "\\n");`;
  return `
const [command] = Bun.argv.slice(2);
if (command === "peer") {
  process.stdout.write(JSON.stringify({ ready: true }) + "\\n");
  ${exchange}
  process.stdout.write("outside-e2e t5-open example at 1\\n");
  await Bun.sleep(600000);
} else {
  process.stdout.write(JSON.stringify({ unknown: "the stand-in probe has no " + command }) + "\\n");
}
`;
}

/** The address row unlisted asks a link for where `BrowserStandIns.unlistedListed` puts it on the
 *  list; otherwise the leg makes its own address, which no list names. */
export const unlistedPerson = "outside-e2e-unlisted@example.com";
/** The text every link the stand-in owner's invite makes holds, which no output of the leg may. */
export const linkToken = "owner-made-token";

/** The browser gate's stand-ins: the owner commands, which keep the access list in state/listed and
 *  each link they made in state/links, and the workflow's run. The run writes the browser checks'
 *  artifacts (each `.txt` and `.code`, the driver's outside-edge.tsv and the sweep's two files),
 *  then, unless state/no-window says it skips its step `The delisting window`, waits up to 30 s for
 *  the owner's removal (state/removed), writes outside-edge-t5.tsv with T5-cookie's and
 *  T5-passkey's `at=` the given seconds after it, and ends the step. It exits with the worst exit
 *  it recorded. Returns the run's script and the leg's environment beside it. */
function browserStandIn(
  state: string,
  inputs: string,
  standIns: StandIns,
  browser: BrowserStandIns
): { readonly run: string; readonly env: Record<string, string> } {
  writeFileSync(join(state, "browser"), "");
  writeFileSync(join(state, "full-grants.json"), fullGrantsList(browser.fullGrants ?? "default"));
  if (browser.noWindow === true) writeFileSync(join(state, "no-window"), "");
  writeFileSync(
    join(state, "listed"),
    browser.unlistedListed === true ? `${unlistedPerson}\n` : ""
  );
  const add = script(
    join(inputs, "owner-add.sh"),
    `#!/usr/bin/env bash
while IFS= read -r each; do [ "$each" != "$1" ] || exit 0; done <'${state}/listed'
printf '%s\\n' "$1" >>'${state}/listed'
`
  );
  const list = script(
    join(inputs, "owner-list.sh"),
    `#!/usr/bin/env bash\ncat '${state}/listed'\n`
  );
  const remove = script(
    join(inputs, "owner-remove.sh"),
    `#!/usr/bin/env bash
${
  browser.removeNothing === true
    ? ""
    : `kept=''
while IFS= read -r each; do [ "$each" = "$1" ] || kept="$kept$each"$'\\n'; done <'${state}/listed'
printf '%s' "$kept" >'${state}/listed'`
}
: >'${state}/removed'
`
  );
  const invite = script(
    join(inputs, "owner-invite.sh"),
    `#!/usr/bin/env bash
${browser.invite === "no-approve" ? "echo '403 OAUTH_SCOPE_REQUIRED'\nexit 1" : ""}
while IFS= read -r each; do
  if [ "$each" = "$1" ]; then
    link="https://dispatch.edge.example/enroll#${linkToken}-$RANDOM$RANDOM"
    printf '%s\\n' "$link" >>'${state}/links'
    printf '%s\\n' "$link"
    exit 0
  fi
done <'${state}/listed'
echo '409 PERSON_NOT_LISTED'
exit 1
`
  );
  const code = (name: string): string =>
    browser.noCode?.includes(name) === true ? "none" : String(browser.codes?.[name] ?? 0);
  const limits = (browser.limits ?? ["PASS"])
    .map(
      (verdict, index) =>
        `printf 'limit-%s-60s\\tat most 10 requests in 60 s\\tpeak 1 in 60 s from dispatch.edge.example\\t%s\\n' ${index + 1} '${verdict}' >>"$out/outside-edge.tsv"`
    )
    .join("\n");
  const run = script(
    join(inputs, "browser-run.sh"),
    `#!/usr/bin/env bash
out=$OUTSIDE_PUBLIC_RESULTS
worst=0
# check NAME CODE LINE: the check's output, and its exit unless CODE is none.
check() {
  printf 'the check began\\n%s\\n' "$3" >"$out/$1.txt"
  [ "$2" = none ] || echo "$2" >"$out/$1.code"
  case $2 in 1) worst=1 ;; 2) [ "$worst" = 1 ] || worst=2 ;; esac
}
check outside-edge-refusals ${code("outside-edge-refusals")} 'PASS: every refusal line was refused'
check outside-path-reach ${code("outside-path-reach")} 'PASS: every target is unreachable from this device'
check outside-edge ${code("outside-edge")} 'the driver wrote its rows'
${
  browser.grantRows === "none"
    ? ""
    : `printf 'grants-list\\tthe list\\tlisted 0 grants\\t%s\\ngrants-revoke\\tthe revoke\\trefused 403 RESTRICTED_MEMBER\\t%s\\n' '${browser.grantRows?.[0] ?? "PASS"}' '${browser.grantRows?.[1] ?? "PASS"}' >"$out/outside-edge-grants.tsv"`
}
check outside-edge-grants ${code("outside-edge-grants")} 'the grants probe wrote its rows'
printf 'row\\twant\\tgot\\tverdict\\nsign-in\\tthe sign-in\\tsigned in at https://dispatch.edge.example as the invited person\\tPASS\\nprojects\\tthe projects\\t[EXAMPLE]\\tPASS\\n' >"$out/outside-edge.tsv"
${limits}
{ printf 'the workflow note\\nlive window 2026-01-01T00:00:00Z 2026-01-01T00:00:09Z\\n'; cat '${state}/sweep-verdict'; } >"$out/restricted-sweep.log"
echo ${standIns.sweep?.exit ?? 0} >"$out/restricted-sweep.exit"
case ${standIns.sweep?.exit ?? 0} in 0) ;; 1) worst=1 ;; *) [ "$worst" = 1 ] || worst=2 ;; esac
if [ -f '${state}/no-window' ]; then
  echo completed >'${state}/window'
  exit "$worst"
fi
waited=0
while [ ! -f '${state}/removed' ] && [ "$waited" -lt 300 ]; do
  sleep 0.1
  waited=$((waited + 1))
done
now=$(date -u +%s)
if [ -f '${state}/removed' ]; then
  printf 'T5-cookie\\tthe cookie\\t401 at=%s\\tPASS\\nT5-passkey\\tthe passkey\\trefused at=%s: 403\\tPASS\\n' "$((now + ${browser.cookieAfter ?? 1}))" "$((now + ${browser.passkeyAfter ?? 2}))" >"$out/outside-edge-t5.tsv"
  check outside-edge-t5 ${code("outside-edge-t5")} 'the window ended'
else
  printf 'T5-cookie\\tthe cookie\\tstill 200 at the deadline\\tUNKNOWN\\nT5-passkey\\tthe passkey\\tnot tried\\tUNKNOWN\\n' >"$out/outside-edge-t5.tsv"
  check outside-edge-t5 2 'the window ended with no 401'
fi
echo completed >'${state}/window'
exit "$worst"
`
  );
  return {
    run,
    env: {
      OUTSIDE_GATE: "browser",
      OUTSIDE_WORKFLOW_REPO: workflowRepository,
      // A link here is left out: the browser gate's link is the owner invite's.
      OUTSIDE_WORKFLOW_INPUTS: `invite_url=${inviteUrl}`,
      ...(browser.ownerByCookie === true
        ? {}
        : {
            OUTSIDE_OWNER_ADD_COMMAND: `bash ${add}`,
            OUTSIDE_OWNER_LIST_COMMAND: `bash ${list}`,
            OUTSIDE_OWNER_INVITE_COMMAND: `bash ${invite}`,
            OUTSIDE_OWNER_REMOVE_COMMAND: `bash ${remove}`,
          }),
      ...(browser.unlistedListed === true ? { OUTSIDE_UNLISTED_PERSON: unlistedPerson } : {}),
      ...(browser.publicCommand === true ? { OUTSIDE_PUBLIC_COMMAND: `bash ${run}` } : {}),
    },
  };
}

export async function prepareStandIn(
  scratch: Scratch,
  standIns: StandIns
): Promise<PreparedStandIn> {
  // The ports the broker and the listener are named at: refused, from a range the kernel hands no
  // `port: 0` listener, each held by a UDP bind of this process so no other `closedPort` call gets
  // it. A program that binds the number over TCP is not stopped; none of this suite does.
  const brokerPort = await closedPort();
  const listenerPort = await closedPort();
  const state = scratch("state");
  writeFileSync(join(state, "canary-refusal"), `${standIns.canaryRefusal}\n`);
  if (standIns.approve === false) writeFileSync(join(state, "login-expires"), "");
  // The fills the sweep's fills mode writes, version 4: the project, the SHA-256 of the origin they
  // were made at, of the routes.txt and of the deployment they were made from and at, when, the
  // repository, and a `can_fail` line for each proxy read, but only with the full member's
  // cookie, as restricted-sweep.sh does.
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
      `restricted-sweep-fills\t${standIns.sweep?.fillsVersion ?? 4}`,
      "project\tPRIV",
      `origin_sha256\t${"e".repeat(64)}`,
      `routes\t${"a".repeat(64)}`,
      `deployment\t${"b".repeat(64)}`,
      `made_at\t${Math.floor(Date.now() / 1000)}`,
      `proxy_repo\t${proxyRepository}`,
      ...canFail.map((read) => `can_fail\t${read}`),
      "",
    ].join("\n")
  );
  // The public leg's TSV: P1-P10 and C0-C7 PASS but where a row is given, its revocations, its
  // verdict, and in the T5 row a T5-agent row whose last answered trade the public command stamps.
  const tab = "\t";
  const publicTsv = [
    ["row", "want", "got", "verdict"],
    ...publicRows.flatMap((name) =>
      name === standIns.duplicate?.name
        ? [
            [name, `the row ${name}`, `what ${name} read first`, standIns.duplicate.first],
            [name, `the row ${name}`, `what ${name} read second`, standIns.duplicate.second],
          ]
        : name === "P6" && standIns.p6 !== undefined
          ? [[name, `the row ${name}`, standIns.p6, standIns.p6.split(" ")[0] as string]]
          : name === "P4" && standIns.peerAheadMs !== undefined
            ? [
                [
                  name,
                  `the row ${name}`,
                  "session omp-session-1: pong 1 s, hello 1 s and done 1 s after the peer sent them; nonce=example sockets=1",
                  "PASS",
                ],
              ]
            : [[name, `the row ${name}`, `what ${name} read`, standIns.verdicts?.[name] ?? "PASS"]]
    ),
    ["revocations", "every login revoked", "the revoke command exited 0", "PASS"],
    ...(standIns.publicVerdict === "absent"
      ? []
      : [["verdict", "every row PASS", "as read", standIns.publicVerdict ?? "PASS"]]),
  ]
    .map((fields) => fields.join(tab))
    .join("\n");
  writeFileSync(join(state, "public.tsv"), `${publicTsv}\n`);
  // The writer's line and the runner's last line, as files, since a sweep's own words may hold a
  // quote no shell string may.
  writeFileSync(
    join(state, "writer-line"),
    `${standIns.sweep?.writer ?? `write 2026-01-01T00:00:04Z 2026-01-01T00:00:05Z ${writeIds.c1} ${writeIds.m1} delivered`}\n`
  );
  writeFileSync(
    join(state, "sweep-verdict"),
    `${standIns.sweep?.verdict ?? "PASS: every read refused"}\n`
  );
  const repo = checkout(scratch);
  const inputs = scratch("inputs");
  const leg = copyLeg(scratch, tailnetLeg, standIns.legChange);
  if (standIns.proxyRefusesToken !== undefined) {
    writeFileSync(
      join(state, "proxy-refuses-token"),
      standIns.proxyRefusesToken === "no-identity" ? "401 NO_IDENTITY" : "403 OAUTH_SCOPE_REQUIRED"
    );
  }
  const legDir = leg.slice(0, leg.lastIndexOf("/"));
  if (standIns.window !== undefined) {
    writeFileSync(join(legDir, "outside-agent-probe.ts"), standInPeerProbe(standIns.peerAheadMs));
  }
  // The writer is stopped by the leg when the public leg ends: on a TERM it leaves a mark and
  // ends. A writer the leg failed to stop is killed later by the bounded child's own stop, with
  // a SIGKILL, which leaves no mark. Fills mode and the writer need the full member's token; fills
  // mode writes the `can_fail` lines of the proxy reads only with the full member's cookie, and
  // without it writes the fills less those lines and leaves state/fills-without-cookie, as the
  // sweep makes its other reads with the token alone.
  script(
    join(legDir, "restricted-sweep.sh"),
    `#!/usr/bin/env bash
[ -n "\${SWEEP_FULL_TOKEN:-}" ] || { echo 'UNKNOWN: SWEEP_FULL_TOKEN is required'; exit 2; }
case $SWEEP_MODE in
  fills)
    ${standIns.sweep?.fillsRefused === true ? "echo 'UNKNOWN: fills mode writes no file that lacks a fill: dm_session'; exit 2" : ""}
    # With no cookie the fills' proxy reads are the token's, which a build before #5079 refuses 401
    # and a build from it on refuses 403 for a token without the person scope: those fills hold no
    # can_fail line.
    if [ -n "\${SWEEP_FULL_COOKIE:-}" ]; then
      : >'${state}/fills-read-the-cookie'
      cat '${state}/fills' >"$SWEEP_FILLS_FILE"
    elif [ ! -f '${state}/proxy-refuses-token' ]; then
      : >'${state}/fills-read-the-token'
      cat '${state}/fills' >"$SWEEP_FILLS_FILE"
    else
      : >'${state}/fills-without-cookie'
      while IFS= read -r line; do
        case $line in can_fail$'\\t'*) ;; *) printf '%s\\n' "$line" ;; esac
      done <'${state}/fills' >"$SWEEP_FILLS_FILE"
    fi
    ;;
  writer)
    cat '${state}/writer-line'
    sleep 600 &
    child=$!
    trap 'kill "$child"; : >"${state}/writer-stopped"; exit 0' TERM
    wait "$child"
    ;;
esac
`
  );
  // The pins at the deployment repository's main (state/deploy), or local files. The browser gate's
  // edge-listener line names a file the repository lacks: that gate reads no edge listener's pin.
  mkdirSync(join(state, "deploy"), { recursive: true });
  const pinsLocal = standIns.pins === "local";
  const pins: string[] = [];
  for (const name of ["broker", "dispatch", "edge-listener"]) {
    if (standIns.browser === undefined || name !== "edge-listener") {
      writeFileSync(
        join(state, "deploy", `${name}.yaml`),
        `example:image: registry.example/${name}@sha256:0\n`
      );
    }
    pins.push(
      `${name} ${pinsLocal ? join(state, "deploy", `${name}.yaml`) : `${name}.yaml`} example:image`
    );
  }
  // A second `dispatch` line, naming another file, where the pins file is to hold one twice.
  if (standIns.pins === "duplicate") pins.push("dispatch dispatch-second.yaml example:image");
  writeFileSync(join(inputs, "pins.txt"), `${pins.join("\n")}\n`);
  if (standIns.t3Kills === true) writeFileSync(join(state, "t3-kills"), "");
  const visibility = standIns.workflow?.visibility ?? "PRIVATE";
  if (visibility !== "") writeFileSync(join(state, "visibility"), `${visibility}\n`);
  writeFileSync(join(state, "view-exit"), `${standIns.workflow?.viewExit ?? 0}\n`);
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
${(standIns.revokeVerdicts ?? ["PASS"]).map((verdict) => `printf 'revoke-cred-%s\\trevoked\\tstatus 200\\t%s\\n' "$(printf '%s' "$OUTSIDE_TAILNET_USER_CODE" | tr 'A-Z' 'a-z')" '${verdict}' >>"$out/outside-edge-revocations.tsv"`).join("\n")}
: >'${state}/revoked'
{ printf 'the workflow note\\nlive window 2026-01-01T00:00:00Z 2026-01-01T00:00:09Z\\n'; cat '${state}/sweep-verdict'; } >"$out/restricted-sweep.log"
echo ${standIns.sweep?.exit ?? 0} >"$out/restricted-sweep.exit"
${standIns.window === undefined ? "" : "sleep 3"}
exit ${standIns.publicExit ?? 0}
`
  );
  const browserRun =
    standIns.browser === undefined
      ? undefined
      : browserStandIn(state, inputs, standIns, standIns.browser);
  const revision = script(join(inputs, "revision.sh"), `#!/bin/sh\necho ${repo.built}\n`);
  writeFileSync(join(inputs, "cookie"), "session=full\n");
  writeFileSync(join(inputs, "token"), `${fullToken}\n`);
  const dispatch = standInDispatch(state, standIns.controls, repo, standIns.serving ?? "pinned");
  try {
    if (standIns.failAfterDispatch === true) {
      throw new Error("a step after the stand-in Dispatch started failed, as the row asks");
    }
    const remove = script(
      join(inputs, "remove.sh"),
      `#!/bin/sh\nexec curl -q -sS -X POST '${dispatch.origin}/_remove' -o /dev/null\n`
    );
    const env: Record<string, string> = {
      PATH: standInPath(
        scratch,
        state,
        browserRun?.run ?? publicLegCommand,
        browserRun !== undefined
      ),
      HOME: scratch("home"),
      ...(process.env.TMPDIR === undefined ? {} : { TMPDIR: process.env.TMPDIR }),
      OUTSIDE_ALLOW_HTTP: "1",
      OUTSIDE_PINS: join(inputs, "pins.txt"),
      ...(pinsLocal ? {} : { OUTSIDE_PINS_REPO: deployRepository }),
      OUTSIDE_IMAGE_REVISION_COMMAND: `sh ${revision}`,
      OUTSIDE_LEGION_CHECKOUT: repo.dir,
      OUTSIDE_ENROLL_REFUSAL_COMMIT: repo.held,
      OUTSIDE_DISPATCH_COMMITS: {
        held: repo.held,
        lacking: `${repo.held} ${repo.apart}`,
        blank: "  ",
        malformed: `${repo.held} HEAD`,
      }[standIns.dispatchCommits],
      OUTSIDE_TAILNET_BROKER_URL: `http://127.0.0.1:${brokerPort}`,
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
            OUTSIDE_LISTENER_URL: `http://127.0.0.1:${listenerPort}`,
            OUTSIDE_OWNER_REMOVE_COMMAND: `sh ${remove}`,
          }),
    };
    // The browser gate runs with none of the agents' path's inputs, and starts the workflow alone.
    if (browserRun !== undefined) {
      for (const name of [
        "OUTSIDE_TAILNET_BROKER_URL",
        "OUTSIDE_CANARY",
        "OUTSIDE_PUBLIC_COMMAND",
      ]) {
        delete env[name];
      }
      Object.assign(env, browserRun.env);
    }
    return { leg, env, dispatch, state, repo };
  } catch (error) {
    // A step after the stand-in Dispatch started threw: stop it, so no process outlives the row.
    dispatch.stop();
    throw error;
  }
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
