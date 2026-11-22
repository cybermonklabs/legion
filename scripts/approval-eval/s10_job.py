"""The sandbox of the claim's judging, as a Kubernetes Job: one a request, in the driver's own
namespace, under the ServiceAccount `--service-account` names, labelled so a NetworkPolicy can
select it, and run on the image the driver's own pod runs.

`job_spec` builds the Job from the driver's own pod: its security contexts, resources,
tolerations, node selector and runtime class, and three of its mounts beside one `emptyDir` (the
gateway key's one Secret item, and `models.yml` and the overlay from their ConfigMap). No service
account token is mounted, no variable comes from a Secret, and no service link is set. The program
(`s10_sandbox.py`), the bundles and the calls travel in environment variables, so the program is no
file of the sandbox; the result comes back on the pod's log (`result_of`), and `KubeRunner` deletes
the Job once it has read it. Before a run's first request, `KubeRunner.preflight` runs the same
Job with no bundle: every check of the sandbox, and no judge.

A failed call of the Kubernetes API is tried again (`RETRY_PAUSES`); one that still fails is no
fault of the request's, and stops the run (`KubeUnavailable`), which writes nothing more for it.
A Job that ran and gave no readable result is that request's own (`SandboxRun.failure`).
"""

import sys

sys.dont_write_bytecode = True

import base64  # noqa: E402
import copy  # noqa: E402
import json  # noqa: E402
import lzma  # noqa: E402
import os  # noqa: E402
import socket  # noqa: E402
import subprocess  # noqa: E402
import time  # noqa: E402
from dataclasses import dataclass, field  # noqa: E402

import s10_sandbox as SANDBOX  # noqa: E402
from s10_store import CannotRun, sha256_of  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))

# The label every sandbox pod carries, which the sandbox's NetworkPolicy selects.
LABEL = {"app.kubernetes.io/name": "s10-judge-sandbox"}
# The ServiceAccount a sandbox runs as unless `--service-account` names another: one of its own,
# bound to no cloud identity and no role. The driver's own pod's account is refused.
SERVICE_ACCOUNT = "s10-judge-sandbox"
# The pauses, in seconds, before each new try of a failed Kubernetes API call: a transient failure
# passes, and one that lasts past them stops the run.
RETRY_PAUSES = (5, 15, 45, 90)
# How long the preflight's Job may run, in seconds: its checks, and no judge.
PREFLIGHT_DEADLINE = 900
# Where Kubernetes writes the namespace of the driver's own pod, in which each Job runs.
NAMESPACE_FILE = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

BOOT = ("import base64, lzma, os; exec(compile(lzma.decompress(base64.b64decode(os.environ['S10_SANDBOX'])), "
        "'s10_sandbox.py', 'exec'))")

# The variables the profile needs, copied from this pod's own environment.
PROFILE_VARIABLES = ("OMP_PROFILE", "PI_PROFILE", "PI_CODING_AGENT_DIR", "PI_CONFIG_FILES", "HOME")
# The largest pod spec the Job may carry, in bytes of its variables' values.
PAYLOAD_LIMIT = 900_000
# Each variable that carries part of the payload, in characters: well under Kubernetes' limit.
PAYLOAD_WIDTH = 100_000


class KubeUnavailable(Exception):
    """A Kubernetes API call failed past its retries: no fault of the request's. The run stops and
    writes nothing more, and the same command resumes the stage."""


@dataclass
class SandboxRun:
    """What one Job gave back: its name, the files of its result, and the facts of its pod;
    `failure` where the Job ran and gave no readable result."""

    job: str
    files: dict = field(default_factory=dict)
    facts: dict = field(default_factory=dict)
    failure: str = None


def payload_variables(files):
    """The bundles and the calls, as the environment variables the Job carries them in."""
    text = SANDBOX.pack({path.lstrip("/"): data for path, data in files.items()})
    variables = {f"{SANDBOX.PAYLOAD_VARIABLE}{index}": text[at:at + PAYLOAD_WIDTH]
                 for index, at in enumerate(range(0, len(text), PAYLOAD_WIDTH))}
    variables[SANDBOX.PAYLOAD_SHA256_VARIABLE] = SANDBOX.sha256_of(text)
    return variables


