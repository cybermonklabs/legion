// The environment filter the rig's own processes share. rig.sh drops every DISPATCH_*, ENVOY_* and
// NATS_* variable at its start (its comment over that loop), and the gh stand-in and daemon-pane.ts
// drop the same three families again for the children they spawn, since each can be started
// outside rig.sh. It is a module of its own rather than gh-standin.ts's export so that
// daemon-pane.ts, which daemon-standin.ts loads at its start under a measured start bound, does not
// load the stand-in, its schemas and its GraphQL reader for three lines.

/** Every variable of `env` but the DISPATCH_*, ENVOY_* and NATS_* ones. Bun hands a child the
 * environment its parent started with unless a spawn names one, so each spawn of the rig's that
 * must not reach a caller's Envoy or Dispatch names this. */
export function withoutServices(env: Record<string, string | undefined>) {
  return Object.fromEntries(
    Object.entries(env).filter(([name]) => !/^(?:DISPATCH|ENVOY|NATS)_/.test(name))
  );
}
