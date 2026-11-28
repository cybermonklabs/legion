# The download_retry_images filter's gate: what job download-retry reads.
GATE = gate("download_retry_images", RETRY_JOB, called_job=IMAGES_JOB, reads=RETRY_IMAGE_READS)
