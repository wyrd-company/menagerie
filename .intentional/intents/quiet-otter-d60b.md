---
menagerie: major
---

A workspace whose rendered definition has a `customizations.menagerie` block is provisioned by its jobs, with no step in a CLI or a web interface: Menagerie issues its Xolotl client key, makes its Wyrwood binding, creates its container, waits for its sockets and only then starts it. A create, start, rebuild, upgrade and apply-branch run the steps, each its own phase in the job console, and **Start** retries a job that failed without rotating the key or writing the binding again. A new create no longer overwrites a deleted workspace's backup branch: it copies the branch to `archive/<name>-<UTC stamp>` first. New configuration `provisioning.readiness-wait`, new phases `issue-key`, `bind`, `wait-for-sockets`, `verify` and `retire`, and new job errors `provisioning-invalid`, `provisioning-unavailable` and `provisioning-not-ready`.
