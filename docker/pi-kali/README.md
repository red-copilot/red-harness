# Red Harness Pi Kali image

This image is the default execution environment for `type: pi`.

Base image:

- `kalilinux/kali-last-release`, pinned by the OCI index digest in `Dockerfile`

Installed base:

- `kali-linux-core`
- Node.js / npm
- Python 3
- Pi coding agent

The Dockerfile switches the base image's APT sources from `kali-rolling` to
`kali-last-snapshot` before installing packages. This keeps builds on Kali's
point-release branch instead of following the daily rolling repository. Kali
refreshes that branch with its point releases, so a rebuild can change after a
new point release. The image also pins the Pi npm package version and installs
security tools at build time; solving does not install packages. To update the
base, resolve and review a new official Kali image digest, then build and run the
container/network integration suite.

Curated security tools include network discovery, web enumeration, credential testing, SQL injection testing, packet inspection, and basic binary/debugging utilities.

The Harness starts the image with a read-only root filesystem, a read-only `/task` mount, a writable `/run/harness` run bundle, dropped Linux capabilities, and explicit network attachment. No Linux capabilities are enabled by default; a task must explicitly opt into `NET_RAW` or another capability through the Pi configuration.
