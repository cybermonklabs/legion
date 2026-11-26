import { isStepRole, type LegionStepRole } from "@legion/contracts";
import { holdSecret } from "@legion/contracts/held-secrets";
import { readSecretFile } from "@legion/envoy-client/secret-file";

export type LegionSessionKind =
  | { kind: "controller" }
  | { kind: "root-architect"; tree: string }
  | { kind: "phase-worker"; role: LegionStepRole; tree: string; issue: string }
  | { kind: "not-legion" };

export function classifySession(env: NodeJS.ProcessEnv): LegionSessionKind {
  if (env.LEGION_CONTROLLER !== undefined && env.LEGION_TREE !== undefined) {
    throw new Error("Legion session has both controller and tree launch markers");
  }

  // The controller marker is LEGION_CONTROLLER alone. `legion controller start` also sets
  // LEGION_ROLE=controller on that process, but "controller" is no claim's role of an issue and
  // this extension never reads LEGION_ROLE for it — one signal, checked once, so the two markers
  // can never disagree in practice.
  if (env.LEGION_CONTROLLER === "1") return { kind: "controller" };

  if (env.LEGION_ROLE === undefined) return { kind: "not-legion" };

  // A phase worker's role is one of Legion's six or a role of the deployment (the deployment roles'
  // contract entry).
  if (!isStepRole(env.LEGION_ROLE)) {
    throw new Error(`LEGION_ROLE "${env.LEGION_ROLE}" is not a Legion role`);
  }
  const tree = requiredEnvironment(env, "LEGION_TREE");
  const issue = requiredEnvironment(env, "LEGION_ISSUE");
  // The daemon roots an issue tree at itself: the root architect's own issue
  // key equals the tree's. A sub-architect on a child issue is a phase
  // worker like any other role.
  if (env.LEGION_ROLE === "architect" && issue === tree) {
    return { kind: "root-architect", tree };
  }
  return { kind: "phase-worker", role: env.LEGION_ROLE, tree, issue };
}

export function requiredEnvironment(env: NodeJS.ProcessEnv, key: string): string {
  const value = env[key];
  if (!value) throw new Error(`${key} is required for Legion`);
  return value;
}

/** `<key>_FILE` (trimmed file contents, read without holding the event loop) ahead of `<key>`: the
 * daemon hands every pane secret over as a 0600 file pointer, never as a tmux `-e` argv value. A
 * set pointer is authoritative — a missing, unreadable, or blank file rejects naming the variable
 * and the read error's code, never the path, which is what a secret pasted into the variable would
 * be (`readSecretFile`), and never falls back to `<key>`. Either way the secret is held, so every
 * line this process prints is scrubbed of it (`@legion/contracts/held-secrets`). */
export async function requiredSecret(env: NodeJS.ProcessEnv, key: string): Promise<string> {
  const fileKey = `${key}_FILE`;
  const file = env[fileKey];
  if (file !== undefined) return readSecretFile(fileKey, file);
  return holdSecret(requiredEnvironment(env, key));
}

export async function requiredControllerCapability(env: NodeJS.ProcessEnv): Promise<string> {
  if (env.LEGION_CONTROLLER_SECRET_FILE !== undefined) {
    return readSecretFile("LEGION_CONTROLLER_SECRET_FILE", env.LEGION_CONTROLLER_SECRET_FILE);
  }
  const secret = env.LEGION_CONTROLLER_SECRET;
  if (!secret) {
    throw new Error(
      "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required to claim the controller. " +
        "Launch OMP with one of them in its environment before running /legion-claim-controller."
    );
  }
  return holdSecret(secret);
}
