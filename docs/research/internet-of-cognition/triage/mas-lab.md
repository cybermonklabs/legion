# Triage: ioc-project/mas-lab

Repository: github.com/ioc-project/mas-lab.

Clone commit: 477441ddb82d1b07c76c36f1426d8a5be0b6194b, of 2026-11-09.

Clone path in this pod: $IOC_CLONES/mas-lab, where IOC_CLONES is /legion/scratch/tmp/ioc-clones.

This is a triage.
It reads names, counts, licences and version strings.
It does not run their code [t-triage-depth].

## What the repository is

MAS Lab builds, runs and tests multi-agent systems from YAML files [t-ml-identity].

A system is a team of model agents with tools and a workflow [t-ml-identity].

The repository holds a runtime, a control plane, a benchmark harness, eight libraries and a dashboard [t-ml-language-mix] [t-ml-drop-runtime] [t-ml-drop-ctl] [t-ml-drop-lab] [t-ml-drop-libraries] [t-ml-drop-ui].

## Maturity

The clone is shallow, at depth one [t-ml-clone-shallow].

Therefore `git log` over it returns one commit [t-ml-log-local], and `git tag` over it returns nothing [t-ml-tag-local].

Maturity is read over the GitHub REST API instead.

The repository holds three tags: v0.2.0, v0.1.0 and a dataset tag [t-ml-tags-api].

It has two published releases, and the newest is v0.2.0 of 2026-09-30 [t-ml-releases].

One of the two releases is a dataset, not the software [t-ml-releases].

The default branch holds at least 201 commits [t-ml-commits].

The repository was made on 2026-06-29 and is not archived [t-ml-created].

It is the oldest repository of the five, and it is under five months old [t-all-age].

The public following is small: 64 stars [t-org-stars].

## Licence

The licence is the Apache License 2.0, read from the LICENSE file [t-ml-licence].

The source files carry the SPDX identifier Apache-2.0 [t-ml-licence].

## Size

The repository tracks 3307 files [t-ml-size-files].

The tracked files hold 511525 lines [t-ml-size-lines].

The language mix is 1856 Python files, 563 YAML files and 415 markdown files [t-ml-language-mix].

## Version range measured

Every one of the 15 Python packages declares version 0.2.0, and the VERSION file holds 0.2.0 [t-ml-version-measured].

The measured range is one version: 0.2.0 [t-ml-version-measured].

## What could cross into our runtime

Four classes cross unchanged.
mas-lab has material in all four.

Schema: 99 schema, registry and OpenAPI files under docs/schemas [t-ml-cross-schemas].

Specification document: 18 reference documents under docs/references [t-ml-cross-reference-docs].

Vocabulary: one ontology extension file in library-kg [t-ml-cross-kg-ontology].

Prompt-shaped skill: 11 SKILL.md files, but all 11 are tutorial, example or test material [t-ml-cross-skills].

## What is dropped, and why

runtime/, 419 files: a Python agent runtime.
It would be a rebuild in Go or TypeScript, not an import [t-ml-drop-runtime].

ctl/, 261 files: a Python control plane and command line tool.
It is a service beside Legion [t-ml-drop-ctl].

lab/, 770 files: benchmark, controller and core components in Python.
It is a service beside Legion, and it is the largest single part [t-ml-drop-lab].

The eight library-* directories, 877 files: Python packages.
Only the schema, ontology and skill files named above cross.
library-telemetry and library-kg also need Neo4j and a collector, which this issue does not start [t-ml-drop-libraries].

ui/ and demo-ui/, 247 files: a React dashboard.
It is a service beside Legion [t-ml-drop-ui].

labs/, 113 files: experiment configuration for their own paper.
It has no use in our runtime [t-ml-drop-labs].

docker/, deployments/, config/, compliance/, traces/, scripts/ and tests/, 191 files: build, deployment and test material for their stack [t-ml-drop-infra].

The rest of docs/, from 394 files in all: a documentation site, with mkdocs.yml, overrides/ and lychee.toml [t-ml-drop-docs-rest].

## One fact that limits every import

The repository holds no Go source file [t-ml-no-go].

No part of it can be linked into our Go runtime [t-all-no-go-ts-runtime].

The TypeScript that is present is dashboard code, not a client library [t-all-no-go-ts-runtime].
