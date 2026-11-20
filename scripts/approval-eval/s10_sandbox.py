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
   files, the caches the image's tools left); and no file anywhere may bear the name of a file a
   claim stage writes (`CLAIM_FILE_NAMES`, `CLAIM_FILE_PATTERN`). A directory the run cannot read
   is recorded as such: nothing in it can reach a judge, which runs as the same user.
3. It measures point 2's runtime by content (`measure_runtime`) and refuses on any difference.
4. It asks each node metadata server for the pod's credential, with the header they require
   (`METADATA_REQUESTS`), and refuses unless every request fails to connect or is refused in the
   400s (`metadata_refusal`): a judge must reach no cloud credential. It records each status, and
   never a body.

Each of those refusals stops the run, since every later request would meet the same sandbox.
Then, as point 5 has it from version 4 on, it records the recursive relative-path and type manifests
of the bundle directory and the skill directory (`manifests_now`): the first must hold exactly the
five bundles, the second exactly the skill's three files of point 2 (`manifest_problem`). Where
either does not, it starts no judge, and the request is a deviation. Otherwise it runs one
`omp -p` in `WORK` with the `eval` tool alone (the scouts it starts keep the tools their
definition declares, measured on omp 18.16.0), its transcripts kept as files, asked to make one
`eval` call whose code is `CELL`: the five recorded `agent()` calls and one `wait()` for them, as
S10's round waits. omp 18.16.0 has no command that runs an eval cell without a model, so the
session's own model makes that one call; the driver holds its transcript to that one call. It
writes nothing in either directory, and once the last judge has returned it records both
manifests again, which must be the ones it recorded before.