def program_variable():
    """The sandbox's program, as the variable the Job runs it from."""
    with open(os.path.join(HERE, "s10_sandbox.py"), "rb") as handle:
        source = handle.read()
    return base64.b64encode(lzma.compress(source)).decode("ascii")


def job_spec(name, pod, image, account, variables, environ, deadline=SANDBOX.OMP_TIMEOUT + 900):
    """The sandbox Job of one request, from this pod's own spec (see the module's docstring)."""
    spec = pod["spec"]
    container = next((one for one in spec["containers"]
                      if any(mount.get("mountPath") == SANDBOX.KEY_MOUNT for mount in one.get("volumeMounts", []))), None)
    if container is None:
        raise CannotRun(f"this pod mounts no gateway key at {SANDBOX.KEY_MOUNT} for the sandbox to copy")
    volumes = {volume["name"]: volume for volume in spec.get("volumes", [])}
    mounts = {mount["mountPath"]: mount for mount in container.get("volumeMounts", [])}
    key_volume = volumes.get(mounts[SANDBOX.KEY_MOUNT]["name"], {})
    secret = key_volume.get("secret") or {}
    items = secret.get("items") or []
    if len(items) != 1 or items[0].get("path") != "key" or mounts[SANDBOX.KEY_MOUNT].get("subPath"):
        raise CannotRun(f"this pod's {SANDBOX.KEY_MOUNT} is not one item of one Secret, named key")
    route_items = []
    route_name = None
    for path in (SANDBOX.MODELS_FILE, SANDBOX.OVERLAY_FILE):
        mount = mounts.get(path)
        config_map = (volumes.get((mount or {}).get("name"), {}) or {}).get("configMap")
        if mount is None or config_map is None or not mount.get("subPath"):
            raise CannotRun(f"this pod mounts no {path} from a ConfigMap for the sandbox to copy")
        if route_name not in (None, config_map["name"]):
            raise CannotRun("this pod mounts models.yml and the overlay from two ConfigMaps")
        route_name = config_map["name"]
        item = next((one for one in config_map.get("items") or [] if one.get("path") == mount["subPath"]),
                    {"key": mount["subPath"], "path": mount["subPath"]})
        route_items.append({"key": item["key"], "path": item["path"]})
    own_env = {one["name"]: one.get("value") for one in container.get("env", []) if "valueFrom" not in one}
    env = [{"name": one, "value": own_env.get(one) or environ.get(one, "")} for one in PROFILE_VARIABLES]
    env.append({"name": "PYTHONDONTWRITEBYTECODE", "value": "1"})
    env.extend({"name": one, "value": value} for one, value in sorted(variables.items()))
    if sum(len(one["value"]) for one in env) > PAYLOAD_LIMIT:
        raise ValueError("the request's bundles do not fit in the sandbox Job's variables")
    pod_spec = {
        "serviceAccountName": account,
        "automountServiceAccountToken": False,
        "enableServiceLinks": False,
        "restartPolicy": "Never",
        "securityContext": spec.get("securityContext") or {},
        "containers": [{
            "name": "sandbox",
            "image": image,
            "imagePullPolicy": "IfNotPresent",
            "command": ["python3", "-c", BOOT],
            "workingDir": SANDBOX.WORK_MOUNT,
            "env": env,
            "securityContext": container.get("securityContext") or {},
            "resources": container.get("resources") or {},
            "volumeMounts": [
                {"name": "gateway-key", "mountPath": SANDBOX.KEY_MOUNT, "readOnly": True},
                {"name": "route", "mountPath": SANDBOX.MODELS_FILE, "subPath": route_items[0]["path"], "readOnly": True},
                {"name": "route", "mountPath": SANDBOX.OVERLAY_FILE, "subPath": route_items[1]["path"], "readOnly": True},
                {"name": "work", "mountPath": SANDBOX.WORK_MOUNT},
            ],
        }],
        "volumes": [
            {"name": "gateway-key", "secret": {"secretName": secret["secretName"], "items": [dict(items[0])],
                                               **({"defaultMode": secret["defaultMode"]} if "defaultMode" in secret else {})}},
            {"name": "route", "configMap": {"name": route_name, "items": route_items}},
            {"name": "work", "emptyDir": {}},
        ],
    }
    for copied in ("tolerations", "nodeSelector", "runtimeClassName"):
        if spec.get(copied):
            pod_spec[copied] = spec[copied]
    return {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {"name": name, "namespace": pod["metadata"]["namespace"], "labels": dict(LABEL)},
        "spec": {"backoffLimit": 0, "activeDeadlineSeconds": deadline,
                 "ttlSecondsAfterFinished": 86400,
                 "template": {"metadata": {"labels": dict(LABEL)}, "spec": pod_spec}},
    }


