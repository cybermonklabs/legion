"""The judges' side of the claim's S10 judging: the one program the sandbox runs.

`s10_judge.py` builds each request's five evidence bundles and records the five `agent()` calls
S10's `run_judges` makes, outside the sandbox. It then starts one Kubernetes Job for the request,
whose one container runs this file (sent in an environment variable, so it is no file of the
sandbox) with the bundles and the calls sent beside it. In order, before any judge starts:

1. It writes the bundles at the paths their prompts name, and the calls, onto the Job's one
   `emptyDir`, mounted at `WORK_MOUNT`.
2. It records the names of the environment's variables, the mounts, and a listing of every file,
   and refuses (point 5) if a variable or a file names a Dispatch, Envoy, GitHub, broker or
   transcript-store credential, if a mount is not one of the runtime's, or if a file is outside
   the bundles, the calls and the runtime. The runtime is what `RUNTIME` names: the image's own
   files, the image being the one the driver's own pod runs, by the digest the claim's run digest
   names (`--image`, which the driver refuses to be any other; it checks the digest the pod ran),
   and the listing taken before anything but this program has run in the container; the kernel's
   and the kubelet's own mounts (`/proc`, `/dev`, `/sys`, `/etc/hosts`, `/etc/hostname`,
   `/etc/resolv.conf`); the three mounts of the gateway key, `models.yml` and the overlay; and
   the `emptyDir`. In the roots a run can write (`WRITABLE_ROOTS`), every file, link or other
   entry but a directory must be this program's own, a mount's own, or under a path of the
   image's own runtime that `IMAGE_OWN` names (omp's profile and native module, the shell's own
   files, the caches the image's tools left); no file anywhere may bear the name of a file a
   claim stage writes (`CLAIM_FILE_NAMES`, `CLAIM_FILE_PATTERN`); and outside the work tree no
   file may bear an evidence bundle's name or hold a sent bundle's bytes (`BUNDLE_NAME`,
   `same_size_digests`). A directory the run cannot read is recorded as such: nothing in it can
   reach a judge, which runs as the same user.
3. It measures point 2's runtime by content (`measure_runtime`) and refuses on any difference.
4. It asks each node metadata server for the pod's credential, with the header they require
   (`METADATA_REQUESTS`), and refuses unless every request fails to connect or is refused in the
   400s (`metadata_refusal`): a judge must reach no cloud credential. A server that accepts the
   connection and gives no answer in time is no proof of that, and is refused too. It records
   each status, and never a body.

Each of those refusals stops the run, since every later request would meet the same sandbox. The
driver's preflight, made before each request starts, is named so (`MODE_VARIABLE`) and sends no
file: every check above, no judge, and then `PROBE_CALLS` calls of point 2's model, which the
driver names (`PROBE_MODEL_VARIABLE`), to point 2's gateway with the Job's own key, each in a
session of its own (`probe_gateway`): the path each judge's calls take, from this pod through its
egress to the gateway and on to the model. The driver refuses the preflight unless each call was
answered with a message by that model (`probe_refusal`), so no request starts while the probe
finds the gateway refusing the judges' model.

Then, as point 5 has it (version 6), it records the recursive relative-path and type manifests of
the work tree, `WORK`, and the skill tree (`manifests_now`): the first must hold exactly
`bundles/`, holding exactly the request's ask-id directory, holding exactly the five bundles; the
second exactly the skill's three files of point 2 (`manifest_problem`). Where either does not, it
starts no judge, and the request is a deviation. Otherwise it runs one `omp -p` in `WORK` with the
`eval` tool alone (the scouts it starts keep the tools their definition declares, measured on omp
18.16.0), its transcripts kept as files, asked to make one `eval` call whose code is `CELL`: the
five recorded `agent()` calls and one `wait()` for them, as S10's round waits. omp 18.16.0 has no
command that runs an eval cell without a model, so the session's own model makes that one call;
the driver holds its transcript to that one call. It writes nothing in either tree, and once the
last judge has returned it records both manifests again, which must be the ones it recorded
before.

What comes back is written to the pod's log only (`print_result`): the record, the listing, every
transcript file, the answers and omp's own output, as one xz-compressed tar in base64 lines that
the driver reads with `kubectl logs` and checks by its sha256. The gateway key's bytes are
replaced in every file first (`redact_key`), and the record names each file that held them. This
program decides nothing about an answer: the driver does, from the transcripts, before any answer
is compared.
"""

import base64
import hashlib
import http.client
import io
import json
import lzma
import os
import re
import stat
import subprocess
import sys
import tarfile
import urllib.parse
import uuid

# The runtime of point 2, by content: what the shadow run's own session measured in its pod.
OMP = "/opt/omp/bin/omp"
RUNTIME = {
    "omp": "ebcef88f3a15bee093a566705d56d7d84e1dbfc0630687866a91ccededd6148e",
    "ompVersion": "omp/18.16.0",
    "piEnvoyTree": "ab6fe1cd5ffe5acb21b88413443d1b82d08c14d32c9ed56a8649525fe09f4ac5",
    "piLegionTree": "4d2e009fbdcca36c7ec0ad83f82b4e7f4615c07240724fb1afbf177b6140b326",
    "modelsYml": "e6f9d0a383138011a5fc49a0d02040b56adeaae0fc105ccc3a8913fb9189acd1",
    "overlayYml": "47135ebdcb024770d445c08e02f5a1e19a2fc8d852a4d91540c784fbd027bf28",
}
# Each tree is measured by point 2's recipe, run as written in that directory.
TREES = {"piEnvoyTree": "/opt/legion/pi-envoy/dist", "piLegionTree": "/opt/legion/pi-legion/dist"}
TREE_RECIPE = "find . -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum"

