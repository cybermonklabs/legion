# The image every CI job that can launch WebKit or Firefox runs in: Playwright's own browser
# host image, with the few tools the Dispatch end-to-end harness needs added at image-build
# time, so that no CI job installs a browser host package from Ubuntu's apt mirror pool
# (dispatch://INFRA-1443).
#
# .github/workflows/browser-image.yaml builds and publishes it, and .github/ci-images.yaml holds
# its row: `built: .github/docker/browser.Dockerfile` where a copied row holds `upstream:`, with
# the digest the two `container:` jobs of .github/workflows/dispatch-browser.yaml pin, `shard`
# (:180) and `alone` (:645). ci.yaml declares no `container:` of its own: it reaches those jobs
# by calling that workflow.
#
# The base bakes the browsers and every host package they need under /ms-playwright, with
# PLAYWRIGHT_BROWSERS_PATH pointing at it, and its Playwright version is the one bun.lock
# resolves (1.64.1, bun.lock:741,1967). .github/scripts/check-playwright-version.sh refuses a
# head where the two disagree: it reads the base FROM below, finds that digest's row, and takes
# the Playwright release out of that row's upstream tag. The base is this repository's own copy
# of mcr.microsoft.com/playwright:v1.64.1-noble, row playwright-v1.64.1-noble-2f05f8a7;
# 990,220,611 bytes compressed in 7 layers for linux/amd64.
# The file store the Dispatch browser suite uploads through, which CI ran as a `docker run` of
# chrislusf/seaweedfs:3.97 published on 127.0.0.1:18333. A job inside a container cannot reach a
# container the job started, so the binary is baked in below and the job starts it on that same
# address. The source is this repository's own copy of that image, row seaweedfs-3.97-bb05d66d
# of .github/ci-images.yaml; `weed` lives at /usr/bin/weed there and is a static ELF, so it
# needs nothing else copied with it.
#
# A named stage, and never `COPY --from=<a registry reference>`:
# .github/scripts/check-ci-images.sh reads a FROM argument and does not read a COPY --from, so
# the second form would take this digest past the gate that holds every CI image to a row of
# .github/ci-images.yaml.
FROM ghcr.io/cybermonklabs/legion/ci@sha256:bb05d66d2963b1cc48073190781c3dd29e2c4c88a2ae2986bf58e38c86d89c6e AS weed

FROM ghcr.io/cybermonklabs/legion/ci@sha256:2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f

LABEL org.opencontainers.image.source=https://github.com/cybermonklabs/legion
LABEL org.opencontainers.image.description="Playwright v1.64.1-noble with PostgreSQL 18 and pgvector, weed, jq, python3, flock and a DejaVu core font: the image every CI job that can launch WebKit or Firefox runs in"

# What the base image already carries, read from its own config history and then from the built
# image rather than guessed: node 26 and npm (NODE_VERSION=26, not 24), yarn, curl, wget, gpg,
# ca-certificates (so the openssl CLI too, which run-server.sh's `openssl genrsa` needs), git,
# openssh-client, zstd, `flock` (util-linux is an Ubuntu base package), a `pwuser` at
# **uid 1001, gid 1001** -- not 1000 -- and the browsers at /ms-playwright. Its apt sources
# point at azure.archive.ubuntu.com, which this build inherits. A stalled mirror here fails one
# image build and no test job, which is the whole point of installing at build time.
ARG DEBIAN_FRONTEND=noninteractive

# PostgreSQL's own apt repository (apt.postgresql.org), because Ubuntu noble carries 16 and the
# harness's migration 0072 needs a `vector` extension built against the server it runs
# (packages/dispatch/e2e/harness-postgres.ts). noble-pgdg carries postgresql-18 18.6 and
# postgresql-18-pgvector 0.8.7.
ARG PG_MAJOR=18
# The SHA-256 of that repository's armored signing key, checked below before apt reads one index
# signed by it: apt trusts every package the key signs, so a rotated or re-exported key has to
# fail this build and be read by a person rather than be trusted as served. It is the same key,
# from the same URL, that packages/daemon/docker/worker.Dockerfile fetches, and this value is
# that file's own ARG PGDG_KEY_SHA256 copied across; its header records which key it stands for,
# RSA-4096 B97B 0AFC AA1A 47F0 44F2 44A0 7FCC 7D46 ACCC 4CF8, uid "PostgreSQL Debian
# Repository".
ARG PGDG_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
# The uid GitHub's ubuntu-24.04 runner runs as, and the uid that owns $GITHUB_WORKSPACE and
# every bind mount a `container:` job gets. `initdb` refuses to run as root, so the job runs the
# container as this uid -- and `initdb` refuses a uid with no passwd entry too. This base
# already answers for uid 1001: it is `pwuser`, created by the base image's own `adduser
# pwuser`. Nothing here creates that user, so the assertion block below is what holds the
# promise: a future base that numbers pwuser differently fails the build by name instead of
# publishing an image whose steps run as somebody else.
ARG RUNNER_UID=1001

