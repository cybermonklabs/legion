// Session attribution for jj commits, shared by the harness plugins.
//
// At root bootstrap the extension exports JJ_CONFIG = <user config chain> +
// a generated overlay that sets three jj templates, so every jj commit made
// from the root session — and from every phase worker, since workers are
// subagents sharing the root process environment — automatically carries the
// pair: an `Omp-Session: <root-session-id>` line, then a `Legion-Role: <role>`
// line. The role is `LEGION_ROLE` when it names a role (`isTrailerRole`), and
// a pane without one gets the session line alone. `templates.commit_trailers`
// gives the pair to `jj commit`, `jj describe` and `jj split`. `jj revert` and
// `jj duplicate` write their descriptions from `templates.revert_description`
// and `templates.duplicate_description` and read no trailer template, so the
// overlay sets those two to jj's own text with the pair added. Attribution
// rides the commit itself; no worker compliance and no host-specific jj config
// needed.
//
// JJ_CONFIG replaces jj's default config lookup, so an existing JJ_CONFIG (or
// jj's own `config path --user` output, chained in printed order) must come
// first; the overlay layers last and wins for its three keys. Attribution
// must degrade to *no* trailer, never a *wrong* one: stale attribution
// overlays already present in the inherited chain (this extension's or the
// dotfiles session-env extension's — both use the omp-attribution-*.toml
// filename grammar) are stripped before the current overlay is appended, and
// the overlay is written atomically. Failure to resolve jj or write the
// overlay is non-fatal and must never block root bootstrap, so the jj call is
// bounded: a jj that never answers (a PATH wrapper that loops) degrades to no
// trailer instead of holding the rest of the claim's boot, the Envoy role
// claim and `claims/ready`, which run after it.
//
// For a pane whose issue sits on a repository the deployment names, the
// overlay's `commit_trailers` carries one more line, `Dispatch: <KEY>`, so a
// squash landing message carries the issue key. That line rides a
// `[[--scope]]` table whose `--when.workspaces` names the pane's own resolved
// workspace, and the unscoped `[templates]` table keeps the pair alone: a
// commit the pane's shell makes in any other jj repository carries the pair
// and no issue key, so a clone of a public repository never carries this
// deployment's issue key.
//
// The scoped table repeats the pair before the issue line, because a scoped
// `templates.commit_trailers` replaces the key rather than adding to it: a
// scope carrying the issue line alone would drop the pair from every commit
// it stamps. The scope sets no other template, so inside it `jj revert` and
// `jj duplicate` use the unscoped two, which write the pair and no issue key.
//
// jj resolves a workspace root before it matches the scope, and matches it as
// a path prefix on whole path components, so the path written must be the
// real, absolute one (`resolvedWorkspace`). A jj repository a role creates
// *under* its own workspace is stamped too; a sibling whose name merely
// extends that path is not; and a second jj workspace of the same repository,
// at another path, carries the pair alone. A workspace path the
// writer cannot contain — one holding a character a TOML basic string cannot
// carry raw — and one that does not resolve both leave the pair
// alone, rather than a scope jj refuses: a scope jj cannot parse breaks every
// `jj` command in the pane, which is far worse than a missing trailer.
import { realpathSync } from "node:fs";
import { mkdir, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { isLegionRole } from "@legion/contracts";
import { SESSION_ID_PATH_SEGMENT } from "@legion/envoy-client/dispatch-session-directory";

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
/** The bound on `jj config path --user`, which reads no repository and answers in milliseconds:
 * well inside the 30 s Oh My Pi gives the session_start handler that awaits it. */
export const JJ_CONFIG_PATH_TIMEOUT_MS = 10_000;
const TOP_LEVEL_TRANSCRIPT = new RegExp(`^\\d{4}-\\d{2}-\\d{2}T[\\d-]+Z_(${UUID})\\.jsonl$`);
const ATTRIBUTION_OVERLAY = /^omp-attribution-.*\.toml$/;
/** The whole value of an issue key, as the gated base branch's rule accepts one: a project part of
 * two to ten characters, then the number. Not `DISPATCH_KEY_PATTERN`
 * (`@legion/contracts/legion-roles`), which caps nothing and so accepts a key that rule refuses.
 * The match is containment as much as correctness: the value is interpolated into a jj template
 * inside a TOML literal, so a quote, a backslash or a newline would write a template jj cannot
 * parse and break every `jj describe` in the pane. */
const ISSUE_KEY = /^[A-Z][A-Z0-9]{1,9}-[0-9]+$/;
/** The first code point a TOML basic string carries raw, and the one it cannot: every control
 * character below the space, and the DEL. */
const FIRST_RAW_CODE = 0x20;
const DEL_CODE = 0x7f;
/** `LEGION_ROLE` in the controller's process (`legion controller start`): it is no `LegionRole`,
 * and a commit the controller makes names it all the same. */
const CONTROLLER_ROLE = "controller";
/** jj 0.45's default `templates.revert_description` (`jj config list --include-defaults
 * templates`), one argument of its `concat` per entry, copied as jj writes them. jj writes a
 * revert's description from this template alone and adds no `commit_trailers` to it, so the
 * overlay's revert template is these parts, then a blank line and the pair. */
const JJ_REVERT_PARTS = [
  `'Revert "' ++ description.first_line() ++ '"' ++ "\\n"`,
  `"\\n"`,
  `"This reverts commit " ++ commit_id ++ ".\\n"`,
];

/** A trailer the overlay writes: its key, then a value the writer checked. */
type Trailer = readonly [key: string, value: string];

export function sessionIdFromTranscript(sessionFile: string): string | undefined {
  return TOP_LEVEL_TRANSCRIPT.exec(path.basename(sessionFile))?.[1];
}

/** The real, absolute path of `workspace` — what `jj root` prints in the pane, and so what jj
 * matches a `[[--scope]]` against — or undefined for an empty, unset or unresolvable one. A
 * relative path and a path that does not exist match no scope and say nothing, so neither may
 * ever reach the overlay. */
export function resolvedWorkspace(workspace: string | undefined): string | undefined {
  if (!workspace) return undefined;
  try {
    return realpathSync(workspace);
  } catch {
    return undefined;
  }
}

/** `workspace` as a TOML basic string, quotes included, or undefined when it holds a character
 * such a string cannot carry raw and no escape of ours should guess at: a scope jj cannot parse
 * would break every `jj` command in the pane, so the caller writes no scope and the pair alone. */
function tomlBasicString(workspace: string): string | undefined {
  for (let at = 0; at < workspace.length; at += 1) {
    const code = workspace.charCodeAt(at);
    if (code < FIRST_RAW_CODE || code === DEL_CODE) return undefined;
  }
  return `"${workspace.replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`;
}

/** Whether `role` may reach the overlay's `Legion-Role:` line: a Legion role (`isLegionRole`) or
 * the controller's. The match is containment as much as correctness, as the issue key's is: the
 * value is interpolated into a jj template inside a TOML literal. */
function isTrailerRole(role: string | undefined): role is string {
  return role !== undefined && (isLegionRole(role) || role === CONTROLLER_ROLE);
}

/** `templates.commit_trailers` giving `trailers`, one line each in order. The `\n` between them is
 * the two characters jj's template language reads as a newline, inside a TOML literal. */
function commitTrailers(trailers: readonly Trailer[]): string {
  return `commit_trailers = '"${trailers.map(([key, value]) => `${key}: ${value}`).join("\\n")}"'\n`;
}

/** The unscoped `[templates]` table: the three templates that give `pair` to `jj commit`,
 * `jj describe`, `jj split`, `jj revert` and `jj duplicate`. The two multi-line templates are TOML
 * `'''` literals, because jj's own revert text holds `'`. */
function templatesTable(pair: readonly Trailer[]): string {
  const revert = [
    ...JJ_REVERT_PARTS,
    `"\\n"`,
    ...pair.map(([key, value]) => `"${key}: ${value}\\n"`),
  ];
  // A duplicate keeps the description of its source and adds each line of the pair that no
  // trailer of the source holds with the same key and value. A description with no trailer holds
  // no line of the pair, so it takes a blank line before them. An empty description stays empty.
  const missing = pair.map(([key, value]) => {
    const held = `trailers.any(|t| t.key() == "${key}" && t.value() == "${value}")`;
    return `    if(!${held}, "${key}: ${value}\\n"),\n`;
  });
  return (
    "[templates]\n" +
    commitTrailers(pair) +
    `revert_description = '''\nconcat(\n${revert.map((part) => `  ${part},\n`).join("")})\n'''\n` +
    "duplicate_description = '''\nconcat(\n  description,\n  if(description, concat(\n" +
    `    if(!trailers, "\\n"),\n${missing.join("")}  )),\n)\n'''\n`
  );
}

/** The `[[--scope]]` table that gives `pair`, then `Dispatch: <issueKey>`, to the commits of
 * `workspace` alone, or "" when the key's whole value is no key or there is no workspace a TOML
 * basic string can carry. A scoped `templates.commit_trailers` replaces the unscoped one, so it
 * repeats the pair. */
function scopeTable(
  pair: readonly Trailer[],
  issueKey: string | undefined,
  workspace: string | undefined
): string {
  if (issueKey === undefined || !ISSUE_KEY.test(issueKey) || !workspace) return "";
  const scopedTo = tomlBasicString(workspace);
  if (scopedTo === undefined) return "";
  return (
    `\n[[--scope]]\n--when.workspaces = [${scopedTo}]\n[--scope.templates]\n` +
    commitTrailers([...pair, ["Dispatch", issueKey]])
  );
}

function withoutOverlays(chain: string): string {
  return chain
    .split(":")
    .filter((entry) => entry && !ATTRIBUTION_OVERLAY.test(path.basename(entry)))
    .join(":");
}

export async function userConfigChain(
  timeoutMs = JJ_CONFIG_PATH_TIMEOUT_MS,
  env: NodeJS.ProcessEnv = process.env
): Promise<string | undefined> {
  try {
    const child = Bun.spawn(["jj", "config", "path", "--user"], {
      detached: true,
      env,
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    });
    const killGroup = () => {
      try {
        process.kill(-child.pid, "SIGKILL");
      } catch {
        // The group exited before cleanup reached it.
      }
    };
    // Every descendant belongs to this call's group. Kill it as soon as the leader exits, before
    // either its output can answer successfully or a non-zero exit can answer unsuccessfully.
    const exited = child.exited.finally(killGroup);
    // Include both streams in the deadline: a wrapper can fork a helper that inherits them after
    // its leader exits, and attribution must never wait for that helper.
    const completed = Promise.all([
      exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    const stdout = await new Promise<string | undefined>((resolve) => {
      const timer = setTimeout(() => {
        killGroup();
        resolve(undefined);
      }, timeoutMs);
      void completed.then(
        ([code, output]) => {
          clearTimeout(timer);
          resolve(code === 0 ? output : undefined);
        },
        () => {
          clearTimeout(timer);
          resolve(undefined);
        }
      );
    });
    if (stdout === undefined) return undefined;
    const paths = stdout.split("\n").filter(Boolean);
    return paths.length > 0 ? paths.join(":") : undefined;
  } catch {
    return undefined;
  }
}

/** Writes the overlay for `sessionId` under `stateDir` and returns the chain it exported into
 * `env.JJ_CONFIG`, or undefined when it exported none: a session id that is not one
 * (`SESSION_ID_PATH_SEGMENT`: the id reaches both the TOML literal and the overlay's file name,
 * so an id that is not one passes nothing on), no base chain to layer over, or a failed write —
 * which still degrades `env.JJ_CONFIG` to the base, since no overlay is active.
 * `options.role` adds the `Legion-Role:` line after the session line, only when it names a role a
 * commit may carry (`isTrailerRole`). `options.issueKey` and `options.workspace` add the
 * `Dispatch:` line, scoped to that one workspace, only when the key's whole value is a key and the
 * path is one a TOML basic string can carry; `options.workspace` is the resolved path
 * (`resolvedWorkspace`), which is what jj matches a scope against. */
export async function exportJjSessionAttribution(
  sessionId: string,
  stateDir: string,
  options: {
    issueKey?: string;
    workspace?: string;
    role?: string;
    env?: NodeJS.ProcessEnv;
    resolveBase?: () => Promise<string | undefined>;
  } = {}
): Promise<string | undefined> {
  const { issueKey, workspace, role, env = process.env, resolveBase = userConfigChain } = options;
  // The id reaches both a TOML literal and a file name here: a single quote would end the literal
  // and break every `jj` command in the pane, and a `/` would move the overlay out of the state
  // directory. Oh My Pi's id is the transcript's UUID; Claude Code's is whatever its hook input
  // carries, which its schema caps at nothing.
  if (!SESSION_ID_PATH_SEGMENT.test(sessionId)) return undefined;
  const overlay = path.join(stateDir, `omp-attribution-${sessionId}.toml`);
  // Bracket access, not a dot: claude-envoy type-checks this source under
  // noPropertyAccessFromIndexSignature, which refuses a dot on an index signature.
  // biome-ignore-start lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  const active = env["JJ_CONFIG"];
  // Already active — never rewrite a live layer; the caller still learns the live chain.
  if (active?.split(":").includes(overlay)) return active;
  const base = (active ? withoutOverlays(active) : "") || (await resolveBase());
  if (!base) return undefined;
  // The session line, then the role line: a caller's role is checked again here, as the key is,
  // because the value reaches a jj template inside a TOML literal.
  const pair: Trailer[] = [["Omp-Session", sessionId]];
  if (isTrailerRole(role)) pair.push(["Legion-Role", role]);
  const contents = templatesTable(pair) + scopeTable(pair, issueKey, workspace);
  try {
    await mkdir(stateDir, { recursive: true });
    const tmp = `${overlay}.${process.pid}.tmp`;
    await writeFile(tmp, contents);
    await rename(tmp, overlay);
    const chain = `${base}:${overlay}`;
    env["JJ_CONFIG"] = chain;
    return chain;
  } catch {
    env["JJ_CONFIG"] = base; // degrade to no trailer, never a stale one
    return undefined;
  }
  // biome-ignore-end lint/complexity/useLiteralKeys: see above
}

/** The writer's `issueKey`, `workspace` and `role` for a Legion pane, from the three variables of
 * the pane's environment that give them: the rules both harnesses' callers follow, in one place.
 *
 * The key is `LEGION_ISSUE_TRAILER` alone, never `LEGION_ISSUE`: a pane works an issue of this
 * deployment's own Dispatch project, and only the daemon knows whether its repository is one the
 * deployment names, so a key reaches a commit only when the daemon put it in this variable
 * (dispatch://INFRA-472). Absent or empty is no key, and the overlay carries the pair alone.
 *
 * The workspace is `LEGION_WORKSPACE` resolved (`resolvedWorkspace`), since jj matches a scope
 * against the workspace root it resolved: a caller that passed the raw variable would write a
 * scope that silently matches nothing. Without one the overlay carries the pair alone too —
 * the issue key belongs to the pane's own repository, not to whatever else its shell commits in.
 *
 * The role is `LEGION_ROLE`, only when it names a role a commit may carry (`isTrailerRole`: a
 * Legion role, or the controller's). Any other value is no role, and the pair is the session line
 * alone. This is the one place that reads the role for the overlay.
 *
 * Each is left out rather than passed undefined, so the result is the options object of a caller
 * type-checked under `exactOptionalPropertyTypes` (claude-envoy's). */
export function paneAttributionOptions(env: NodeJS.ProcessEnv): {
  issueKey?: string;
  workspace?: string;
  role?: string;
} {
  // biome-ignore-start lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  const issueKey = env["LEGION_ISSUE_TRAILER"];
  const workspace = resolvedWorkspace(env["LEGION_WORKSPACE"]);
  const role = env["LEGION_ROLE"];
  // biome-ignore-end lint/complexity/useLiteralKeys: see above
  return {
    ...(issueKey ? { issueKey } : {}),
    ...(workspace ? { workspace } : {}),
    ...(isTrailerRole(role) ? { role } : {}),
  };
}