What comes back is written to the pod's log only (`print_result`): the record, the listing, every
transcript file, the answers and omp's own output, as one xz-compressed tar in base64 lines that
the driver reads with `kubectl logs` and checks by its sha256. The gateway key's bytes are
replaced in every file first (`redact_key`), and the record names each file that held them. This
program decides nothing about an answer: the driver does, from the transcripts, before any answer
is compared.
"""

import base64
import hashlib
import io
import json
import lzma
import os
import re
import stat
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request

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

# Point 5, from version 4: the directory `skill://approval-review` resolves to, the skill's files of
# point 2 in it, and the recursive relative-path and type manifest each of the two directories a
# judge may `glob` or `read` must have, before its first judge and after its last.
SKILL_DIR = "/opt/legion/pi-envoy/dist/skills/approval-review"
SKILL_FILES = ("SKILL.md", "references/never-decide.md", "references/reviewer-principles.md")
SKILL_MANIFEST = [["SKILL.md", "f"], ["references", "d"], ["references/never-decide.md", "f"],
                  ["references/reviewer-principles.md", "f"]]
BUNDLE_MANIFEST = [[f"judge-{index}.md", "f"] for index in range(1, 6)]

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

# The environment variables the driver sends this program and its payload in.
PROGRAM_VARIABLE = "S10_SANDBOX"
PAYLOAD_VARIABLE = "S10_PAYLOAD_"
PAYLOAD_SHA256_VARIABLE = "S10_PAYLOAD_SHA256"

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


def image_own(path, layout):
    """Whether a file under a root a run can write is a mount's own or the image's own runtime."""
    return path in (layout["models"], layout["overlay"]) or path.startswith(IMAGE_OWN)


def listing_refusal(lines, payload, layout):
    """Why the listing holds a credential, a claim stage's file, or a file outside the bundles, the
    calls and the runtime.

    `payload` names the files the driver sent, by path. Under the `emptyDir` only those, and the
    directories that hold them, may be there; under the gateway key's mount only the key, by
    Kubernetes' own `..data` links to it; under every other root a run can write, only directories
    and the image's own runtime (`image_own`). Every other file is the image's own, whose digest the
    driver checks."""
    wanted_directories = {layout["work"]}
    for path in payload:
        parent = os.path.dirname(path)
        while under(parent, layout["work"]) and parent not in wanted_directories:
            wanted_directories.add(parent)
            parent = os.path.dirname(parent)
    placed = set()
    for line in lines:
        path = listed_path(line)
        if CREDENTIAL_FILE.search(path):
            return f"{path} names a credential"
        entry = line[0] in "flo"
        if entry and (os.path.basename(path) in CLAIM_FILE_NAMES or CLAIM_FILE_PATTERN.search(path)):
            return f"{path} bears the name of a file a claim stage writes"
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


def manifests_now(bundle_dir):
    """The manifests of the bundle directory and of the directory `skill://approval-review`
    resolves to."""
    return {"bundle": None if bundle_dir is None else manifest_of(bundle_dir),
            "skill": manifest_of(os.path.realpath(SKILL_DIR))}


def manifest_problem(manifests, finished=True):
    """Why the manifests recorded around one request's judges break point 5 (version 5), or None:
    before the first judge, the bundle directory holds exactly `judge-1.md` to `judge-5.md` and the
    skill directory exactly `SKILL.md` and `references/` with its two files, each a regular file,
    and no link or other entry in either; once the last judge has returned, the same manifests.
    `finished` is a result's check, which needs both: a result whose manifests before the judges
    pass and that holds none after them broke the rule. Only the check before the first judge,
    where no judge has run yet, passes `finished=False`."""
    if not isinstance(manifests, dict) or not isinstance(manifests.get("before"), dict):
        return "the sandbox recorded no manifests of the bundle and skill directories before the judges"
    before = manifests["before"]
    for name, wanted in (("bundle", BUNDLE_MANIFEST), ("skill", SKILL_MANIFEST)):
        if before.get(name) != wanted:
            return f"before the first judge the {name} directory's manifest is {before.get(name)}, not {wanted}"
    if not finished:
        return None
    if not isinstance(manifests.get("after"), dict):
        return "the sandbox recorded no manifests of the bundle and skill directories after the last judge"
    if manifests["after"] != before:
        return f"after the last judge the manifests are {manifests['after']}, not the ones recorded before"
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


def ask_metadata():
    """What each metadata server answers a request for the pod's credential that carries the
    header it requires: the status of each answer, None where no connection was made, and never
    a byte of a body. The sandbox refuses unless each is None or in the 400s (`metadata_refusal`)."""
    answers = []
    for url in METADATA_REQUESTS:
        request = urllib.request.Request(url, headers=METADATA_HEADER)
        try:
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=10) as response:
                answers.append({"url": url, "status": response.status})
        except urllib.error.HTTPError as error:
            answers.append({"url": url, "status": error.code})
        except OSError as error:
            answers.append({"url": url, "status": None, "error": type(error).__name__})
    return answers


def metadata_refusal(answers):
    """Why the metadata servers' answers let a judge reach a credential, or None: every request
    failed to connect or was refused in the 400s."""
    if len(answers or []) != len(METADATA_REQUESTS):
        return "the metadata servers were not each asked"
    for answer in answers:
        status = answer.get("status")
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
    the record last, each with the gateway key's bytes replaced; the record names the files that
    held them (`keyFound`)."""
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
    files, record["keyFound"] = redact_key(files, gateway_key())
    files["record.json"] = (json.dumps(record, indent=2, sort_keys=True) + "\n").encode("utf-8")
    return files


def print_result(files):
    """Writes the result to the log: `RESULT_LINE <n> <chunk>` lines, then the count and sha256."""
    text = pack(files)
    chunks = [text[at:at + RESULT_WIDTH] for at in range(0, len(text), RESULT_WIDTH)]
    for index, chunk in enumerate(chunks):
        print(f"{RESULT_LINE} {index} {chunk}", flush=True)
    print(f"{RESULT_END} {len(chunks)} {sha256_of(text)}", flush=True)


def checked(environ, layout, record):
    """Places the payload and makes every check before any judge, into `record`; returns the
    listing and the reason the sandbox refuses to start a judge, or None."""
    files = payload_of(environ)
    place(files, layout)
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
    refusal = mount_refusal(record["mounts"], layout) or listing_refusal(listing, files, layout)
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
    omp_output = None
    if record["refused"] is None:
        record["manifests"] = {"before": manifests_now(record.get("bundleDirectory"))}
        if manifest_problem(record["manifests"], finished=False) is None:
            returncode, stdout, stderr = run_omp(environ)
            record["omp"] = {"returncode": returncode}
            omp_output = (stdout, stderr)
            record["manifests"]["after"] = manifests_now(record.get("bundleDirectory"))
        record["manifest"] = manifest_problem(record["manifests"])
    print_result(collected(record, listing, omp_output))
    return 0


if __name__ == "__main__":
    sys.exit(main())
