// The address a test's own server listens on, so that no client of another test can reach it.
//
// A server of a test holds its port only while that test runs. A client the test leaves behind,
// or a client of another test on the machine, keeps dialing the address its own server had, and
// the kernel gives that port to the next listener that asks for one on the same address: on
// 127.0.0.1, which every such server shares, that listener takes the stranger's requests for its
// own. A form POST of another test's OAuth client reached the stand-in of a real-binary row that
// way and failed the row. An address no other server of the machine listens on is out of every
// such client's reach, whatever port it gets. This is the rule the Go tests keep with
// `internal/testport`'s `LoopbackIP`, in both Go modules.

/**
 * An address for one server a test's clients dial: on Linux, where every address of 127.0.0.0/8
 * is the loopback, one of them chosen at random and never 127.0.0.1; elsewhere 127.0.0.1, the only
 * loopback address such a system brings up by default. Take a new one for each server, and build
 * the clients' URLs from the address the server reports (`server.url`), never from 127.0.0.1.
 */
export function loopbackHost(): string {
  if (process.platform !== "linux") return "127.0.0.1";
  const octet = (from: number, count: number): number => from + Math.floor(Math.random() * count);
  return `127.${octet(1, 254)}.${octet(0, 256)}.${octet(1, 254)}`;
}
