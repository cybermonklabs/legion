"""Tests of `pg.run`: the password a DSN carries reaches psql in `PGPASSWORD` alone, in no argument.

Run from this directory with `python3 -m unittest pg_test`. A stand-in psql first on PATH records
each call's arguments and the `PGPASSWORD` it was given, one JSON object per call, so the rows read
what psql was handed rather than what a server answered.
"""

import sys

sys.dont_write_bytecode = True

import json  # noqa: E402
import os  # noqa: E402
import pathlib  # noqa: E402
import subprocess  # noqa: E402
import tempfile  # noqa: E402
import unittest  # noqa: E402
from unittest import mock  # noqa: E402

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import pg  # noqa: E402

STAND_IN = """#!/usr/bin/env python3
import json, os, sys
with open(os.environ["PG_TEST_CALLS"], "a") as calls:
    calls.write(json.dumps({"argv": sys.argv[1:], "PGPASSWORD": os.environ.get("PGPASSWORD")}) + "\\n")
"""


class RunTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = pathlib.Path(directory.name)
        psql = root / "psql"
        psql.write_text(STAND_IN)
        psql.chmod(0o755)
        self.calls = root / "calls"
        environment = mock.patch.dict(os.environ, {
            "PATH": f"{root}{os.pathsep}{os.environ['PATH']}",
            "PG_TEST_CALLS": str(self.calls),
            "PGPASSWORD": "the-callers-own-not-a-secret",
        })
        environment.start()
        self.addCleanup(environment.stop)

    def call(self, dsn):
        pg.run(dsn, "-c", "select 1")
        (call,) = [json.loads(line) for line in self.calls.read_text().splitlines()]
        return call

    def test_an_encoded_password_is_in_PGPASSWORD_and_in_no_argument(self):
        call = self.call("postgres://tester:not-a-secret%40can%2Fary@127.0.0.1:5999/postgres?sslmode=disable")
        self.assertEqual(call["argv"], ["postgres://tester@127.0.0.1:5999/postgres?sslmode=disable",
                                        "-v", "ON_ERROR_STOP=1", "-c", "select 1"])
        self.assertEqual(call["PGPASSWORD"], "not-a-secret@can/ary")
        for argument in call["argv"]:
            self.assertNotIn("not-a-secret", argument)

    def test_a_query_string_password_is_taken_out_with_its_ampersand(self):
        call = self.call("postgres://tester@127.0.0.1:5999/postgres?sslmode=disable&pass%77ord=not-a-secret%26x&application_name=a%3Db")
        self.assertEqual(call["argv"][0], "postgres://tester@127.0.0.1:5999/postgres?sslmode=disable&application_name=a%3Db")
        self.assertEqual(call["PGPASSWORD"], "not-a-secret&x")
        for argument in call["argv"]:
            self.assertNotIn("not-a-secret", argument)

    def test_a_DSN_with_no_password_is_passed_whole_and_keeps_the_callers_PGPASSWORD(self):
        for dsn in ("postgres://tester@127.0.0.1:5999/postgres?sslmode=disable",
                    "postgres://127.0.0.1:5999/postgres"):
            self.calls.unlink(missing_ok=True)
            call = self.call(dsn)
            self.assertEqual(call["argv"][0], dsn)
            self.assertEqual(call["PGPASSWORD"], "the-callers-own-not-a-secret")

    def test_a_password_libpq_cannot_decode_is_refused_before_psql_runs(self):
        with self.assertRaises(SystemExit) as refused:
            pg.run("postgres://tester:not-a-secret%zz@127.0.0.1:5999/postgres", "-c", "select 1")
        self.assertEqual(str(refused.exception), "pg: refused a Postgres URL: its password holds a % "
                         "that starts no %XX escape, or %00 (the URL is not printed)")
        self.assertFalse(self.calls.exists())


# The corpus the three splits share: packages/dispatch/e2e/__tests__/psql-password.test.ts and
# packages/envoy/scripts/dev-broker.test.sh read it too, so the three are held to one table, each
# case of which was checked against libpq's own PQconninfoParse.
CORPUS = pathlib.Path(__file__).resolve().parents[2] / "packages/envoy/scripts/fixtures/pg-url.json"


class CorpusTest(unittest.TestCase):
    def setUp(self):
        self.corpus = json.loads(CORPUS.read_text())

    def test_the_corpus_has_accepted_and_refused_cases(self):
        self.assertTrue(self.corpus["accepted"])
        self.assertTrue(self.corpus["refused"])

    def test_each_accepted_case_splits_as_libpq_does(self):
        for case in self.corpus["accepted"]:
            with self.subTest(url=case["bare"]):
                self.assertEqual(pg.split_dsn(case["url"]), (case["bare"], case["password"]))

    def test_each_refused_case_is_refused_naming_its_fault(self):
        for case in self.corpus["refused"]:
            with self.subTest(fault=case["fault"]):
                with self.assertRaises(SystemExit) as refused:
                    pg.split_dsn(case["url"])
                self.assertEqual(str(refused.exception),
                                 f"pg: refused a Postgres URL: {case['fault']} (the URL is not printed)")


class RunSearchTest(unittest.TestCase):
    """run_search.py takes the fixture's database URL from FIXTURE_DATABASE_URL, never from its
    own command line, and its psql gets the URL without its password."""

    setUp = RunTest.setUp

    def run_search(self, *arguments, fixture_url=None):
        directory = pathlib.Path(self.calls).parent
        cases = directory / "cases.json"
        cases.write_text(json.dumps([{"case_id": "ask-1", "kind": 1, "sha256": "",
                                      "source": {"kind": "ask", "ask_id": "00000000-0000-0000-0000-000000000001",
                                                 "offset": 0, "length": 1}}]))
        environment = {key: value for key, value in os.environ.items() if key != "FIXTURE_DATABASE_URL"}
        if fixture_url is not None:
            environment["FIXTURE_DATABASE_URL"] = fixture_url
        return subprocess.run(
            [sys.executable, "-B", str(pathlib.Path(__file__).resolve().parent / "run_search.py"),
             "--cases", str(cases), "--url", "http://127.0.0.1:9", "--token", "fixture-token",
             "--out", str(directory / "out.jsonl"), *arguments],
            env=environment, capture_output=True, text=True, timeout=60)

    def test_the_fixture_url_comes_from_FIXTURE_DATABASE_URL_and_its_password_reaches_psql_alone(self):
        url = "postgres://tester:not-a-secret%40fix@127.0.0.1:5999/searchfix?sslmode=disable"
        result = self.run_search(fixture_url=url)
        self.assertEqual(result.returncode, 0, result.stderr)
        (call,) = [json.loads(line) for line in self.calls.read_text().splitlines()]
        self.assertEqual(call["argv"][0], "postgres://tester@127.0.0.1:5999/searchfix?sslmode=disable")
        self.assertEqual(call["PGPASSWORD"], "not-a-secret@fix")
        for argument in call["argv"]:
            self.assertNotIn("not-a-secret", argument)

    def test_a_run_with_no_FIXTURE_DATABASE_URL_is_refused_naming_it_and_the_option_is_gone(self):
        missing = self.run_search()
        self.assertEqual(missing.returncode, 2)
        self.assertIn("FIXTURE_DATABASE_URL must name the fixture's database", missing.stderr)
        option = self.run_search("--fixture-dsn", "postgres://h/searchfix",
                                 fixture_url="postgres://h/searchfix")
        self.assertEqual(option.returncode, 2)
        self.assertIn("unrecognized arguments: --fixture-dsn", option.stderr)
        self.assertFalse(self.calls.exists())


if __name__ == "__main__":
    unittest.main()
