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
   files, by the digest the Job pulls it by (the driver checks the digest the pod ran), which is
   every file of the root file system when the listing is taken, since nothing but this program
   has run in the container yet; the kernel's and the kubelet's own mounts (`/proc`, `/dev`,
   `/sys`, `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`); the three mounts of the gateway
   key, `models.yml` and the overlay; and the `emptyDir`.
3. It measures point 2's runtime by content (`measure_runtime`) and refuses on any difference.
4. It records whether the metadata server refuses a request that does not carry its header, and
   refuses to go on if it answers one.

Then it runs one `omp -p` in `WORK`, its transcripts kept as files, asked to make one `eval` call
whose code is `CELL`: the five recorded `agent()` calls and one `wait()` for them, as S10's round
waits. omp 18.16.0 has no command that runs an eval cell without a model, so the session's own
model makes that one call; the driver holds its transcript to that one call.

What comes back is written to the pod's log only (`print_result`): the record, the listing, every
transcript file, the answers and omp's own output, as one xz-compressed tar in base64 lines that
the driver reads with `kubectl logs` and checks by its sha256. This program decides nothing about
an answer: the driver does, from the transcripts, before any answer is compared.
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

# The mounts the kernel and the kubelet make in every container, which hold no file of a run.
SYSTEM_TREES = ("/proc", "/dev", "/sys")
KUBELET_FILES = ("/etc/hosts", "/etc/hostname", "/etc/resolv.conf")

# The link-local metadata server, asked without the header it requires of every request.
METADATA_URL = "http://169.254.169.254/computeMetadata/v1/"

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


def listing_refusal(lines, payload, layout):
    """Why the listing holds a credential or a file outside the bundles, the calls and the runtime.

    `payload` names the files the driver sent, by path. Under the `emptyDir` only those, and the
    directories that hold them, may be there; under the gateway key's mount only the key, by
    Kubernetes' own `..data` links to it. Every other file is the image's own or a mount's."""
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
        if under(path, layout["work"]):
            if path in payload and line.startswith("f "):
                placed.add(path)
            elif not (path in wanted_directories and line.startswith("d ")):
                return f"{path} is a file outside the bundles and the calls"
        elif under(path, layout["key"]) and path != layout["key"]:
            name = os.path.basename(path)
            if name != "key" and not name.startswith(".."):
                return f"{path} is no part of the gateway key's one item"
    missing = sorted(set(payload) - placed)
    if missing:
        return f"{missing[0]} was sent and is not there"
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
    """What the metadata server answers a request without its header: refused or answered."""
    try:
        with urllib.request.urlopen(METADATA_URL, timeout=10) as response:
            return {"refused": False, "status": response.status}
    except urllib.error.HTTPError as error:
        return {"refused": True, "status": error.code}
    except OSError as error:
        return {"refused": True, "status": None, "error": type(error).__name__}


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
    """The one `omp -p` the judges run under, with its transcripts as files in `SESSIONS`: its exit
    status (None where it ran out of time), its standard output and its standard error."""
    env = {name: value for name, value in environ.items() if not name.startswith("S10_")}
    env["OMP_SESSION_STORAGE"] = "file"
    try:
        completed = subprocess.run([OMP, "-p", "--session-dir", SESSIONS, PROMPT], cwd=WORK, env=env,
                                   capture_output=True, timeout=OMP_TIMEOUT)
    except subprocess.TimeoutExpired as expired:
        return None, expired.stdout or b"", expired.stderr or b""
    return completed.returncode, completed.stdout, completed.stderr


def collected(record, listing, omp_output):
    """The files the result carries: the record, the listing, omp's output, every transcript, the
    answers."""
    files = {"record.json": (json.dumps(record, indent=2, sort_keys=True) + "\n").encode("utf-8"),
             "listing.txt": ("\n".join(listing) + "\n").encode("utf-8")}
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
    if not record["metadata"]["refused"]:
        return listing, "the metadata server answered a request that carried no header"
    return listing, None


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
        returncode, stdout, stderr = run_omp(environ)
        record["omp"] = {"returncode": returncode}
        omp_output = (stdout, stderr)
    print_result(collected(record, listing, omp_output))
    return 0


if __name__ == "__main__":
    sys.exit(main())
