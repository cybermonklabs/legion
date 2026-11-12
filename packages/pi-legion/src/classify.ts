import { readSecretFile } from "@legion/envoy-client/secret-file";
import { requiredEnvironment } from "@legion/pi-shared/legion-session";

/** `<key>_FILE` (trimmed file contents) ahead of `<key>`: the daemon hands every pane secret over
 * as a 0600 file pointer, never as a tmux `-e` argv value. A set pointer is authoritative — a
 * missing, unreadable, or blank file throws naming both the variable and the path, never falling
 * back to `<key>`. */
export function requiredSecret(env: NodeJS.ProcessEnv, key: string): string {
  const fileKey = `${key}_FILE`;
  const file = env[fileKey];
  if (file !== undefined) return readSecretFile(fileKey, file);
  return requiredEnvironment(env, key);
}

export function requiredControllerCapability(env: NodeJS.ProcessEnv): string {
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
  return secret;
}