# The Job's mounts: the one item of the gateway key, the two files of the route, the emptyDir.
KEY_MOUNT = "/var/run/model-gateway-key"
MODELS_FILE = "/home/legion/.omp/profiles/legion/agent/models.yml"
OVERLAY_FILE = "/etc/legion-operator/overlay.yml"
WORK_MOUNT = "/work"
# Where the judges run: the directory S10's prompts name their bundles under.
WORK = os.path.join(WORK_MOUNT, "shadow")
# What the program writes beside the bundles, and where omp keeps the transcripts.
CALLS_FILE = "/work/s10/calls.json"
RESULTS_FILE = "/work/s10/results.json"
SESSIONS = "/work/s10/sessions"

# Point 5 (version 6): the directory `skill://approval-review` resolves to (the skill tree), the
# skill's files of point 2 in it, and the recursive relative-path and type manifest each of the two
# trees a judge may list must have, before its first judge and after its last: the skill tree's,
# and the work tree's (`WORK`), which holds the request's five bundles and the directories to them.
SKILL_DIR = "/opt/legion/pi-envoy/dist/skills/approval-review"
SKILL_FILES = ("SKILL.md", "references/never-decide.md", "references/reviewer-principles.md")
SKILL_MANIFEST = sorted([[name, "f"] for name in SKILL_FILES]
                        + [[name, "d"] for name in sorted({os.path.dirname(one) for one in SKILL_FILES} - {""})])
BUNDLE_NAMES = tuple(f"judge-{index}.md" for index in range(1, 6))


def work_manifest(ask_id):
    """The work tree's manifest while the judges of `ask_id` run: `bundles/`, holding exactly the
    ask-id directory, holding exactly the five bundles, each a regular file."""
    return sorted([["bundles", "d"], [f"bundles/{ask_id}", "d"]]
                  + [[f"bundles/{ask_id}/{name}", "f"] for name in BUNDLE_NAMES])


# A file outside the work tree that could be an evidence bundle: one bearing a bundle's name, in
# any case, or one of a sent bundle's size whose bytes are a sent bundle's (`same_size_digests`).
# The work tree holds only what the driver sent, so the five are the only bundles in the sandbox
# (point 5, version 6); a `bundles` directory elsewhere in the image is no bundle.
BUNDLE_NAME = re.compile(r"judge-[1-5]\.md", re.IGNORECASE)

# The mounts the kernel and the kubelet make in every container, which hold no file of a run.
SYSTEM_TREES = ("/proc", "/dev", "/sys")
KUBELET_FILES = ("/etc/hosts", "/etc/hostname", "/etc/resolv.conf")

# The roots the image's own user can write in, or that hold a user's files: what a run, rather
# than the image, could have left there is refused unless it is this program's or a mount's own.
WRITABLE_ROOTS = ("/home", "/root", "/tmp", "/var/tmp", "/run", "/srv", "/mnt", "/media", "/var/mail",
                  "/var/local", "/var/spool", WORK_MOUNT)
# What of those roots is the image's own runtime, by path prefix: the shell's own files, omp's
# profile (its plugins and databases, `models.yml` the mount among them) and native module, the
# caches the image's tools left when it was built, and the system's own run-time entries.
IMAGE_OWN = (
    "/home/legion/.bashrc", "/home/legion/.bash_logout", "/home/legion/.profile",
    "/home/legion/.omp/natives/", "/home/legion/.omp/profiles/legion/",
    "/home/legion/.cache/", "/home/legion/.config/google-chrome-for-testing", "/home/legion/.bun/install/cache/",
    "/home/legion/.local/share/pki/", "/home/legion/.local/share/uv/", "/home/legion/.aws/cli/cache/",
    "/home/legion/go/pkg/mod/", "/tmp/node-compile-cache/", "/tmp/uv-",
    "/run/adduser", "/run/shm", "/var/spool/mail",
)
# The names of the files a claim stage writes (the build, the draw, the inputs, the judging, the
# analysis, the records manifest and the sessions file), none of which may be in the sandbox.
CLAIM_FILE_NAMES = frozenset({
    "snapshot.json", "corpus.jsonl", "counts.json", "trees.json", "draw.json", "slices.json",
    "inventory.jsonl", "s10-inputs.json", "s10-judged.json", "outcomes.jsonl", "outcome.json",
    "report.json", "report.md", "sessions.txt", "confidence-cases.txt", "anchor-descriptors.json",
})
# A request file (`requests/<ask id>.json`) or a page record (`pages/<sha256>.json`) of the inputs.
CLAIM_FILE_PATTERN = re.compile(
    r"/requests/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json$|/pages/[0-9a-f]{64}\.json$")

# The node's metadata servers, each asked for the pod's cloud credential with the header they
# require, `Metadata-Flavor: Google`: the link-local one, and the one Workload Identity serves on
# the node itself. The sandbox goes on only where every request fails to connect or is refused
# with an answer in the 400s, so no credential of the pod's account can reach a judge.
METADATA_REQUESTS = tuple(
    f"{server}/computeMetadata/v1/instance/service-accounts/default/{path}"
    for server in ("http://169.254.169.254", "http://169.254.169.252:988")
    for path in ("token", "identity?audience=s10-judge-sandbox"))
