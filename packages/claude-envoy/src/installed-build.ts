// Which build of this plugin Claude Code is running, read off the path of the bundle it loaded.
//
// Claude Code installs a plugin under `…/plugins/cache/<marketplace>/<plugin>/<version>/`, where
// the version is the published npm version, since this plugin's marketplace entry is an `npm`
// source. Every registration names that build to the listener, so the Agents page can tell a
// session running a release from one running a checkout.
import { dirname, join, sep } from "node:path"
import { clientBuild, clientConnectionName } from "@legion/envoy-client/nats-connection"

/** The segments after `…/plugins/cache/` in a plugin install path, or undefined for any other
 *  path: a source checkout, `--plugin-dir`, or a bare server config. */
function pluginCacheSegments(bundlePath: string): string[] | undefined {
  const parts = bundlePath.split(sep)
  for (let index = 0; index + 1 < parts.length; index++) {
    if (parts[index] === "plugins" && parts[index + 1] === "cache") return parts.slice(index + 2)
  }
  return undefined
}

/**
 * The version a plugin install path names (`…/plugins/cache/<marketplace>/<plugin>/<version>/…`,
 * the published npm version for this plugin, whose marketplace entry is an `npm` source), or
 * undefined for any other path.
 */
export function installedPluginVersion(bundlePath: string): string | undefined {
  return pluginCacheSegments(bundlePath)?.[2]
}

/** The name the channel server's NATS connection presents: `name`, then its build, which
 *  legacy-subjects-gate.sh reads from the hub's census of connections. The build is the build-commit
 *  stamp the worker image writes beside the bundle (`/opt/legion/claude-envoy/dist/build-commit`),
 *  else `<package>@<version>` for a release Claude Code or Codex installed in its plugin cache,
 *  else none (a checkout). */
export function channelConnectionName(
  name: string,
  bundlePath: string,
  packageName: string,
): string {
  const version = installedPluginVersion(bundlePath)
  return clientConnectionName(
    name,
    clientBuild(
      join(dirname(bundlePath), "build-commit"),
      version === undefined ? undefined : { name: packageName, version },
    ),
  )
}
