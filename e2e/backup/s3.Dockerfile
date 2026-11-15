# The S3 server the backup suite's repository lives on: `rclone serve
# s3`, the static binary out of rclone's own image and nothing else.
# restic 0.18 works against it, multipart and `check --read-data`
# included.
# It has one key, which may do anything, and makes a bucket when asked —
# a stand-in for storage, not for a provider's policy, which the suite
# does not test. The Debian archive has no S3 server (its rclone, 1.60,
# has no `serve s3`), MinIO's community edition is archived, and
# SeaweedFS accepts a Deny policy without enforcing it.
#
# Pinned by digest — the multi-arch index's, so amd64 and arm64 each
# get their own binary; the tag is for Dependabot and for people.
FROM rclone/rclone:1.76@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466 AS rclone

FROM scratch
COPY --from=rclone /usr/local/bin/rclone /rclone
# Data is a tmpfs mounted at /data (docker-compose.yml), which counts
# against the container's memory limit.
ENTRYPOINT ["/rclone", "serve", "s3", "--addr", ":9000", "/data"]