# One layer for everything that is installed. The apt lists are removed in the same layer, so
# they never reach the pushed image.
RUN set -eux; \
    # postgresql-common's postinst runs `pg_createcluster` and so a whole `initdb` for a cluster
    # at /var/lib/postgresql/18/main that nothing here ever starts: the harness makes its own
    # under the run's temporary directory. Turning it off keeps both the build and the image
    # smaller.
    install -d /etc/postgresql-common; \
    echo 'create_main_cluster = false' > /etc/postgresql-common/createcluster.conf; \
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
      -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    # The key is checked before the source that trusts it is written, so no index signed by an
    # unexpected key is ever read. The ARG above says what the value is and where it came from.
    printf '%s  %s\n' "$PGDG_KEY_SHA256" \
      /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc > /tmp/pgdg-key.sha256; \
    sha256sum --check --strict /tmp/pgdg-key.sha256; \
    rm -f /tmp/pgdg-key.sha256; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "postgresql-${PG_MAJOR}" \
      "postgresql-client-${PG_MAJOR}" \
      "postgresql-${PG_MAJOR}-pgvector" \
      python3 \
      util-linux \
      jq \
      # fc-list and fc-match, and the font the runner image resolves `sans-serif` to.
      # Measured: on ubuntu-24.04 `fc-match sans-serif` answers
      # `DejaVuSans.ttf: "DejaVu Sans" "Book"`, and in this base, which carries Playwright's
      # `tools` fonts and no DejaVu at all, it answers
      # `wqy-zenhei.ttc: "WenQuanYi Zen Hei" "Regular"` -- a CJK face with different metrics.
      # Every layout row of the suite measures text, and one of them
      # (agent-view.e2e.ts:922, the reasoning link's padded box) failed by 0.8 px in runs
      # 42338254048, 42339402235 and 42340825955 for exactly that reason. fonts-dejavu-core is
      # 1.1 MB and makes the two machines resolve the same family. The assertion block below is
      # what holds the answer where it has to be.
      fontconfig \
      fonts-dejavu-core \
      # oven-sh/setup-bun extracts Bun's release zip with @actions/tool-cache, whose
      # extractZip execs `unzip` on Linux (dist/setup/index.js, extractZipNix). Without it
      # ./.github/actions/setup-bun fails in a container before anything else runs.
      unzip \
      # gst-inspect-1.0, which Playwright's own dependency list does not install: it ships the
      # GStreamer libraries and plugins WebKit links and loads, not the command-line tools. The
      # registry assertion below is the thing that needs it.
      gstreamer1.0-tools; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# The `weed` of the stage above, in the one place a PATH lookup finds it.
COPY --from=weed /usr/bin/weed /usr/local/bin/weed

# initdb and pg_ctl live only in the version directory (the DISPATCH_E2E_PG_BIN the suite
# names); psql, pg_config and the rest are also reachable through postgresql-common's wrappers
# in /usr/bin. Appended, so nothing here shadows a runner tool of the same name.
ENV PATH="${PATH}:/usr/lib/postgresql/${PG_MAJOR}/bin"

# What this image promises, asserted at build time rather than in a CI step: a base or package
# change fails the build with the thing's own name rather than a browser row a day later.
RUN set -eux; \
    # The five GStreamer decoders WebKit plays media through, each `--exists`, which answers
    # from the registry this very command builds. All five are already in the base:
    # matroskademux and vp8dec from gstreamer1.0-plugins-good, h264parse from
    # gstreamer1.0-plugins-bad, avdec_h264 from gstreamer1.0-libav and vorbisdec from
    # gstreamer1.0-plugins-base, every one of them installed by the base image's own
    # `playwright-core install-deps`. Only gst-inspect-1.0 itself had to be added
    # (gstreamer1.0-tools, above).
    for element in matroskademux vp8dec h264parse avdec_h264 vorbisdec; do \
      gst-inspect-1.0 --exists "$element" \
        || { echo "::error::the GStreamer registry holds no $element"; exit 1; }; \
      echo "gst-inspect-1.0 --exists $element: present"; \
    done; \
    # Every tool the Dispatch end-to-end harness and run-server.sh reach for, proven here so a
    # missing one fails the build rather than a job.
    for tool in psql initdb pg_ctl pg_config python3 flock jq weed git openssl unzip tar curl node fc-match; do \
      command -v "$tool" >/dev/null || { echo "::error::the image holds no $tool"; exit 1; }; \
      printf '%-10s %s\n' "$tool" "$(command -v "$tool")"; \
    done; \
    # The uid every `container:` job of .github/workflows/dispatch-browser.yaml runs as
    # (`--user 1001`), which the base's own `adduser pwuser` answers for. Nothing in this file
    # creates it, so this read is what holds that promise: a base numbering pwuser differently
    # fails the build here by name, rather than publishing an image whose steps run as a uid
    # with no passwd entry -- which `initdb` refuses outright.
    getent passwd "${RUNNER_UID}" \
      || { echo "::error::no passwd entry holds uid ${RUNNER_UID}, which every container: job runs as"; exit 1; }; \
    # The generic family the docs site's screenshots and the suite's layout rows measure text
    # in. This image must answer it as GitHub's ubuntu-24.04 runner does, which is the whole
    # reason fonts-dejavu-core is installed above; without it fontconfig falls through to
    # Playwright's own CJK faces, whose metrics differ.
    expected='DejaVuSans.ttf: "DejaVu Sans" "Book"'; \
    answer=$(fc-match sans-serif); \
    echo "fc-match sans-serif: $answer"; \
    [ "$answer" = "$expected" ] \
      || { echo "::error::fc-match sans-serif answered '$answer', not '$expected'"; exit 1; }; \
    pg_config --version; \
    python3 --version; \
    weed version
