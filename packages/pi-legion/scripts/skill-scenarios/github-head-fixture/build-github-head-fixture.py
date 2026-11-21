#!/usr/bin/env python3
"""Build the tester's fixture for the two records the world gained at 5c34544f: the GitHub
stand-in's log (logs/github.log) and the head watcher's log (logs/heads.log).

  build-github-head-fixture.py <out dir>

One directory per case, holding only the files the two rules read: services-from, dispatch.json,
world.json, logs/github.log, logs/heads.log, pushes.log and, where a case has one, a bare
remote.git. manifest.json lists each case: its rule, its scenario, what it is, and the verdict the
tester's reader gives it with that reader's exact reason text (world-health.py's github_log_reason
or head_reason). That reader is the tester's own and lives outside this tree, so each case carries
its reading as data (`reason=`), the commit ids filled in from the commits this build makes; the
tester's reader gave every case this verdict and this reason on 2026-11-16, over a build of this
script. github-standin.test.ts builds the fixture with this script and holds health's rules 18 and
19 to it.

The commits are made at a fixed date by a fixed identity, in a git environment of the build's own
(`ENV`), so their ids are the same on every machine: git reads GIT_AUTHOR_*, GIT_COMMITTER_* and
the GIT_CONFIG_* pairs before any -c setting, and a Legion pod exports all of them, so a build that
inherited them committed as the pod's App, at ids no other machine makes. Each bare remote is
cloned from the build's scratch, which is gone once the build ends, so the url its origin keeps is
set to ORIGIN, a path of no machine."""
import json, os, shutil, subprocess, sys

OUT = os.path.abspath(sys.argv[1])
shutil.rmtree(OUT, ignore_errors=True); os.makedirs(OUT)
ROUTES = {"GET /repos/{owner}/{repo}/installation", "POST /app/installations/{id}/access_tokens",
          "GET /repos/{owner}/{repo}/commits/{sha}", "GET /repos/{owner}/{repo}",
          "GET /repos/{owner}/{repo}/commits/{sha}/pulls", "GET /repos/{owner}/{repo}/pulls/{number}"}
L = {"at": "2026-11-16T17:28:53.474Z", "listening": "127.0.0.1:20491"}
G = {"at": "2026-11-16T17:29:08.200Z", "method": "GET", "path": "/repos/fernbrook/widgets/commits/54ff56ea0137d652dec48b5b202afcea40a48581",
     "accept": "application/vnd.github.sha", "status": 200, "route": "GET /repos/{owner}/{repo}/commits/{sha}"}
M = {"at": "2026-11-16T17:29:08.160Z", "method": "POST", "path": "/app/installations/1/access_tokens",
     "accept": "application/vnd.github+json", "status": 201, "route": "POST /app/installations/{id}/access_tokens"}
O = {"at": "2026-11-16T17:29:08.508Z", "opened": "heads.log"}
ORIGIN = "/fixture/src"
IDENTITY, DATE = ("fixture", "fixture@example.invalid"), "2026-11-16T00:00:00Z"
ENV = {name: value for name, value in os.environ.items()
       if not name.startswith(("GIT_AUTHOR_", "GIT_COMMITTER_", "GIT_CONFIG"))}
ENV.update(GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1",
           GIT_AUTHOR_NAME=IDENTITY[0], GIT_AUTHOR_EMAIL=IDENTITY[1], GIT_AUTHOR_DATE=DATE,
           GIT_COMMITTER_NAME=IDENTITY[0], GIT_COMMITTER_EMAIL=IDENTITY[1], GIT_COMMITTER_DATE=DATE)