def redacted(spec):
    """A Job spec as it is kept: each variable the driver sent its payload in, by its sha256."""
    kept = copy.deepcopy(spec)
    for container in kept["spec"]["template"]["spec"]["containers"]:
        for one in container["env"]:
            if one["name"].startswith("S10_"):
                one["value"] = "sha256:" + sha256_of(one["value"])
    return kept


def result_of(log):
    """The result the sandbox printed on its log, checked by its count and sha256."""
    chunks = {}
    end = None
    for line in log.splitlines():
        parts = line.split(" ")
        if len(parts) == 3 and parts[0] == SANDBOX.RESULT_LINE:
            chunks[int(parts[1])] = parts[2]
        elif len(parts) == 3 and parts[0] == SANDBOX.RESULT_END:
            end = (int(parts[1]), parts[2])
    if end is None or sorted(chunks) != list(range(end[0])):
        raise ValueError("the sandbox's log holds no whole result")
    text = "".join(chunks[index] for index in range(end[0]))
    if SANDBOX.sha256_of(text) != end[1]:
        raise ValueError("the sandbox's result is not the one its sha256 names")
    return SANDBOX.unpack(text)


def image_refusal(pod, image):
    """Why `image` is not the image this driver's own pod runs, or None: the container that holds
    the gateway key names it in its spec, by the same name and digest, and its status reports that
    digest. So the sandbox's image is the session image the claim's run digest names by its digest
    (`--image`), never one an operator built for the run."""
    for container in pod["spec"]["containers"]:
        if not any(mount.get("mountPath") == SANDBOX.KEY_MOUNT for mount in container.get("volumeMounts", [])):
            continue
        status = next((one for one in (pod.get("status") or {}).get("containerStatuses") or []
                       if one.get("name") == container["name"]), {})
        digest = image.split("@", 1)[-1]
        if container.get("image") == image and str(status.get("imageID") or "").endswith("@" + digest):
            return None
        return (f"--image is {image}, and this driver's own pod runs {container.get('image')} "
                f"({status.get('imageID')}): the sandbox runs only the image its driver runs")
    return f"this pod mounts no gateway key at {SANDBOX.KEY_MOUNT}, so no image of its own can be the sandbox's"


def account_refusal(pod, account):
    """Why the sandbox may not run as `account`, or None: the driver's own pod's account holds what
    the driver may do in the cluster, and no judge may hold it."""
    own = (pod.get("spec") or {}).get("serviceAccountName") or "default"
    if account == own:
        return (f"--service-account is {account}, the account this driver's own pod runs as: the sandbox runs "
                "under an account of its own")
    return None


