---
menagerie: minor
menagerie-docs: patch
---

Register templates in the interface, with no configuration change or restart. A manager registers a template from its Git address, and Menagerie refuses one that fails a check, naming it: its address, listing its versions, a release tag, fetching and reading it, and `.devcontainer/devcontainer.json` in its files. A version is a release tag `MAJOR.MINOR.PATCH` of the template's repository; a manager upgrades a template to a newer version, and an upgrade of a workspace applies its template's current version and no longer takes a `templateId`. Templates can be renamed, given a runner image, and removed while no workspace uses them. The first start imports the configuration's `templates` entries once, one template per repository at its newest version, and names by id the entries it skips; the key is no longer read, and each start warns while it is present. The template service refuses folder, `file://`, `git://`, `http://` and template-name addresses, and any address with a password.
