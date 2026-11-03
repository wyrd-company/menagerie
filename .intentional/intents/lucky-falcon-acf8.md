---
menagerie: patch
---

The deployment example mounts the Wyrwood consumer directory in long form with `create_host_path: false`, so Docker never creates a missing directory as root and Menagerie returns after a host restart with no step by hand. A first install creates the directory once before `docker compose create`.
