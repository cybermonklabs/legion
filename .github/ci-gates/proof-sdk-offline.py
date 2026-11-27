# The proof-sdk-offline job: a frozen install behind a proxy that answers 504 for every host
# the proof editor's old `github:sjawhar/proof-sdk` route, pinned to a commit, used, which proves the
# install reaches none of them. It reads what any frozen install reads (bun_job: setup-bun,
# the manifests, the lockfile, the patches and the tarball the proof editor installs
# proof-sdk-upstream from), the proxy program its first step starts, and this file.
GATE = gate("proof_sdk_offline", "proof-sdk-offline", reads=reads(
    (".github/scripts/ci-fault-proxy.py",
     f"{at('proof-sdk-offline', 'Start the fault proxy over the old proof-sdk route')} runs it; "
     f"the hosts it faults (the job's FAULT_HOSTS) and the log its refusals are counted from "
     f"are that step's arguments"),
) + bun_job("proof-sdk-offline"))
