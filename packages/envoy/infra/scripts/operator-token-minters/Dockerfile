# The minter auditor extends a released worker image by digest. That image already carries bash,
# jq, gcloud, kubectl, curl and agent-secrets (which the check no longer calls); this image adds
# the GKE exec plugin and the auditor entrypoint and check.
#
# The base is the worker image of a legion-v* release, built by a run of worker-image.yaml on main,
# and release-minters-auditor.yaml verifies its GitHub artifact attestation against the commit
# below before it builds. A pull request's run pushes a sha- tag and attests nothing, and a pull
# request may edit the workflow that pushes it (docs/kubernetes.md, "Pin by digest, never by tag"),
# so a base from such a run would put this check's own bash, jq, gcloud, kubectl and curl outside
# review: a changed jq alone can make every step print PASS. Bump the two values together, from a
# release's "Worker image" digest and the commit its tag names, and verify the new pair with the
# command in that section of docs/kubernetes.md before committing it.
ARG WORKER_IMAGE_DIGEST=sha256:9662eb8a2c012438a1c48410beb5e772f594cf3ec3ef89840284fe0773c08aa9
# legion-v26.12.4, whose tag names this commit.
ARG WORKER_IMAGE_SOURCE_COMMIT=45f6e9d39d5602994a7dc417b73617fc064b6e62
FROM ghcr.io/cybermonklabs/legion-worker@${WORKER_IMAGE_DIGEST}
LABEL org.opencontainers.image.source=https://github.com/cybermonklabs/legion

USER root
# kubectl looks its exec credential plugin up on PATH. `gcloud components install` fetches whatever
# the component manager serves at build time, with no version or hash recorded here, so the plugin
# is fetched as the worker image fetches kubectl and the SDK itself: one pinned archive of the
# SDK's rapid channel, checked against the SHA-256 that channel publishes for it. Both values are
# the channel's own, from component gke-gcloud-auth-plugin-linux-x86_64 of
# https://dl.google.com/dl/cloudsdk/channels/rapid/components-2.json, read 2026-11-27, when the
# channel served SDK 594.0.1 and plugin 0.5.21.
ARG GKE_AUTH_PLUGIN_VERSION=0.5.21
ARG GKE_AUTH_PLUGIN_ARCHIVE=components/google-cloud-sdk-gke-gcloud-auth-plugin-linux-x86_64-20261030071842.tar.gz
ARG GKE_AUTH_PLUGIN_SHA256=b8d3bf3ebce3aff0146d85457146869ea66da7dc62d291e176cb88b6e73dd452
RUN set -eu; \
    t="$(mktemp -d)"; \
    curl -fsSLo "$t/plugin.tar.gz" "https://dl.google.com/dl/cloudsdk/channels/rapid/${GKE_AUTH_PLUGIN_ARCHIVE}"; \
    printf '%s  %s\n' "$GKE_AUTH_PLUGIN_SHA256" "$t/plugin.tar.gz" > "$t/SHA256SUMS"; \
    sha256sum --check --strict "$t/SHA256SUMS"; \
    tar -xzf "$t/plugin.tar.gz" -C /usr/local --no-same-owner bin/gke-gcloud-auth-plugin; \
    chmod 0755 /usr/local/bin/gke-gcloud-auth-plugin; \
    rm -rf "$t"; \
    gke-gcloud-auth-plugin --version

COPY packages/envoy/infra/scripts/operator-token-minters.sh /usr/local/bin/operator-token-minters.sh
COPY packages/envoy/infra/scripts/operator-token-minters /opt/legion/operator-token-minters
RUN chmod 0755 /usr/local/bin/operator-token-minters.sh /opt/legion/operator-token-minters/entrypoint.sh \
    && ln -s /opt/legion/operator-token-minters /usr/local/bin/operator-token-minters

USER 1000:1000
ENTRYPOINT ["/opt/legion/operator-token-minters/entrypoint.sh"]
