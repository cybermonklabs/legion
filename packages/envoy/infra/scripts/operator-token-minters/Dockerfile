# The minter auditor extends the published worker image by digest. That image already carries bash,
# jq, gcloud, kubectl, curl and agent-secrets; this image installs the GKE exec plugin and adds only
# the auditor entrypoint and check.
FROM ghcr.io/cybermonklabs/legion-worker@sha256:83b5ad7293bff529d3b0a101e8fd0a7556c142f6681572fbe02a4d8db8278faf

USER root
# The component lands in the SDK's own bin directory, which is not on PATH; kubectl's exec
# credential plugin is looked up on PATH.
RUN gcloud components install gke-gcloud-auth-plugin --quiet \
    && ln -s "$(dirname "$(readlink -f "$(command -v gcloud)")")/gke-gcloud-auth-plugin" /usr/local/bin/gke-gcloud-auth-plugin \
    && gke-gcloud-auth-plugin --version

COPY packages/envoy/infra/scripts/operator-token-minters.sh /usr/local/bin/operator-token-minters.sh
COPY packages/envoy/infra/scripts/operator-token-minters /opt/legion/operator-token-minters
RUN chmod 0755 /usr/local/bin/operator-token-minters.sh /opt/legion/operator-token-minters/entrypoint.sh \
    && ln -s /opt/legion/operator-token-minters /usr/local/bin/operator-token-minters

USER 1000:1000
ENTRYPOINT ["/opt/legion/operator-token-minters/entrypoint.sh"]
