---
menagerie: minor
menagerie-docs: minor
---

Each registered template names its credential too, chosen as a backup repository's is: a GitHub App that serves its `https` address, or one of the SSH keys Wyrwood's agent offers for an SSH address. Its checks, version checks, workspace creation and upgrades, and **Apply branch** fetch with that credential alone, and a refused fetch or a failed version check names it. **Register template** asks for the credential, **Validate** has a chosen App get a token for the repository, and **Change credential** changes it after a fetch with the new one passes, recording that fetch as the template's version check. `PUT /api/templates/{id}/credential` changes one. Templates registered by an earlier release, or imported from the `templates` key, keep the credential their URL chooses until it is changed.
