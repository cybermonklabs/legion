# The download_retry_sites filter's gate: what job download-retry reads.
GATE = gate("download_retry_sites", RETRY_JOB, called_job=RETRY_JOB, reads=RETRY_SITE_READS)
