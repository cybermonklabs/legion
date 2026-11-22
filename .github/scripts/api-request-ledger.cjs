// Measurement only; never merged. A Node preload (NODE_OPTIONS=--require <this file>) that writes
// one line to stderr for each HTTP response this process receives from api.github.com or
// uploads.github.com, or that carries a GitHub rate-limit header: the method, host, path, status,
// and the x-ratelimit-resource, x-ratelimit-used and x-ratelimit-remaining headers. It logs no
// other header and no request header, so no token can reach the log.
//
// It reads Node's diagnostics channels, which are process-wide: undici publishes
// `undici:request:headers` (Node's built-in fetch and any copy of undici bundled into an action,
// which is what @actions/github's getProxyFetch uses), and node:http publishes
// `http.client.response.finish`. A line `api-ledger: loaded` per process shows the preload ran.
"use strict";
const dc = require("node:diagnostics_channel");

const API_HOSTS = new Set(["api.github.com", "uploads.github.com"]);
const RATE = ["x-ratelimit-resource", "x-ratelimit-used", "x-ratelimit-remaining"];
const script = (process.argv[1] || "").split("/").slice(-3).join("/");
let count = 0;

function emit(method, host, path, status, get) {
  const rate = RATE.map((name) => get(name));
  if (!API_HOSTS.has(host) && rate[0] === undefined) return;
  count += 1;
  const shown = String(path).slice(0, 160);
  process.stderr.write(
    `api-ledger: ${count} ${method} ${host}${shown} ${status} resource=${rate[0] ?? "-"} used=${rate[1] ?? "-"} remaining=${rate[2] ?? "-"} pid=${process.pid}\n`,
  );
}

dc.subscribe("undici:request:headers", ({ request, response }) => {
  const raw = response.headers || [];
  const get = (wanted) => {
    for (let i = 0; i + 1 < raw.length; i += 2) {
      if (String(raw[i]).toLowerCase() === wanted) return String(raw[i + 1]);
    }
    return undefined;
  };
  let host = "";
  try {
    host = new URL(request.origin).host;
  } catch {
    host = String(request.origin);
  }
  emit(request.method, host, request.path, response.statusCode, get);
});

dc.subscribe("undici:request:error", ({ request, error }) => {
  let host = "";
  try {
    host = new URL(request.origin).host;
  } catch {
    host = String(request.origin);
  }
  if (API_HOSTS.has(host)) {
    process.stderr.write(`api-ledger: error ${request.method} ${host}${String(request.path).slice(0, 160)} ${error && error.code} pid=${process.pid}\n`);
  }
});

dc.subscribe("http.client.response.finish", ({ request, response }) => {
  const host = String(request.host || request.getHeader("host") || "");
  emit(request.method, host, request.path, response.statusCode, (name) => {
    const value = response.headers[name];
    return value === undefined ? undefined : String(value);
  });
});

process.stderr.write(`api-ledger: loaded pid=${process.pid} script=${script}\n`);
process.on("exit", () => {
  process.stderr.write(`api-ledger: exit pid=${process.pid} script=${script} responses=${count}\n`);
});
