# Red Harness Pi Kali image

This image is the default execution environment for `type: pi`.

Base image:

- `kalilinux/kali-rolling`

Installed base:

- `kali-linux-core`
- Node.js / npm
- Python 3
- Pi coding agent

Curated security tools include network discovery, web enumeration, credential testing, SQL injection testing, packet inspection, and basic binary/debugging utilities.

The Harness starts the image with a read-only root filesystem, a read-only `/task` mount, a writable `/run/redharness` run bundle, dropped Linux capabilities, and explicit network attachment. `NET_RAW` is the only capability enabled by the default Pi contract.