METADATA_HEADER = {"Metadata-Flavor": "Google"}
# What stands in a kept file where the gateway key's bytes were.
KEY_REDACTION = b"[the model gateway key]"

# A variable that names a Dispatch, Envoy, GitHub, broker or transcript-store credential, a token,
# a file of one or the server one is for, or any other secret.
CREDENTIAL_VARIABLE = re.compile(
    r"DISPATCH|ENVOY|GITHUB|(^|_)GH_|AGENT_SECRETS|TRANSCRIPT|TOKEN|SECRET|PASSWORD|CREDENTIAL",
    re.IGNORECASE,
)
# A file that is such a credential, or is where one is kept: a Secret's mount, the broker's key, a
# token file named for its service, git's and gh's stores, and the Envoy configuration that holds
# a Dispatch token. git's own credential helpers (`git-credential-store` and the rest) are programs
# of the image and hold none.
CREDENTIAL_FILE = re.compile(
    r"^/(var/)?run/secrets/"
    r"|^/(var/)?run/(legion/)?agent-secrets/"
    r"|transcript-store"
    r"|(^|/)(dispatch|envoy|github|gh)[-_.]?(token|pat|credentials?)([-_.][^/]*)?$"
    r"|(^|/)\.git-credentials$|(^|/)gh/hosts\.ya?ml$|(^|/)\.netrc$|(^|/)envoy\.json$"
    r"|(^|/)\.config/dispatch/",
    re.IGNORECASE,
)

# The one eval cell: S10's five `agent()` calls, as recorded, and one wait for them all, with the
# timeout and error handling of S10's round (`wait_fn(handles, timeout=1500, raise_errors=False)`).
CELL = (
    "import json\n"
    f"calls = json.load(open({CALLS_FILE!r}))\n"
    'handles = [agent(c["prompt"], agent=c["agent"], label=c["label"], schema=c["schema"], '
    'model=c["model"]) for c in calls]\n'
    "results = wait(handles, timeout=1500, raise_errors=False)\n"
    'json.dump([{"id": h.id, "result": r} for h, r in zip(handles, results)], '
    f"open({RESULTS_FILE!r}, \"w\"), default=repr)\n"
    'print("judged", len(handles))\n'
)

# What the top session is asked: that one call, and nothing else.
PROMPT = (
    "You run one step of an evaluation harness in a sandbox. Call the eval tool exactly once, with "
    'language "py" and exactly the code below, and make no other tool call of any kind. The code '
    f"starts five read-only judge subagents, whose prompts are in {CALLS_FILE}, and waits for "
    "them. Do not read that file or anything else yourself. When the eval call returns, reply "
    "with the one word: done.\n\n```python\n" + CELL + "```\n"
)

# How long omp may run, in seconds: the judges' own 1500 and the session around them.
OMP_TIMEOUT = 3300

# The environment variables the driver sends this program and its payload in, and the run's mode:
# the preflight, with no file and no judge, or a request's.
PROGRAM_VARIABLE = "S10_SANDBOX"
PAYLOAD_VARIABLE = "S10_PAYLOAD_"
PAYLOAD_SHA256_VARIABLE = "S10_PAYLOAD_SHA256"
MODE_VARIABLE = "S10_SANDBOX_MODE"
MODES = ("preflight", "request")

# Point 2's gateway: the `baseUrl` of the `anthropic` provider in point 2's `models.yml`, whose
# bytes `RUNTIME` pins (`modelsYml`), with the path of its `anthropic-messages` calls.
GATEWAY_URL = "http://model-gateway.model-gateway.svc.cluster.local:4000/v1/messages"
# The variable the driver names point 2's model in for the preflight's calls, as `provider/model`.
PROBE_MODEL_VARIABLE = "S10_PROBE_MODEL"
# How many calls of that model the preflight makes, each asking for one token in a session of its
# own, and how long each blocking step of a call may wait, in seconds. The gateway pins each session
# to one of the model's organizations by its id, as it pins each judge's session, so where a share w
# of sessions is refused, thirty calls all pass with odds of (1 - w)^30: under 1 in 20 for w = 1/10,
# and about 5 in a million for w = 1/3, where one of the model's two organizations refuses every
# session and takes a third of them. The first call not answered ends the probe, and the preflight
# Job's deadline bounds the whole probe.
PROBE_CALLS = 30
PROBE_TIMEOUT = 60
PROBE_PROMPT = "Reply with the one word: ready."
# How much of a refused call's error message, or of its body where it gives none, the record keeps.
PROBE_MESSAGE_WIDTH = 300

# The lines the result is printed in, on the pod's log.
RESULT_LINE = "s10-sandbox"
RESULT_END = "s10-sandbox-end"
RESULT_WIDTH = 60000


def sha256_of(data):
    """The sha256 of some bytes, or of a text's UTF-8 bytes."""
    if isinstance(data, str):
        data = data.encode("utf-8")
    return hashlib.sha256(data).hexdigest()


def pack(files):
    """Files by name as one xz-compressed tar in base64: how the result and the payload travel."""
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for name in sorted(files):
            data = files[name]
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o644
            tar.addfile(info, io.BytesIO(data))
    return base64.b64encode(lzma.compress(buffer.getvalue())).decode("ascii")


