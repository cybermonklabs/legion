/**
 * The address the arguments of a `node:net` bind or dial name, read as `node:net` reads them:
 * `(port[, host][, …])`, or `(options[, …])` whose `port` and `host` name it. A port is a number,
 * or a string `node:net` takes for one, of 0 to 65535. Undefined where the arguments name no TCP
 * port, as a unix socket's path does. It reads and changes nothing else: the dial counter of
 * extensions/envoy.test.ts and the held-port preload test/nats-default-port-held.ts both decode
 * a call with it, and each does what it does with the answer itself.
 */
export function netCallAddress(
  args: readonly unknown[]
): { readonly port: number; readonly host: string } | undefined {
  const [first, second] = args;
  const options =
    typeof first === "object" && first !== null
      ? (first as { port?: unknown; host?: unknown })
      : undefined;
  const port = options === undefined ? first : options.port;
  const host = options === undefined ? second : options.host;
  if (typeof port !== "number" && !(typeof port === "string" && port.trim() !== "")) {
    return undefined;
  }
  const number = Number(port);
  if (!Number.isInteger(number) || number < 0 || number > 65_535) return undefined;
  return { port: number, host: typeof host === "string" ? host : "" };
}
