"""The claim's judging stage on disk (`s10_judge.py`'s `--out`): its record and every file it names.

The stage record, `s10-judged.json`, chains the stage to the inputs it judged and names the sha256
of every file written under `--out`. A file is committed by writing it beside its place, writing
the record that names it, then moving it into its place, so a resume never takes a half-written
file for written. A run into an `--out` that holds another run's record, or files of none, is
refused; a run into its own resumes it, taking only the files its record names, with the bytes it
names. `s10-judged-io.ts` reads these files back for the analysis.
"""

import sys

sys.dont_write_bytecode = True

import datetime  # noqa: E402
import hashlib  # noqa: E402
import json  # noqa: E402
import os  # noqa: E402
import shutil  # noqa: E402

STAGE_FILE = "s10-judged.json"
OUTCOMES_FILE = "outcomes.jsonl"
STAGE_VERSION = 4
CODE_DIRECTORY = "code"
REQUESTS_DIRECTORY = "requests"
OUTCOME_FILE = "outcome.json"
# Where the frozen `run_judges` writes a request's bundles, through the link `WORK/bundles`, before
# they are kept under `requests/<ask id>/`: so every byte it writes lands under `--out`.
BUILDING_DIRECTORY = "building"
# The file that marks a request started: committed before S10's code runs on it; and the reason a
# request started and not finished is a `started` deviation, which `s10-judged-io.ts` gives too.
STARTED_FILE = "started.json"
STARTED_REASON = "a run started the request and stopped before its outcome; it is never judged again"
# Where an outcome the analysis's parser refuses is kept, beside the `shape` deviation in its place.
UNREAD_FILE = "unread-outcome.json"
# Where an outcome built whole is kept when its read-back did not run: the next run reads that
# outcome back and commits it, and judges nothing again (point 6).
PENDING_FILE = "pending-outcome.json"
# Where the preflight before each request keeps its sandbox's result, `preflights/<n>/`, before that
# request starts.
PREFLIGHT_DIRECTORY = "preflights"


class CannotRun(Exception):
    """The command cannot run as the registered text fixes it, so it stops before any judgement."""


def sha256_of(data):
    """The sha256 of some bytes, or of a text's UTF-8 bytes."""
    if isinstance(data, str):
        data = data.encode("utf-8")
    return hashlib.sha256(data).hexdigest()


def utc_now():
    """The time now, as the records here write a time."""
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")


def json_bytes(value):
    """A record as a file holds it."""
    return (json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n").encode("utf-8")


def under(path, tree):
    """Whether `path` is `tree` or inside it."""
    return path == tree or path.startswith(tree.rstrip("/") + "/")


def request_path(ask_id, name):
    """Where one request's file is kept, under the stage's directory."""
    return f"{REQUESTS_DIRECTORY}/{ask_id}/{name}"


class Stage:
    """The stage record and every file it names, under `--out`."""

    def __init__(self, directory, record):
        self.directory = directory
        self.record = record

    @classmethod
    def open(cls, directory, chain):
        """Takes `directory` for this run: an empty one is started with a record naming `chain`;
        one holding this chain's record is resumed, held to its record; anything else is refused."""
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, STAGE_FILE)
        if not os.path.exists(path):
            if os.listdir(directory):
                raise CannotRun(f"{directory} holds files and no {STAGE_FILE}, so no run here can show they are its own")
            stage = cls(directory, {"stage": "s10-judged", "version": STAGE_VERSION, "state": "started", **chain,
                                    "startedAt": utc_now(), "finishedAt": None, "stops": [], "counts": None,
                                    "files": {}})
            stage.save()
            return stage
        with open(path, "rb") as handle:
            record = json.loads(handle.read())
        if record.get("stage") != "s10-judged" or record.get("version") != STAGE_VERSION:
            raise CannotRun(f"{path} is no record of version {STAGE_VERSION} of the judged stage")
        for name in chain:
            if record.get(name) != chain[name]:
                raise CannotRun(f"{directory} holds another run's judging: its {name} is not this run's. "
                                "A resume takes only its own claim run's directory, over the same inputs and code")
        stage = cls(directory, record)
        stage.hold_to_record()
        return stage

    def hold_to_record(self):
        """Every file there must be one the record names, with its bytes, and every one there. What
        `building/` holds is bundles the frozen code was writing when a run stopped, which no
        record took: they are removed unread, as a partial file is, and their request is one a run
        started (`started.json`), which is never judged again."""
        shutil.rmtree(os.path.join(self.directory, BUILDING_DIRECTORY), ignore_errors=True)
        files = self.record["files"]
        for root, _, names in os.walk(self.directory):
            for name in names:
                path = os.path.join(root, name)
                relative = os.path.relpath(path, self.directory)
                if relative == STAGE_FILE or relative.startswith(STAGE_FILE + "."):
                    if relative != STAGE_FILE:
                        os.remove(path)
                    continue
                if relative.endswith(".partial"):
                    whole = relative[:-len(".partial")]
                    with open(path, "rb") as handle:
                        data = handle.read()
                    if files.get(whole) == sha256_of(data):
                        os.replace(path, os.path.join(self.directory, whole))
                    else:
                        os.remove(path)
        for root, _, names in os.walk(self.directory):
            for name in names:
                relative = os.path.relpath(os.path.join(root, name), self.directory)
                if relative != STAGE_FILE and relative not in files:
                    raise CannotRun(f"{os.path.join(self.directory, relative)} is a file the stage record does not name, "
                                    "so no run here can show it is its own")
        for relative, wanted in files.items():
            path = os.path.join(self.directory, relative)
            if not os.path.exists(path):
                raise CannotRun(f"the stage record names {path}, which is not there")
            with open(path, "rb") as handle:
                if sha256_of(handle.read()) != wanted:
                    raise CannotRun(f"{path} is not the file the stage record names")

    def save(self):
        """Writes the record as it stands, whole."""
        path = os.path.join(self.directory, STAGE_FILE)
        partial = f"{path}.{os.getpid()}.partial"
        with open(partial, "wb") as handle:
            handle.write(json_bytes(self.record))
        os.replace(partial, path)

    def has(self, relative):
        return relative in self.record["files"]

    def read(self, relative):
        with open(os.path.join(self.directory, relative), "rb") as handle:
            return handle.read()

    def commit(self, files):
        """Writes files beside their places, the record naming them, then each into its place."""
        for relative in files:
            if os.path.isabs(relative) or ".." in relative.split("/") or self.has(relative):
                raise CannotRun(f"{relative} is no new file of this stage")
        for relative, data in files.items():
            path = os.path.join(self.directory, relative)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path + ".partial", "wb") as handle:
                handle.write(data)
            self.record["files"][relative] = sha256_of(data)
        self.save()
        for relative in files:
            path = os.path.join(self.directory, relative)
            os.replace(path + ".partial", path)