def unpack(text):
    """The files `pack` packed, by name; a name that leaves its directory is refused."""
    files = {}
    with tarfile.open(fileobj=io.BytesIO(lzma.decompress(base64.b64decode(text))), mode="r") as tar:
        for member in tar.getmembers():
            parts = member.name.split("/")
            if not member.isfile() or member.name.startswith("/") or ".." in parts:
                raise ValueError(f"the packed member {member.name!r} is no file of its own")
            files[member.name] = tar.extractfile(member).read()
    return files


def credential_variables(names):
    """The variable names that name a credential, in order."""
    return [name for name in sorted(names) if CREDENTIAL_VARIABLE.search(name)]


def unescape_mount(field):
    """A mount point as `/proc/self/mountinfo` writes it, with its octal escapes read."""
    return re.sub(r"\\([0-7]{3})", lambda match: chr(int(match.group(1), 8)), field)


def mount_points(mountinfo):
    """The mount points `/proc/self/mountinfo` names, in its order."""
    return [unescape_mount(line.split(" ")[4]) for line in mountinfo.splitlines() if line.strip()]


def under(path, tree):
    """Whether `path` is `tree` or inside it."""
    return path == tree or path.startswith(tree.rstrip("/") + "/")


def mount_refusal(points, layout):
    """Why a mount is not one of the runtime's, or None. `layout` names the real paths."""
    runtime = {"/", layout["key"], layout["models"], layout["overlay"], layout["work"], *KUBELET_FILES}
    for point in points:
        if point in runtime or any(under(point, tree) for tree in SYSTEM_TREES):
            continue
        return f"{point} is a mount outside the runtime"
    for wanted in (layout["key"], layout["models"], layout["overlay"], layout["work"]):
        if wanted not in points:
            return f"{wanted} is not mounted, and the runtime mounts it"
    return None


def listing_of(root="/"):
    """Every entry under `root` but the kernel's trees, one line each, sorted by path.

    A line is the entry's kind (`d` a directory, `f` a file, `l` a link, `o` anything else), its
    mode in octal, its size in bytes and its path, and for a link ` -> ` and its target. Nothing is
    followed through a link."""
    lines = []
    pending = [root]
    while pending:
        directory = pending.pop()
        try:
            entries = list(os.scandir(directory))
        except OSError as error:
            lines.append(f"e 0 0 {directory} ({error.strerror})")
            continue
        for entry in entries:
            path = entry.path
            if any(under(path, tree) for tree in SYSTEM_TREES):
                continue
            facts = entry.stat(follow_symlinks=False)
            mode = stat.S_IMODE(facts.st_mode)
            if stat.S_ISLNK(facts.st_mode):
                lines.append(f"l {mode:o} {facts.st_size} {path} -> {os.readlink(path)}")
            elif stat.S_ISDIR(facts.st_mode):
                lines.append(f"d {mode:o} {facts.st_size} {path}")
                pending.append(path)
            elif stat.S_ISREG(facts.st_mode):
                lines.append(f"f {mode:o} {facts.st_size} {path}")
            else:
                lines.append(f"o {mode:o} {facts.st_size} {path}")
    return sorted(lines, key=lambda line: line.split(" ", 3)[3])


def listed_path(line):
    """The path one line of `listing_of` names."""
    path = line.split(" ", 3)[3]
    return path.split(" -> ", 1)[0] if line.startswith("l ") else path


def listed_size(line):
    """The size in bytes one line of `listing_of` gives, or None where it gives none."""
    fields = line.split(" ", 3)
    return int(fields[2]) if len(fields) == 4 and fields[2].isdigit() else None


def image_own(path, layout):
    """Whether a file under a root a run can write is a mount's own or the image's own runtime."""
    return path in (layout["models"], layout["overlay"]) or path.startswith(IMAGE_OWN)


def same_size_digests(lines, payload, layout):
    """The sha256 of every regular file outside the work tree with the size of a bundle sent, by
    its path, or None for one this program, which runs as the judges' user, cannot read: what
    `listing_refusal` holds to the bundles' own."""
    sizes = {len(data) for path, data in payload.items() if BUNDLE_NAME.fullmatch(os.path.basename(path))}
    digests = {}
    for line in lines:
        path = listed_path(line)
        if line.startswith("f ") and listed_size(line) in sizes and not under(path, layout["work"]):
            try:
                digests[path] = file_sha256(path)
            except OSError:
                digests[path] = None
    return digests


