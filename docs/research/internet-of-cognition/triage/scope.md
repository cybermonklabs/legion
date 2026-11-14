# Triage: ioc-project/scope

Repository: github.com/ioc-project/scope.

Clone commit: 6f3e758623da6b4a3ae462813001c545bb15a9e6, of 2026-11-10.

Clone path in this pod: $IOC_CLONES/scope, where IOC_CLONES is /legion/scratch/tmp/ioc-clones.

This is a triage.
It does not run their code [t-triage-depth].

## What the repository is

SCOPE is the System for Cognitive Observability, Provenance and Explainability [t-sc-identity].

It reads OpenTelemetry traces of multi-agent runs, maps the spans onto a shared ontology, and keeps each run as a knowledge graph [t-sc-identity].

It adds analyses such as anomaly detection and consistency checking, and serves the result through an API and a dashboard [t-sc-identity].

The sentence above is the project's own prose about its code [t-sc-identity].

## Maturity

The clone is shallow, at depth one, so `git log` over it returns one commit [t-sc-clone-shallow] and `git tag` over it returns nothing [t-sc-tag-local].

Maturity is read over the GitHub REST API instead.

The repository holds four tags, one for each part: ui, ontology, helm and backend [t-sc-tags-api].

The parts are released apart, so one repository version does not exist [t-sc-tags-api].

It has one published release, the Helm charts 1.0.0 of 2026-11-05 [t-sc-releases].

The backend, ui and ontology tags carry no release entry [t-sc-releases].

The default branch holds at least 81 commits, and the repository was made on 2026-10-12 [t-sc-commits].

It is one month old [t-all-age].

The public following is small: 33 stars [t-org-stars].

## Licence

The licence is the Apache License 2.0, read from the LICENSE file [t-sc-licence].

## Size

The repository tracks 1344 files and 351863 lines [t-sc-size].

The parts are 881 backend files, 248 ui files, 69 docs files, 65 deploy files and 40 ontology files [t-sc-layout].

## Version range measured

backend/pyproject.toml declares version 1.0.0 for the package ioc-scope-lib [t-sc-version-backend].

This agrees with the version the plan cites.

But the repository does not hold one version.
The mce packages are at 2.0.0, the api, dem, norm, worker and backend packages are at 1.0.0, and two plugin packages are at 0.2.0 and 0.1.0 [t-sc-version-range].

The five ontologies are each at 1.1.0, which agrees with the ontology-v1.1.0 tag [t-sc-version-ontology].

The measured range is 0.1.0 to 2.0.0 [t-sc-version-range] [t-sc-version-ontology].

## What could cross into our runtime

One class only: the vocabulary.

scope holds six Turtle files, five ontologies and one SHACL shapes file [t-sc-cross-ontology].

Their sizes are 628, 427, 228, 166, 136 and 112 lines [t-sc-cross-ontology].

Nothing in scope is a prompt-shaped skill, and nothing in it is a specification document of a wire protocol [t-sc-drop-docs].

## What is dropped, and why

backend/, 881 Python files: the API, the workers, mce, dem, norm and the stateful evaluations.
It is a service beside Legion, and it needs Neo4j, ClickHouse and a broker, which this issue does not start [t-sc-drop-backend].

ui/, 248 files: a dashboard.
It is a service beside Legion [t-sc-drop-ui].

deploy/, 65 files: Helm charts and Docker Compose for their stack.
The one release of the repository is of this part [t-sc-drop-deploy].

The Python part of ontology/, 34 of its 40 files: generators, a SHACL validator and Pydantic models.
These would be a rebuild [t-sc-drop-ontology-code].

docs/, examples/ and overrides/, 85 files: a documentation site and demo traces [t-sc-drop-docs].

## One fact that limits every import

scope holds no Go source file [t-all-no-go-ts-runtime].

Its dashboard TypeScript is not a client library [t-all-no-go-ts-runtime].
