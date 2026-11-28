# The jobs that prove the retry rather than the build (dispatch://INFRA-1545, T7):
# download-retry runs each real site under .github/scripts/ci-fault-proxy.py, and
# download-retry-images and download-retry-images-worker build the four Dockerfiles under the
# same fault, the first with the browser image twice and the second with the worker image five
# times, as five runs of one matrix, both on the download_retry_images input, which the gate
# below names through the first of them; its reads are both jobs'. Each gate declares
# the machinery alone — the retry loop, the fault, the rig, and the composite actions or
# Dockerfiles whose own downloads run under it — and nothing those downloads read. A
# manifest, a lockfile, a pin or a module file moves the bytes that come down the wire; what
# these jobs prove is that a download survives a faulting host, which those bytes cannot
# change. So this gate declares none of them, and the filter beside it names none either:
# between them the bun install set, the Go workspace and the two tool pins made an ordinary
# dependency bump select a job of about two hours. The job that proves a version is the one
# that builds with it — worker-image, browser-image and envoy-smoke for the images, every Bun
# and Go job for the rest.
GATE = gate("download_retry", RETRY_JOB, reads=RETRY_SITE_READS + RETRY_IMAGE_READS)