def listing_refusal(lines, payload, layout, same_size=None):
    """Why the listing holds a credential, a claim stage's file, an evidence bundle but the five
    sent, or a file outside the bundles, the calls and the runtime.

    `payload` holds the files the driver sent, by path. Under the `emptyDir` only those, and the
    directories that hold them, may be there; under the gateway key's mount only the key, by
    Kubernetes' own `..data` links to it; under every other root a run can write, only directories
    and the image's own runtime (`image_own`). Outside the `emptyDir`, no file may bear a bundle's
    name (`BUNDLE_NAME`), and none of a sent bundle's size may hold its bytes, by the sha256 the
    sandbox recorded of each (`same_size`, `same_size_digests`): the five bundles are the only ones
    in the sandbox (point 5, version 6). Every other file is the image's own, whose digest the
    driver checks."""
    wanted_directories = {layout["work"]}
    for path in payload:
        parent = os.path.dirname(path)
        while under(parent, layout["work"]) and parent not in wanted_directories:
            wanted_directories.add(parent)
            parent = os.path.dirname(parent)
    bundles = {path: data for path, data in payload.items() if BUNDLE_NAME.fullmatch(os.path.basename(path))}
    sizes = {len(data) for data in bundles.values()}
    digests = {sha256_of(data) for data in bundles.values()}
    placed = set()
    for line in lines:
        path = listed_path(line)
        if CREDENTIAL_FILE.search(path):
            return f"{path} names a credential"
        entry = line[0] in "flo"
        if entry and (os.path.basename(path) in CLAIM_FILE_NAMES or CLAIM_FILE_PATTERN.search(path)):
            return f"{path} bears the name of a file a claim stage writes"
        if entry and not under(path, layout["work"]):
            if BUNDLE_NAME.fullmatch(os.path.basename(path)):
                return f"{path} bears an evidence bundle's name and is none of the five sent"
            if line.startswith("f ") and listed_size(line) in sizes:
                if path not in (same_size or {}):
                    return f"{path} has a sent bundle's size, and the sandbox recorded no sha256 of it"
                if same_size[path] in digests:
                    return f"{path} holds the bytes of an evidence bundle sent"
        if under(path, layout["work"]):
            if path in payload and line.startswith("f "):
                placed.add(path)
            elif not (path in wanted_directories and line.startswith("d ")):
                return f"{path} is a file outside the bundles and the calls"
        elif under(path, layout["key"]) and path != layout["key"]:
            name = os.path.basename(path)
            if name != "key" and not name.startswith(".."):
                return f"{path} is no part of the gateway key's one item"
        elif entry and any(under(path, root) for root in WRITABLE_ROOTS) and not image_own(path, layout):
            return f"{path} is under a root a run can write, and is neither this program's, a mount's nor the image's own"
    missing = sorted(set(payload) - placed)
    if missing:
        return f"{missing[0]} was sent and is not there"
    return None


def manifest_of(directory):
    """A directory's recursive relative-path and type manifest: each entry below it, by its path
    from it and its type (`d` a directory, `f` a regular file, `l` a link, `o` anything else),
    sorted, no link followed. None where `directory` is no directory."""
    try:
        if not stat.S_ISDIR(os.lstat(directory).st_mode):
            return None
    except OSError:
        return None
    entries = []
    pending = [""]
    while pending:
        relative = pending.pop()
        for entry in os.scandir(os.path.join(directory, relative)):
            path = os.path.join(relative, entry.name) if relative else entry.name
            mode = entry.stat(follow_symlinks=False).st_mode
            kind = ("l" if stat.S_ISLNK(mode) else "d" if stat.S_ISDIR(mode)
                    else "f" if stat.S_ISREG(mode) else "o")
            entries.append([path, kind])
            if kind == "d":
                pending.append(path)
    return sorted(entries)


def manifests_now():
    """The manifests of the work tree and of the skill tree, the directory `skill://approval-review`
    resolves to."""
    return {"work": manifest_of(WORK), "skill": manifest_of(os.path.realpath(SKILL_DIR))}


def bundle_manifest_of(work, ask_id):
    """The bundle directory's manifest, taken from the work tree's: each entry below
    `bundles/<ask id>/`, by its path from there. None where the work tree's holds no such
    directory."""
    if not isinstance(work, list) or [f"bundles/{ask_id}", "d"] not in work:
        return None
    prefix = f"bundles/{ask_id}/"
    return sorted([one[0][len(prefix):], one[1]] for one in work
                  if isinstance(one, list) and len(one) == 2 and isinstance(one[0], str) and one[0].startswith(prefix))


# The trees whose manifests a version of point 5's rule holds (`s10_transcripts.RULES`), by the
# name it gives them: from version 4 the bundle directory and the skill tree, from version 6 the
# work tree and the skill tree.
MANIFESTED_TREES = {"bundle": ("bundle directory", "skill tree"), "work": ("work tree", "skill tree")}


def manifest_problem(manifests, ask_id, finished=True, trees="work"):
    """Why the manifests recorded around the judges of `ask_id` break the manifest rule of point 5
    the version naming `trees` writes, or None. Before the first judge: the work tree holds exactly
    `bundles/`, holding exactly the ask-id directory, holding exactly `judge-1.md` to `judge-5.md`
    (`work`), or that directory holds exactly the five (`bundle`); and the skill tree exactly
    `SKILL.md` and `references/` with its two files; each file regular, and no link or other entry
    in either. Once the last judge has returned, the same manifests. `finished` is a result's
    check, which needs both: a result whose manifests before the judges pass and that holds none
    after them broke the rule. Only the check before the first judge, where no judge has run yet,
    passes `finished=False`."""
    if not isinstance(manifests, dict) or not isinstance(manifests.get("before"), dict):
        return "the sandbox recorded no manifests of the work and skill trees before the judges"
    wanted = {"work tree": work_manifest(ask_id), "bundle directory": [[name, "f"] for name in BUNDLE_NAMES],
              "skill tree": SKILL_MANIFEST}

    def held(recorded):
        found = {"work tree": recorded.get("work"), "bundle directory": bundle_manifest_of(recorded.get("work"), ask_id),
                 "skill tree": recorded.get("skill")}
        return {name: found[name] for name in MANIFESTED_TREES[trees]}

    before = held(manifests["before"])
    for name, found in before.items():
        if found != wanted[name]:
            return f"before the first judge the {name}'s manifest is {found}, not {wanted[name]}"
    if not finished:
        return None
    if not isinstance(manifests.get("after"), dict):
        return "the sandbox recorded no manifests of the work and skill trees after the last judge"
    after = held(manifests["after"])
    if after != before:
        return f"after the last judge the manifests are {after}, not the ones recorded before"
    return None


