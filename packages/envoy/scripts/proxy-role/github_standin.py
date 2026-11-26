#!/usr/bin/env python3
"""A stand-in GitHub for proxy-role-check.sh, served over TLS on one address for api.github.com,
github.com and uploads.github.com, as the broker's proxy reaches it under those names.

It answers what a Legion role's gh, gh extensions and git ask a role's GitHub for: the REST read
of a repository (GET /repos/{owner}/{repo}), GraphQL (POST /graphql: the viewer, and the pull
request list `gh pr list` reads), and git's smart HTTP advertisement
(GET /{owner}/{repo}.git/info/refs?service=git-upload-pack or git-receive-pack, which
`git ls-remote` and `git push --dry-run` read). A call that carries no credential is answered 401,
with git's Basic challenge on github.com, so git asks its credential helper. Any host answers
GET /public with no credential: the public fetch a forwarder opens itself.

Each request it gets is one JSON line appended to --records: the host, the method, the path, and
what credential it carried, by kind and never by value: "app" for an installation token the
minting stand-in mints (`ghs_standin_<owner>`, which a stand that numbers its tokens follows with
`_<n>`), with that owner as "account"; "placeholder" for a broker placeholder, which must never
reach GitHub; "none"; or "other". The token is recorded as the first 16 hex digits of its SHA-256,
so two calls can be told to carry one token, and no line holds a credential.
"""

import argparse
import base64
import hashlib
import http.server
import json
import re
import ssl
import sys
import threading
import urllib.parse

PLACEHOLDER_PREFIX = "agent-secrets-placeholder-"
APP_PREFIX = "ghs_standin_"
# The one commit each repository's main branch names in the advertisement.
HEAD = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"


def credential_of(header):
    """The token an Authorization header carries: `token X`, `Bearer X`, or the password of
    `Basic base64(user:X)`; "" for none."""
    if not header:
        return ""
    scheme, _, value = header.partition(" ")
    scheme = scheme.lower()
    if scheme in ("token", "bearer"):
        return value.strip()
    if scheme == "basic":
        try:
            decoded = base64.b64decode(value.strip()).decode("utf-8", "replace")
        except ValueError:
            return ""
        return decoded.partition(":")[2]
    return ""


def kind_of(token):
    if not token:
        return "none", ""
    if token.startswith(PLACEHOLDER_PREFIX):
        return "placeholder", ""
    if token.startswith(APP_PREFIX):
        return "app", re.sub(r"_[0-9]+$", "", token[len(APP_PREFIX):])
    return "other", ""


def pkt(line):
    data = line.encode()
    return b"%04x" % (len(data) + 4) + data


def advertisement(service):
    caps = "report-status delete-refs side-band-64k agent=git/standin" if service == "git-receive-pack" \
        else "multi_ack side-band-64k ofs-delta agent=git/standin"
    return (pkt("# service=%s\n" % service) + b"0000" +
            pkt("%s refs/heads/main\0%s\n" % (HEAD, caps)) + b"0000")


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    records = None
    lock = threading.Lock()

    def log_message(self, fmt, *args):  # noqa: D401 - the records are the log
        return

    def record(self, token):
        kind, account = kind_of(token)
        entry = {
            "host": (self.headers.get("Host") or "").split(":")[0],
            "method": self.command,
            "path": urllib.parse.urlsplit(self.path).path,
            "kind": kind,
            "account": account,
            "token_sha16": hashlib.sha256(token.encode()).hexdigest()[:16] if token else "",
        }
        with self.lock, open(self.records, "a", encoding="utf-8") as out:
            out.write(json.dumps(entry) + "\n")
        return kind

    def answer(self, status, body, content_type="application/json", headers=None):
        if isinstance(body, str):
            body = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def body(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def handle_any(self):
        token = credential_of(self.headers.get("Authorization"))
        payload = self.body()
        kind = self.record(token)
        url = urllib.parse.urlsplit(self.path)
        host = (self.headers.get("Host") or "").split(":")[0]
        parts = [p for p in url.path.split("/") if p]
        if url.path == "/public":
            return self.answer(200, "public\n", "text/plain")
        if kind != "app":
            if host == "github.com":
                return self.answer(401, "authentication required\n", "text/plain",
                                   {"WWW-Authenticate": 'Basic realm="GitHub"'})
            return self.answer(401, json.dumps({"message": "Bad credentials"}))
        if host == "github.com" and len(parts) == 4 and parts[2:] == ["info", "refs"]:
            requested = urllib.parse.parse_qs(url.query).get("service", [""])[0]
            # The two services git's smart HTTP names, each with its own content type; the query's
            # value picks one and is never written back into a header.
            types = {"git-upload-pack": "application/x-git-upload-pack-advertisement",
                     "git-receive-pack": "application/x-git-receive-pack-advertisement"}
            service = next((name for name in types if name == requested), None)
            if service is None:
                return self.answer(403, "smart HTTP only\n", "text/plain")
            return self.answer(200, advertisement(service), types[service], {"Cache-Control": "no-cache"})
        if host == "api.github.com" and self.command in ("GET", "HEAD") and len(parts) == 3 and parts[0] == "repos":
            owner, repo = parts[1], parts[2]
            return self.answer(200, json.dumps({
                "full_name": "%s/%s" % (owner, repo), "name": repo, "owner": {"login": owner},
                "private": False, "visibility": "public", "default_branch": "main",
            }))
        if host == "api.github.com" and self.command == "POST" and url.path == "/graphql":
            try:
                query = json.loads(payload or b"{}").get("query", "")
            except ValueError:
                query = ""
            if "pullRequests" in query:
                data = {"repository": {"pullRequests": {"totalCount": 0, "nodes": [],
                                                        "pageInfo": {"hasNextPage": False, "endCursor": ""}}}}
            elif "viewer" in query:
                data = {"viewer": {"login": "proxy-role-check[bot]"}}
            else:
                data = {}
            return self.answer(200, json.dumps({"data": data}))
        return self.answer(404, json.dumps({"message": "Not Found"}))

    do_GET = handle_any
    do_HEAD = handle_any
    do_POST = handle_any


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--address", required=True, help="host:port to listen on")
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--records", required=True, help="the JSON-lines file each request is appended to")
    args = parser.parse_args()
    host, _, port = args.address.rpartition(":")
    Handler.records = args.records
    server = http.server.ThreadingHTTPServer((host, int(port)), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(args.cert, args.key)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    print("github-standin listening %s" % args.address, file=sys.stderr, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