def g(*a, cwd):
    return subprocess.run(["git", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false", *a], cwd=cwd,
                          capture_output=True, text=True, check=True, env=ENV).stdout.strip()
src = os.path.join(OUT, ".src"); os.makedirs(src)
g("init", "-q", "-b", "main", cwd=src); open(src + "/greet.ts", "w").write("one\n"); g("add", "greet.ts", cwd=src); g("commit", "-qm", "one", cwd=src)
SEED = g("rev-parse", "HEAD", cwd=src)
g("checkout", "-qb", "legion/WID-1", cwd=src); open(src + "/greet.ts", "w").write("two\n"); g("commit", "-qam", "two", cwd=src)
PUSHED = g("rev-parse", "HEAD", cwd=src)
manifest = []
def case(slug, rule, what, *, reason, prov=("github-standin.ts", "head-watcher.ts"), scenario="reviewer-text-findings", dispatch=True,
         world=True, github=None, heads=None, pushes=None, remote=None):
    n = len(manifest) + 1
    name = f"{n:02d}-{slug}"; r = os.path.join(OUT, name); os.makedirs(r + "/logs")
    open(r + "/services-from", "w").write("".join(f"{p} shared /rig sha256:0\n" for p in prov) + "nats shared nats://127.0.0.1:4229 -\n")
    if dispatch: open(r + "/dispatch.json", "w").write(json.dumps({"database": "run_x", "port": 1, "url": "http://127.0.0.1:1"}) + "\n")
    if world: open(r + "/world.json", "w").write(json.dumps({"key": "WID-1", "repo": "fernbrook/widgets", "pr": 1001, "branch": "legion/WID-1", "base": "main", "head": SEED}) + "\n")
    for file, lines in (("logs/github.log", github), ("logs/heads.log", heads)):
        if lines is not None:
            open(os.path.join(r, file), "w").write("".join((l if isinstance(l, str) else json.dumps(l)) + "\n" for l in lines))
    if pushes is not None:
        open(r + "/pushes.log", "w").write("".join(f"2026-11-16T17:40:00.000Z {ref} {SEED} {commit}\n" for ref, commit in pushes))
    if remote is not None:
        g("clone", "-q", "--bare", src, r + "/remote.git", cwd=OUT); g("update-ref", "refs/heads/legion/WID-1", remote, cwd=r + "/remote.git")
        g("remote", "set-url", "origin", ORIGIN, cwd=r + "/remote.git")
    manifest.append({"case": name, "rule": rule, "scenario": scenario, "what": what,
                     "files": sorted(os.path.relpath(os.path.join(d, f), r) for d, _, fs in os.walk(r) for f in fs if "remote.git" not in d) + (["remote.git/"] if remote else []),
                     "verdict": "healthy" if reason is None else "unhealthy", "reason": reason})
B = "refs/heads/legion/WID-1"
# --- the GitHub stand-in's log
case("older-rig-no-log", "github", "the run's services-from names no github-standin.ts (a run of a rig before the stand-in), and it has no log", prov=(),
     reason=None)
case("no-log", "github", "the stand-in's rig, a Dispatch of its own, and no logs/github.log",
     reason="github: no logs/github.log, in a run whose rig starts a GitHub stand-in for its Dispatch")
case("no-dispatch-no-log", "github", "the stand-in's rig, a run with no Dispatch of its own (no dispatch.json), and no log", scenario="ask-on-message", dispatch=False, world=False,
     reason=None)
case("empty", "github", "an empty log", github=[],
     reason="github: logs/github.log is empty")
case("unopened", "github", "a log whose first line is a request, not the listening line", github=[G],
     reason="github: logs/github.log does not open with a listening line")
case("not-json", "github", "a line that is not JSON", github=[L, G, "panic: boom"],
     reason="github: logs/github.log:3 is not JSON: 'panic: boom'")
case("refused", "github", "a refused request (501, no route)", github=[L, {**{k: v for k, v in G.items() if k != "route"}, "path": "/repos/fernbrook/widgets/branches/main", "status": 501, "refused": "no route of the world answers it"}],
     reason="github refusal: logs/github.log:2 GET /repos/fernbrook/widgets/branches/main answered 501: no route of the world answers it")
case("status-422", "github", "a 422 on a served route", github=[L, {**G, "path": "/repos/fernbrook/widgets/commits/0123456789abcdef0123456789abcdef01234567", "status": 422}],
     reason="github refusal: logs/github.log:2 GET /repos/fernbrook/widgets/commits/0123456789abcdef0123456789abcdef01234567 answered 422 on GET /repos/{owner}/{repo}/commits/{sha}")
case("status-404", "github", "a 404 on a served route", github=[L, {**G, "path": "/repos/fernbrook/widgets/pulls/9", "accept": "application/vnd.github+json", "status": 404, "route": "GET /repos/{owner}/{repo}/pulls/{number}"}],
     reason="github refusal: logs/github.log:2 GET /repos/fernbrook/widgets/pulls/9 answered 404 on GET /repos/{owner}/{repo}/pulls/{number}")
case("route-outside-table", "github", "a 200 whose route the rig's table does not hold", github=[L, {**G, "path": "/repos/fernbrook/widgets/branches/main", "route": "GET /repos/{owner}/{repo}/branches/{branch}"}],
     reason="github: logs/github.log:2 GET /repos/fernbrook/widgets/branches/main names the route 'GET /repos/{owner}/{repo}/branches/{branch}', which the rig's table does not hold")
case("mint-200", "github", "the token mint answered 200, where GitHub answers 201", github=[L, {**M, "status": 200}],
     reason="github: logs/github.log:2 POST /app/installations/1/access_tokens answered 200 on POST /app/installations/{id}/access_tokens, where GitHub answers 201")
case("read-201", "github", "a read answered 201, where GitHub answers 200", github=[L, {**G, "status": 201}],
     reason="github: logs/github.log:2 GET /repos/fernbrook/widgets/commits/54ff56ea0137d652dec48b5b202afcea40a48581 answered 201 on GET /repos/{owner}/{repo}/commits/{sha}, where GitHub answers 200")
case("clean", "github", "the listening line, a mint at 201 and two reads at 200", github=[L, M, G, G],
     reason=None)
case("second-listening", "github", "a second listening line after a request", github=[L, G, L],
     reason="github: logs/github.log:3 is a second listening line: the stand-in started twice")
case("log-norig-table", "github", "a clean log, read with a rig that has no github-routes.ts", github=[L, G],
     reason="github: the run kept a logs/github.log, and the rig given (--world-cmd) has no github-routes.ts to read it against")
case("no-request-world", "github", "the listening line alone, in a run with a world (world.json)", github=[L],
     reason="github: logs/github.log holds its listening line and no request: the run's Dispatch never asked its GitHub")
case("no-request-no-world", "github", "the listening line alone, in a run with no world.json", scenario="ask-on-message", world=False, github=[L],
     reason=None)
case("no-status", "github", "a request line with no status", github=[L, {k: v for k, v in G.items() if k != "status"}],
     reason="github: logs/github.log:2 is no request line (no method, path or status)")
# --- the head watcher's log
case("no-watcher-rig", "head", "the run's services-from names no head-watcher.ts", prov=("github-standin.ts",), remote=SEED,
     reason=None)
case("no-world", "head", "the watcher's rig, a run with no world.json", scenario="ask-on-message", world=False,
     reason=None)
case("no-heads-log", "head", "a world, no push, and no logs/heads.log", remote=SEED, pushes=[],
     reason="head: no logs/heads.log, in a run whose rig starts a head watcher for its world")
case("empty-heads-log", "head", "a world, no push, and an empty logs/heads.log", remote=SEED, pushes=[], heads=[],
     reason="head: logs/heads.log is empty")
case("unopened-heads-log", "head", "a heads.log that opens with a push line", remote=PUSHED, pushes=[(B, PUSHED)], heads=[{"at": "t", "ref": B, "pushed": PUSHED, "ok": True}],
     reason="head: logs/heads.log does not open with its opened line")
case("no-push", "head", "no push, the remote at the seeded head, the record opened", remote=SEED, pushes=[], heads=[O],
     reason=None)
case("push-stored", "head", "one push, stored", remote=PUSHED, pushes=[(B, PUSHED)], heads=[O, {"at": "t", "ref": B, "pushed": PUSHED, "ok": True}],
     reason=None)
case("push-no-line", "head", "one push, and no line storing it", remote=PUSHED, pushes=[(B, PUSHED)], heads=[O],
     reason=f"head: pushes.log:1 pushed {PUSHED[:12]} to {B}, and logs/heads.log holds no line storing it")
case("push-failed", "head", "one push the watcher could not store", remote=PUSHED, pushes=[(B, PUSHED)], heads=[O, {"at": "t", "ref": B, "pushed": PUSHED, "ok": False, "error": "connection refused"}],
     reason=f"head: pushes.log:1 pushed {PUSHED[:12]} to {B}, and the watcher could not store it: connection refused")
case("push-failed-then-stored", "head", "one push, a failed line and then a stored line", remote=PUSHED, pushes=[(B, PUSHED)], heads=[O, {"at": "t", "ref": B, "pushed": PUSHED, "ok": False, "error": "connection refused"}, {"at": "t", "ref": B, "pushed": PUSHED, "ok": True}],
     reason=None)
case("remote-moved-unlogged", "head", "the remote's branch moved off the seeded head, and pushes.log holds no line of it", remote=PUSHED, pushes=[], heads=[O],
     reason=f"head: the remote's {B} stands at {PUSHED[:12]}, which is not the seeded head and logs/heads.log holds no line storing it")
case("push-other-branch", "head", "a push to refs/heads/main alone", remote=SEED, pushes=[("refs/heads/main", PUSHED)], heads=[O],
     reason=None)
case("heads-not-json", "head", "a heads.log line that is not JSON", remote=SEED, pushes=[], heads=[O, "garbage"],
     reason="head: logs/heads.log:2 is not JSON: 'garbage'")
case("no-pushes-log", "head", "a stored push in the record, the remote moved, and no pushes.log at all", remote=PUSHED, heads=[O, {"at": "t", "ref": B, "pushed": PUSHED, "ok": True}],
     reason=None)
shutil.rmtree(src)
json.dump({"made_by": "build-github-head-fixture.py, with the reading of the tester's world-health.py (github_log_reason and head_reason) recorded for each case",
           "seeded_head": SEED, "pushed_head": PUSHED, "routes": sorted(ROUTES), "cases": manifest}, open(os.path.join(OUT, "manifest.json"), "w"), indent=1)
for m in manifest:
    print(f"{m['case']:34s} {m['rule']:6s} {m['verdict']:9s} {m['reason'] or ''}"[:230])
print(len(manifest), "cases;", sum(m["verdict"] == "unhealthy" for m in manifest), "unhealthy")