def file_sha256(path):
    """The sha256 of one file's bytes."""
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def measure_runtime():
    """Point 2's runtime as this container holds it, measured the way point 2 measured it."""
    measured = {"omp": file_sha256(OMP)}
    version = subprocess.run([OMP, "--version"], capture_output=True, text=True, timeout=120)
    measured["ompVersion"] = version.stdout.strip()
    for name, directory in TREES.items():
        tree = subprocess.run(["sh", "-c", TREE_RECIPE], cwd=directory, capture_output=True, text=True,
                              timeout=600)
        measured[name] = tree.stdout.split(" ", 1)[0].strip() if tree.returncode == 0 else None
    measured["modelsYml"] = file_sha256(MODELS_FILE)
    measured["overlayYml"] = file_sha256(OVERLAY_FILE)
    measured["piConfigFiles"] = os.environ.get("PI_CONFIG_FILES")
    return measured


def runtime_refusal(measured):
    """Why the measured runtime is not point 2's, or None."""
    for name, wanted in RUNTIME.items():
        if measured.get(name) != wanted:
            return f"the runtime's {name} is {measured.get(name)!r}, and point 2 fixes {wanted!r}"
    if measured.get("piConfigFiles") != OVERLAY_FILE:
        return f"PI_CONFIG_FILES is {measured.get('piConfigFiles')!r}, and point 2 fixes {OVERLAY_FILE!r}"
    return None


def ask_metadata(timeout=10):
    """What each metadata server answers a request for the pod's credential that carries the
    header it requires, never a byte of a body: for each, whether a connection was made
    (`connected`), and the status of its answer, None where it gave none in `timeout` seconds. The
    sandbox refuses unless each request failed to connect or was answered in the 400s
    (`metadata_refusal`)."""
    answers = []
    for url in METADATA_REQUESTS:
        parts = urllib.parse.urlsplit(url)
        connection = http.client.HTTPConnection(parts.hostname, parts.port or 80, timeout=timeout)
        try:
            try:
                connection.connect()
            except OSError as error:
                answers.append({"url": url, "connected": False, "status": None, "error": type(error).__name__})
                continue
            try:
                connection.request("GET", f"{parts.path}?{parts.query}" if parts.query else parts.path,
                                   headers=METADATA_HEADER)
                answers.append({"url": url, "connected": True, "status": connection.getresponse().status})
            except (OSError, http.client.HTTPException) as error:
                answers.append({"url": url, "connected": True, "status": None, "error": type(error).__name__})
        finally:
            connection.close()
    return answers


def metadata_refusal(answers):
    """Why the metadata servers' answers let a judge reach a credential, or None: every request
    failed to connect or was refused in the 400s. A server that took the connection and gave no
    answer is no proof that a judge could not, and is refused."""
    if len(answers or []) != len(METADATA_REQUESTS):
        return "the metadata servers were not each asked"
    for answer in answers:
        status = answer.get("status")
        if status is None and answer.get("connected") is not False:
            return (f"{answer.get('url')} took the connection and gave no answer ({answer.get('error')}), "
                    "which is no proof that a judge could not reach a credential")
        if status is not None and not 400 <= status < 500:
            return f"{answer.get('url')} answered {status} to a request with its header, so a judge could reach a credential"
    return None


def gateway_key():
    """The model gateway key's bytes, as the Job mounts them, without the blanks around them;
    None where there is no key to read."""
    try:
        with open(os.path.join(KEY_MOUNT, "key"), "rb") as handle:
            key = handle.read().strip()
    except OSError:
        return None
    return key or None


def redact_key(files, key):
    """`files` with every occurrence of the gateway key's bytes replaced, and the names of the
    files that held one, so no kept file holds the key."""
    if not key:
        return dict(files), []
    held = sorted(name for name, data in files.items() if key in data)
    return {name: data.replace(key, KEY_REDACTION) for name, data in files.items()}, held


def probe_call(parts, body, headers, timeout, key):
    """One call of the preflight's probe, as `probe_gateway` records it: its status, None where
    none came, and the answer's `type` and `model`; for a call not answered 200 with a message,
    also its error and the start of the message its body gives, with the key's bytes replaced
    before the message is cut. `timeout` bounds each blocking step of the call, not the call."""
    connection = http.client.HTTPConnection(parts.hostname, parts.port or 80, timeout=timeout)
    try:
        connection.request("POST", parts.path, body=body, headers=headers)
        response = connection.getresponse()
        status, data = response.status, response.read()
    except (OSError, http.client.HTTPException, ValueError) as error:
        return {"status": None, "type": None, "model": None, "error": type(error).__name__, "message": None}
    finally:
        connection.close()
    try:
        answer = json.loads(data)
    except ValueError:
        answer = None
    answer = answer if isinstance(answer, dict) else {}
    made = {"status": status, "type": answer.get("type"), "model": answer.get("model")}
    if status != 200 or made["type"] != "message":
        error = answer.get("error") if isinstance(answer.get("error"), dict) else {}
        message = str(error.get("message") or data.decode("utf-8", "replace"))
        if key:
            message = message.replace(key.decode("utf-8", "replace"), KEY_REDACTION.decode("utf-8"))
        made.update({"error": str(error.get("type") or "no message"), "message": message[:PROBE_MESSAGE_WIDTH]})
    return made


