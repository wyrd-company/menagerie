---
menagerie: minor
menagerie-docs: minor
---

Each backup repository names its credential: a GitHub App from the new `github-apps` list, which names any number of Apps, or one of the SSH keys Wyrwood's agent offers. Its backups, moves and Apply branch fetches use that credential alone, so repositories in organizations with Apps of their own, or with deploy keys of their own, back up from one Menagerie. **Register repository** asks for the credential and shows the chosen key's public key to give the Git host, and **Change credential** changes it after a Validate with the new one passes. `GET /api/credentials` lists the Apps and keys, and `PUT /api/backup-repositories/{id}/credential` changes one. Repositories registered by an earlier release keep the credential their URL chooses until it is changed, and `github-app` keeps working as one App named `github-app`.
