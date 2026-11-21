import { randomBytes } from "node:crypto";
import { closeSync, mkdirSync, openSync, readFileSync, writeFileSync } from "node:fs";
import {
  type AddressInfo,
  createConnection,
  createServer,
  type Server,
  type Socket,
} from "node:net";
import * as path from "node:path";
import { awaitReady } from "./envoy-stack";

/** A real TLS front door in an owned, capped container. It claims no fixed host port and changes
 * no host settings. Only this container's address is allowed by the broker's test dial policy. */
export async function resultsTLSFiles(work: string): Promise<{
  readonly host: string;
  readonly address: string;
  readonly url: string;
  readonly ca: string;
  readonly key: string;
  start(upstream: string): Promise<void>;
  stop(): Promise<void>;
}> {
  mkdirSync(work, { recursive: true });
  const host = `dispatch-${randomBytes(8).toString("hex")}.test`;
  const ca = path.join(work, "ca.pem");
  const key = path.join(work, "ca-key.pem");
  const certificate = path.join(work, "server.pem");
  const serverKey = path.join(work, "server-key.pem");
  function openssl(...args: string[]): void {
    const ran = Bun.spawnSync(["openssl", ...args], { cwd: work, stdout: "pipe", stderr: "pipe" });
    if (ran.exitCode !== 0)
      throw new Error(`CI TLS certificate generation: ${ran.stderr.toString()}`);
  }
  openssl(
    "req",
    "-x509",
    "-newkey",
    "rsa:2048",
    "-nodes",
    "-keyout",
    key,
    "-out",
    ca,
    "-days",
    "1",
    "-subj",
    "/CN=CI results test authority"
  );
  const request = path.join(work, "server.csr");
  openssl(
    "req",
    "-newkey",
    "rsa:2048",
    "-nodes",
    "-keyout",
    serverKey,
    "-out",
    request,
    "-subj",
    `/CN=${host}`,
    "-addext",
    `subjectAltName=DNS:${host}`
  );
  openssl(
    "x509",
    "-req",
    "-in",
    request,
    "-CA",
    ca,
    "-CAkey",
    key,
    "-CAcreateserial",
    "-out",
    certificate,
    "-days",
    "1",
    "-copy_extensions",
    "copy"
  );
  const script = path.join(work, "tls-front.py");
  writeFileSync(
    script,
    `import http.client, http.server, json, ssl, urllib.parse\nclass Handler(http.server.BaseHTTPRequestHandler):\n def handle_request(self):\n  with open('/work/upstream.json') as config: upstream=urllib.parse.urlsplit(json.load(config))\n  data=self.rfile.read(int(self.headers.get('Content-Length','0')))\n  connection=http.client.HTTPConnection(upstream.hostname,upstream.port,timeout=30)\n  headers={key:value for key,value in self.headers.items() if key.lower() not in ('host','connection','transfer-encoding')}\n  headers['Host']=upstream.netloc\n  try:\n   connection.request(self.command,self.path,body=data,headers=headers)\n   answer=connection.getresponse()\n   body=answer.read()\n   self.send_response(answer.status)\n   for key,value in answer.getheaders():\n    if key.lower() not in ('connection','transfer-encoding','content-length','server','date'): self.send_header(key,value)\n   self.send_header('Content-Length',str(len(body)))\n   self.end_headers()\n   self.wfile.write(body)\n  finally: connection.close()\n do_GET=do_POST=do_PATCH=do_PUT=do_DELETE=handle_request\n def log_message(self,format,*args): pass\nserver=http.server.ThreadingHTTPServer(('0.0.0.0',443),Handler)\ncontext=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)\ncontext.load_cert_chain('/work/server.pem','/work/server-key.pem')\nserver.socket=context.wrap_socket(server.socket,server_side=True)\nprint('tls-ready',flush=True)\nserver.serve_forever()\n`
  );
  const name = `ci-results-tls-${crypto.randomUUID()}`;
  const log = path.join(work, "tls-front.log");
  const fd = openSync(log, "w");
  const child = Bun.spawn(
    [
      "docker",
      "run",
      "--rm",
      "--init",
      "--name",
      name,
      "--label",
      `owner=${name}`,
      "--cap-drop",
      "ALL",
      "--cap-add",
      "NET_BIND_SERVICE",
      "--cap-add",
      "DAC_OVERRIDE",
      "--memory",
      "128m",
      "--cpus",
      "1",
      "--mount",
      `type=bind,src=${work},dst=/work,readonly`,
      "python:3.12-slim",
      "python3",
      "/work/tls-front.py",
    ],
    { cwd: work, stdin: "ignore", stdout: fd, stderr: fd }
  );
  closeSync(fd);
  let bridge: Server | undefined;
  const connections = new Set<Socket>();
  async function stop(): Promise<void> {
    for (const socket of connections) socket.destroy();
    if (bridge !== undefined) await new Promise<void>((resolve) => bridge?.close(() => resolve()));
    if (child.exitCode === null) {
      const stopping = Bun.spawn(["docker", "stop", "--time", "1", name], {
        stdout: "ignore",
        stderr: "ignore",
      });
      await stopping.exited;
    }
    await child.exited;
  }
  try {
    await awaitReady(
      "CI Dispatch TLS front door",
      child,
      log,
      { timeoutMs: 180_000, tailLines: 50 },
      async () => readFileSync(log, "utf8").includes("tls-ready")
    );
    const inspected = Bun.spawnSync(
      [
        "docker",
        "inspect",
        "--format",
        "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.Gateway}}{{end}}",
        name,
      ],
      { stdout: "pipe", stderr: "pipe" }
    );
    const [address, gateway] = inspected.stdout.toString().trim().split(/\s+/);
    if (
      inspected.exitCode !== 0 ||
      address === undefined ||
      gateway === undefined ||
      address === "" ||
      gateway === ""
    )
      throw new Error("the CI TLS container has no readable network address");
    return {
      host,
      address,
      url: `https://${host}`,
      ca,
      key,
      async start(upstream) {
        const target = new URL(upstream);
        bridge = createServer((socket) => {
          connections.add(socket);
          const toDispatch = createConnection({ host: target.hostname, port: Number(target.port) });
          socket.on("error", () => toDispatch.destroy());
          toDispatch.on("error", () => socket.destroy());
          socket.on("close", () => {
            connections.delete(socket);
            toDispatch.destroy();
          });
          toDispatch.on("close", () => socket.destroy());
          socket.pipe(toDispatch).pipe(socket);
        });
        const listener = bridge;
        await new Promise<void>((resolve, reject) => {
          listener.once("error", reject);
          listener.listen(0, gateway, () => {
            listener.removeListener("error", reject);
            resolve();
          });
        });
        const relay = new URL(upstream);
        relay.hostname = gateway;
        relay.port = String((listener.address() as AddressInfo).port);
        writeFileSync(path.join(work, "upstream.json"), JSON.stringify(relay.toString()));
      },
      stop,
    };
  } catch (error) {
    await stop();
    throw error;
  }
}