def probe_gateway(model, key, url=None, calls=PROBE_CALLS, timeout=PROBE_TIMEOUT):
    """The preflight's calls of `model` (point 2's, as `provider/model`) to the gateway at `url`
    (`GATEWAY_URL` unless a test names another), on the endpoint and model group a judge's calls
    take, with `key`, the Job's own gateway key: `calls` of them, or fewer where one is not answered
    200 with a message, which ends the probe. Unlike a judge's call, each asks for one token, with no
    thinking and no stream, so it shows that the gateway serves the model, not how it serves a long
    turn. Each call is a session of its own, named in `metadata.user_id` in the form whose session
    id the gateway reads, so the gateway pins each call to an organization by that id as it pins
    each judge's session, and not every call to one organization by their shared text. Its record
    names the model and, for each call, its session and what `probe_call` records; never a key or a
    header, and of a body at most `PROBE_MESSAGE_WIDTH` characters."""
    parts = urllib.parse.urlsplit(url or GATEWAY_URL)
    headers = {"content-type": "application/json", "anthropic-version": "2023-06-01"}
    if key:
        headers["x-api-key"] = key.decode("utf-8", "replace")
    made = []
    while len(made) < calls and (not made or "error" not in made[-1]):
        session = f"s10-preflight-{uuid.uuid4()}"
        body = json.dumps({"model": str(model).partition("/")[2], "max_tokens": 1,
                           "metadata": {"user_id": f"user_s10_account_preflight_session_{session}"},
                           "messages": [{"role": "user", "content": PROBE_PROMPT}]}).encode("utf-8")
        made.append({"session": session, **probe_call(parts, body, headers, timeout, key)})
    return {"model": model, "calls": made}


def probe_refusal(probe, model):
    """Why the preflight's probe shows that the gateway does not serve `model`, point 2's, to the
    sandbox, or None: the probe named `model`, and made `PROBE_CALLS` calls, each answered 200 with
    a message by `model`, under its own name or its name without the provider, as
    `s10_transcripts.model_problems` holds a judge's served model."""
    if not isinstance(probe, dict) or probe.get("model") != model or not isinstance(probe.get("calls"), list):
        return f"the preflight recorded no probe of the judges' model {model}"
    served = {model, str(model).split("/", 1)[-1]}
    for index, call in enumerate(probe["calls"], 1):
        call = call if isinstance(call, dict) else {}
        if call.get("status") != 200 or call.get("type") != "message":
            said = f" ({call.get('error')}: {call.get('message')})" if call.get("error") else ""
            return (f"call {index} of the preflight's {PROBE_CALLS} calls of {model} to the gateway was answered "
                    f"{call.get('status')} with no message{said}, so no request starts while the gateway refuses "
                    "the judges' model")
        if not isinstance(call.get("model"), str) or call["model"] not in served:
            return (f"call {index} of the preflight's {PROBE_CALLS} calls of {model} to the gateway was answered by "
                    f"{call.get('model')!r}, so no request starts while the gateway serves the judges another model")
    if len(probe["calls"]) != PROBE_CALLS:
        return f"the preflight made {len(probe['calls'])} calls of {model} to the gateway, not {PROBE_CALLS}"
    return None


def layout_of():
    """The paths the checks hold mounts and files to, with the links on the way followed."""
    return {
        "key": os.path.realpath(KEY_MOUNT),
        "models": os.path.realpath(MODELS_FILE),
        "overlay": os.path.realpath(OVERLAY_FILE),
        "work": os.path.realpath(WORK_MOUNT),
    }


def payload_of(environ):
    """The files the driver sent, by absolute path, checked against the sha256 it sent."""
    chunks = []
    while f"{PAYLOAD_VARIABLE}{len(chunks)}" in environ:
        chunks.append(environ[f"{PAYLOAD_VARIABLE}{len(chunks)}"])
    text = "".join(chunks)
    if sha256_of(text) != environ.get(PAYLOAD_SHA256_VARIABLE):
        raise ValueError("the payload's sha256 is not the one the driver sent")
    files = unpack(text)
    return {"/" + name: data for name, data in files.items()}


def place(files, layout):
    """Writes the sent files, each under the `emptyDir`, at the path its prompt names."""
    for path, data in files.items():
        if os.path.normpath(path) != path or path == layout["work"] or not under(path, layout["work"]):
            raise ValueError(f"{path} is no path under the emptyDir")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as handle:
            handle.write(data)


def run_omp(environ):
    """The one `omp -p` the judges run under, given the `eval` tool alone (the scouts it starts
    keep their own declared tools), with its transcripts as files in `SESSIONS`: its exit status
    (None where it ran out of time), its standard output and its standard error."""
    env = {name: value for name, value in environ.items() if not name.startswith("S10_")}
    env["OMP_SESSION_STORAGE"] = "file"
    try:
        completed = subprocess.run([OMP, "-p", "--tools", "eval", "--session-dir", SESSIONS, PROMPT], cwd=WORK,
                                   env=env, capture_output=True, timeout=OMP_TIMEOUT)
    except subprocess.TimeoutExpired as expired:
        return None, expired.stdout or b"", expired.stderr or b""
    return completed.returncode, completed.stdout, completed.stderr


