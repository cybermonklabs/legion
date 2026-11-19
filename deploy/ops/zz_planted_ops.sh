#!/usr/bin/env bash
# A planted operator script: the pull requests of one issue, by the stored column.
psql "$DATABASE_URL" -Atc "select repo, number from delivery_pull_requests where issue_key = '$1'"
