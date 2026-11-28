#!/usr/bin/env python3
"""The corpus files the search-measure scripts write and read back, and the line they say.

A corpus directory holds one JSON file (`projects.json`, `coverage.json`) or one JSONL file
(`issues.jsonl` and the rest) per kind: `harvest.py` writes them, `load_fixture.py` loads them
into a database and `build_cases.py` reads them beside the cases. Both halves live here so the
two never disagree about what a corpus file is.

A blank line is no row: these readers raise `json.JSONDecodeError` on one rather than skip it,
because a corpus file with a hole in it is a harvest to run again, not a measurement to make.
"""

import json
import os
import sys


def read_json(corpus, name):
    with open(os.path.join(corpus, name)) as handle:
        return json.load(handle)


def read_jsonl(corpus, name):
    return list(JsonlRows(corpus, name))


class JsonlRows:
    """The rows of one JSONL corpus file, parsed a line at a time each time they are iterated.

    `read_jsonl` holds a whole file, and a deployment's `documents.jsonl` is every document's
    markdown: more than a gigabyte, which parsed whole is more than the memory of the pod that
    loads it. A reader of such a file takes this instead, and may go through it as often as it
    needs, because each pass opens the file again and holds one row of it at a time.
    """

    def __init__(self, corpus, name):
        self.path = os.path.join(corpus, name)

    def __iter__(self):
        with open(self.path) as handle:
            for line in handle:
                yield json.loads(line)


def write_json(out, name, value):
    with open(os.path.join(out, name), "w") as handle:
        json.dump(value, handle)


def write_jsonl(out, name, rows):
    with JsonlWriter(out, name) as writer:
        writer.writerows(rows)


class JsonlWriter:
    """A JSONL corpus file written a row at a time, counting the rows it is given.

    A harvest writes each row as its answer comes, rather than every row once the last answer is
    in, so it holds no more of a corpus than the answers it is reading; `count` is what its
    summary line says.
    """

    def __init__(self, out, name):
        self.handle = open(os.path.join(out, name), "w")
        self.count = 0

    def __enter__(self):
        return self

    def __exit__(self, *raised):
        self.handle.close()

    def writerow(self, row):
        self.handle.write(json.dumps(row) + "\n")
        self.count += 1

    def writerows(self, rows):
        for row in rows:
            self.writerow(row)


def say(line):
    """One line of progress, on stderr, so a run's own output stays what it answered."""
    print(line, file=sys.stderr, flush=True)