def collected(record, listing, omp_output):
    """The files the result carries: the listing, omp's output, every transcript, the answers, and
    the record last, each with the gateway key's bytes replaced, the record's own text too; the
    record names the files that held them (`keyFound`)."""
    files = {"listing.txt": ("\n".join(listing) + "\n").encode("utf-8")}
    if omp_output is not None:
        files["omp-stdout.txt"], files["omp-stderr.txt"] = omp_output
    for directory, _, names in os.walk(SESSIONS):
        for name in names:
            path = os.path.join(directory, name)
            with open(path, "rb") as handle:
                files["sessions/" + os.path.relpath(path, SESSIONS)] = handle.read()
    if os.path.exists(RESULTS_FILE):
        with open(RESULTS_FILE, "rb") as handle:
            files["results.json"] = handle.read()
    key = gateway_key()
    files, record["keyFound"] = redact_key(files, key)
    text = (json.dumps(record, indent=2, sort_keys=True) + "\n").encode("utf-8")
    if key and key in text:
        # A field of the record (an answer the probe kept, say) held the key: it is named, and replaced.
        record["keyFound"] = [*record["keyFound"], "record.json"]
        text = (json.dumps(record, indent=2, sort_keys=True) + "\n").encode("utf-8").replace(key, KEY_REDACTION)
    files["record.json"] = text
    return files


def print_result(files):
    """Writes the result to the log: `RESULT_LINE <n> <chunk>` lines, then the count and sha256."""
    text = pack(files)
    chunks = [text[at:at + RESULT_WIDTH] for at in range(0, len(text), RESULT_WIDTH)]
    for index, chunk in enumerate(chunks):
        print(f"{RESULT_LINE} {index} {chunk}", flush=True)
    print(f"{RESULT_END} {len(chunks)} {sha256_of(text)}", flush=True)


def checked(environ, layout, record):
    """Places the payload and makes every check before any judge, into `record`, the layout the
    checks held the mounts and files to among it, so the driver makes them again; returns the
    listing and the reason the sandbox refuses to start a judge, or None. The mode its variable
    names must be one of `MODES`, and a preflight's payload holds no file and a request's some."""
    mode = environ.get(MODE_VARIABLE)
    if mode not in MODES:
        raise ValueError(f"{MODE_VARIABLE} names the mode {mode!r}, none of {list(MODES)}")
    files = payload_of(environ)
    if (mode == "preflight") != (not files):
        raise ValueError(f"a {mode} was sent {len(files)} files")
    place(files, layout)
    record["layout"] = layout
    record["payload"] = {path: sha256_of(data) for path, data in sorted(files.items())}
    bundle_dirs = {os.path.dirname(path) for path in files if path.endswith(".md")}
    record["bundleDirectory"] = bundle_dirs.pop() if len(bundle_dirs) == 1 else None
    record["environment"] = sorted(environ)
    with open("/proc/self/mountinfo") as handle:
        record["mounts"] = mount_points(handle.read())
    listing = listing_of("/")
    record["listing"] = {"lines": len(listing), "sha256": sha256_of("\n".join(listing) + "\n")}
    named = credential_variables(environ)
    if named:
        return listing, f"the variable {named[0]} names a credential"
    record["sameSize"] = same_size_digests(listing, files, layout)
    refusal = mount_refusal(record["mounts"], layout) or listing_refusal(listing, files, layout, record["sameSize"])
    if refusal is not None:
        return listing, refusal
    record["runtime"] = measure_runtime()
    refusal = runtime_refusal(record["runtime"])
    if refusal is not None:
        return listing, refusal
    record["metadata"] = ask_metadata()
    return listing, metadata_refusal(record["metadata"])


def main():
    environ = dict(os.environ)
    record = {"refused": None}
    listing = []
    try:
        listing, record["refused"] = checked(environ, layout_of(), record)
    except Exception as error:  # noqa: BLE001 - any failure before a judge is a refusal, reported
        record["refused"] = f"the sandbox failed before any judge: {type(error).__name__}: {error}"
    # The driver's preflight, as its mode names it: the checks above, no judge, and the probe of
    # point 2's model through the gateway once every check has passed.
    record["preflight"] = environ.get(MODE_VARIABLE) == "preflight"
    if record["refused"] is None and record["preflight"]:
        record["probe"] = probe_gateway(environ.get(PROBE_MODEL_VARIABLE), gateway_key())
    omp_output = None
    if record["refused"] is None and not record["preflight"]:
        ask_id = os.path.basename(record["bundleDirectory"]) if record.get("bundleDirectory") else None
        record["manifests"] = {"before": manifests_now()}
        if manifest_problem(record["manifests"], ask_id, finished=False) is None:
            returncode, stdout, stderr = run_omp(environ)
            record["omp"] = {"returncode": returncode}
            omp_output = (stdout, stderr)
            record["manifests"]["after"] = manifests_now()
        record["manifest"] = manifest_problem(record["manifests"], ask_id)
    print_result(collected(record, listing, omp_output))
    return 0


if __name__ == "__main__":
    sys.exit(main())
