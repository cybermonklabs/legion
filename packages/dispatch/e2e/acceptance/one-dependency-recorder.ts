/** `bun one-dependency-recorder.ts <server> <record file>`: a recording proxy for check
 *  c-one-dependency. It listens on a free port of 127.0.0.1 and prints `{"port": <n>}` as its first
 *  line, then passes each request on to the server unchanged and answers with the server's answer,
 *  status, headers and body, so the client it serves reads what the server wrote. Before it passes a
 *  request on, it appends one JSON line to the record file: the request's method, its path with its
 *  query, and its body as the client sent it. one-dependency.sh points `dispatch issue-update` at it
 *  and reads, off that record, the members the command sent. It runs until it is stopped. */
import { appendFileSync } from "node:fs";

const [server, record] = Bun.argv.slice(2);
if (server === undefined || record === undefined) {
  console.error("usage: bun one-dependency-recorder.ts <server> <record file>");
  process.exit(2);
}
const upstream = new URL(server).origin;

const proxy = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const body = request.method === "GET" || request.method === "HEAD" ? "" : await request.text();
    appendFileSync(
      record,
      `${JSON.stringify({ method: request.method, path: url.pathname + url.search, body })}\n`
    );
    const headers = new Headers(request.headers);
    headers.delete("host");
    headers.delete("content-length");
    const answer = await fetch(upstream + url.pathname + url.search, {
      method: request.method,
      headers,
      body: body === "" ? undefined : body,
      redirect: "manual",
    });
    // fetch has already decoded the body, so the answer names no encoding and no length of its own.
    const answered = new Headers(answer.headers);
    answered.delete("content-encoding");
    answered.delete("content-length");
    return new Response(await answer.arrayBuffer(), { status: answer.status, headers: answered });
  },
});
console.log(JSON.stringify({ port: proxy.port }));