class KubeRunner:
    """Runs one request's sandbox as a Job in this pod's namespace, reads its log, deletes it; and,
    once a run, the same Job with no bundle (`preflight`). It refuses an image its own pod does not
    run and its own pod's account, and holds the gateway key's bytes, which the driver keeps out of
    every file it keeps. Each Kubernetes API call is tried again after each of `pauses`; one that
    still fails raises `KubeUnavailable`."""

    def __init__(self, image, account, environ, poll=15, pauses=RETRY_PAUSES):
        self.image = image
        self.account = account
        self.environ = environ
        self.poll = poll
        self.pauses = pauses
        self.program = program_variable()
        self._pod = None
        refusal = image_refusal(self.own_pod(), image) or account_refusal(self.own_pod(), account)
        if refusal is not None:
            raise CannotRun(refusal)
        self.key = SANDBOX.gateway_key()

    def invoke(self, arguments, stdin):
        """One `kubectl` call in this pod's namespace, as it completed."""
        return subprocess.run(["kubectl", "-n", self.namespace, *arguments], input=stdin, capture_output=True,
                              text=True)

    def kubectl(self, *arguments, stdin=None, check=True):
        """A Kubernetes API call's output, tried again after each pause while it fails. A `create`
        that a try before it may have made answers `AlreadyExists`, which is its own Job. Past the
        pauses it raises `KubeUnavailable`, or returns what it got where `check` is false."""
        failures = []
        for attempt, pause in enumerate((0, *self.pauses)):
            if pause:
                time.sleep(pause)
            completed = self.invoke(arguments, stdin)
            if completed.returncode == 0:
                return completed.stdout
            if attempt and arguments[0] == "create" and "AlreadyExists" in completed.stderr:
                return completed.stdout
            failures.append(f"exited {completed.returncode}: {completed.stderr.strip()[:300]}")
        if not check:
            return ""
        raise KubeUnavailable(f"kubectl {arguments[0]} failed {len(failures)} times, last {failures[-1]}")

    def kubectl_json(self, *arguments):
        """A Kubernetes API answer, as JSON; one that is no JSON is a failure of the API."""
        try:
            return json.loads(self.kubectl(*arguments))
        except ValueError as error:
            raise KubeUnavailable(f"kubectl {arguments[0]} gave no JSON: {error}") from error

    def own_pod(self):
        if self._pod is None:
            with open(NAMESPACE_FILE) as handle:
                self.namespace = handle.read().strip()
            self._pod = self.kubectl_json("get", "pod", socket.gethostname(), "-o", "json")
        return self._pod

    def run(self, ask_id, files, extra_env=None):
        """One request's sandbox, with its bundles and calls."""
        return self.run_job("s10-judge-" + sha256_of(f"{ask_id} {time.time()} {os.getpid()}")[:16], files, "request",
                            extra_env)

    def preflight(self):
        """The sandbox with no bundle and no call, which makes every check and starts no judge."""
        return self.run_job("s10-judge-preflight-" + sha256_of(f"{time.time()} {os.getpid()}")[:16], {}, "preflight",
                            deadline=PREFLIGHT_DEADLINE)

    def run_job(self, name, files, mode, extra_env=None, deadline=SANDBOX.OMP_TIMEOUT + 900):
        """One Job in `mode` (`SANDBOX.MODES`): created, waited for, its one pod's log read,
        deleted. `KubeUnavailable` where the API fails; `failure` where the Job ran and gave no
        readable result."""
        pod = self.own_pod()
        variables = {SANDBOX.PROGRAM_VARIABLE: self.program, SANDBOX.MODE_VARIABLE: mode, **payload_variables(files),
                     **(extra_env or {})}
        run = SandboxRun(job=name)
        try:
            try:
                spec = job_spec(name, pod, self.image, self.account, variables, self.environ, deadline)
            except ValueError as error:
                run.failure = str(error)
                return run
            run.facts["spec"] = redacted(spec)
            self.kubectl("create", "-f", "-", stdin=json.dumps(spec))
            deadline = time.time() + spec["spec"]["activeDeadlineSeconds"] + 600
            while True:
                status = self.kubectl_json("get", "job", name, "-o", "json").get("status") or {}
                if status.get("succeeded") or status.get("failed"):
                    break
                if time.time() > deadline:
                    run.failure = "the Job did not finish by its deadline"
                    return run
                time.sleep(self.poll)
            pods = self.kubectl_json("get", "pods", "-l", f"job-name={name}", "-o", "json").get("items") or []
            if len(pods) != 1:
                run.failure = f"the Job ran {len(pods)} pods"
                return run
            statuses = pods[0].get("status", {}).get("containerStatuses") or [{}]
            terminated = (statuses[0].get("state") or {}).get("terminated") or {}
            run.facts.update({"pod": pods[0]["metadata"]["name"], "imageId": statuses[0].get("imageID"),
                              "phase": pods[0].get("status", {}).get("phase"), "exitCode": terminated.get("exitCode")})
            log = self.kubectl("logs", pods[0]["metadata"]["name"], "-c", "sandbox")
            try:
                run.files = result_of(log)
            except (ValueError, OSError, KeyError) as error:
                run.failure = str(error)
            return run
        finally:
            self.kubectl("delete", "job", name, "--cascade=background", "--wait=false", "--ignore-not-found",
                         check=False)
