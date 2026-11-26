# Changelog

## 0.2.8

### Fixes

- The installation command and the deployment example use the Menagerie 0.5.1 image, and the install guide says how to upgrade from 0.5.0.

## 0.2.7

### Features

- Workspaces can back up to different Git repositories. A new **Backups** screen registers a backup repository with a display name and a Git URL once Menagerie has proved it can write to it, and removes one no workspace backs up to. **New workspace** asks which backup repository the workspace backs up to, choosing the only registered one for you, and the workspace list shows where each workspace backs up to. The create job request takes an optional `backupRepositoryId`. The first start of this release registers the configured `workspace-repository` once and keeps every existing workspace backing up to it; the key is then optional and no longer read, and each start that finds it logs a warning, so remove it after that start.
- A workspace's backups can move to another registered backup repository: **Move backups** on its row starts a `move-backup` job that checks Menagerie can write to the destination, refuses a destination whose branch of the workspace's name holds another history (`backup-branch-conflict`), commits and pushes a current backup there and reads it back, and only then makes the destination the workspace's repository. A failed move (`backup-move-failed`) keeps the earlier repository and says why, and the earlier repository keeps its branch for recovery. A workspace with no backup repository gets its first the same way. New `job-limits.operations.move-backup`.

## 0.2.6

### Fixes

- The installation command and the deployment example use the Menagerie 0.5.0 image.
- The upgrade steps from 0.4.2 say that a create, an upgrade and an apply branch need a Xolotl key too, apply the `templates` steps only where the configuration has that key, and say what an unreadable definition loses. The failure table adds a cause of `provisioning-unavailable`, says the name rule is enforced on a create, and says which runner image is used.

## 0.2.5

### Fixes

- Register templates in the interface, with no configuration change or restart. A manager registers a template from its Git address, and Menagerie refuses one that fails a check, naming it: its address, listing its versions, a release tag, fetching and reading it, and `.devcontainer/devcontainer.json` in its files. A version is a release tag `MAJOR.MINOR.PATCH` of the template's repository; a manager upgrades a template to a newer version, and an upgrade of a workspace applies its template's current version and no longer takes a `templateId`. Templates can be renamed, given a runner image, and removed while no workspace uses them. The first start imports the configuration's `templates` entries once, one template per repository at its newest version, and names by id the entries it skips; the key is no longer read, and each start warns while it is present. The template service refuses folder, `file://`, `git://`, `http://` and template-name addresses, and any address with a password. An upgrade that names `templateId` is refused as `invalid-request`; an upgrade without it applies the current version of the workspace's template, so upgrade the template first to take a newer version. The import keeps one template per repository and folder, with the newest entry's id: a create naming another entry's id is refused as `unknown-template`, so point clients at the id that is kept before you upgrade. It skips a repository none of whose entries names a `MAJOR.MINOR.PATCH` tag (a leading `v` is accepted), an entry on a host with a port that names a ref, and an address the template service refuses; the start log names each skipped entry, so register those templates in the interface, then remove the `templates` key.
- A template address on a host with a port takes no ref: in an `https://` or `ssh://` address no `:` may come before an `@`, so a password that holds a `/` is refused before any fetch. Register such a template without its ref, which takes the newest version; the first start skips a `templates` entry of that shape and names it by id.
- Document the Wyrwood upgrade that Menagerie needs: 0.7.1 or later. From 0.6.1 it is a stopped rollout with a schema 5 migration (stop all agents, back up controller state, upgrade the controller and every stopped agent, then restart the agents); rollback requires the backup.
- Describe Menagerie's data: the No folder row and its delete, what a delete and `retire` remove whatever the definition says, `binding-outside-grant`, backing up the data volume, and what the first start after the upgrade takes over and what to repair before it.
- Manage required Wyrwood policies and single-select or multi-select policy groups, with grant checks and revisioned workspace selections.
- Upgrading from 0.4.2 needs steps on the host. Before you pull the image: upgrade the Wyrwood controller and agents to 0.7.1 first, in Wyrwood's upgrade order; back up the `/var/lib/menagerie` volume, as going back needs it; repair a workspace definition that no longer parses; make each `templates` entry name a `MAJOR.MINOR.PATCH` tag (an entry on a host with a port takes no ref: register that template in the interface after the first start instead); and point clients at the template ids that are kept, as the import keeps only the newest entry's id for each repository and folder and a create naming another entry's id is refused as `unknown-template`. With `provisioning.xolotl`, set `provisioning.xolotl.key-policy` before the first start, and this covers existing workspaces: every workspace whose definition has a `customizations.menagerie` block and no Xolotl key gets a new client key `<key-policy>/<name>` at its next Start or Rebuild, where 0.4.2 issued one only when the template declared `xolotl` (keys a 0.4.2 template declared are imported at the first start). Without `key-policy`, that Start or Rebuild fails as `provisioning-unavailable`. With `provisioning.xolotl`, a workspace that has a `customizations.menagerie` block and no Xolotl key, and whose name is not 1 to 63 lowercase letters, digits, `_` or `-`, starting with a letter or digit, fails every Start and Rebuild as `provisioning-invalid` once `key-policy` is set. A workspace whose 0.4.2 definition declares `customizations.menagerie.xolotl` keeps the key imported at the first start, a workspace with no block is not provisioned, and a host without `provisioning.xolotl` issues no key: none of those is affected. Before the upgrade, list the affected workspaces, recreate each under a valid name, move its work and delete the old one; a delete still works after the upgrade. After the first start: read the start log for the entries it skipped and register those templates in the interface, then remove the `templates` key; with `provisioning.wyrwood`, mark as required, or put in groups, the policies your templates used to declare, before anyone creates a workspace; and stop sending `templateId` on an upgrade, which is refused as `invalid-request` (an upgrade without it applies the current version of the workspace's template). A new workspace name on a host with provisioning follows the same name rule.
- Choose workspace policies at creation and edit them on the workspace row. Apply stored selections immediately through Wyrwood 0.7.1 without a rebuild, retain imported identities, and show pending or failed application. Policies a template declares (`customizations.menagerie.wyrwood.policies`, `customizations.menagerie.wyrwood.values` and `customizations.menagerie.xolotl`) are no longer read: mark the policies every workspace needs as required, or put them in groups, on the Policies screen before anyone creates a workspace. With `provisioning.xolotl`, a workspace whose definition has a `customizations.menagerie` block and no Xolotl key gets a new client key `<key-policy>/<name>` at its next create, start or rebuild, and that fails as `provisioning-unavailable` until `provisioning.xolotl.key-policy` is set, as the key policy no longer comes from a template. 0.4.2 issued a key only when the template declared `xolotl`; keys a 0.4.2 template declared are imported at the first start. Menagerie needs Wyrwood 0.7.1 or later, up from 0.6.1: upgrade the Wyrwood controller and agents first. The configuration guide covers the host's key policy and policy recovery.
- Menagerie finds new versions of each registered template by itself, when it starts and every `template-versions.every` (an hour by default), and on request with **Check versions**, with no restart. The Templates screen shows each template's newest found version and its last check, and a failed check keeps the versions found before. A template is upgraded to a version a check found, chosen from a list, and a moved tag refuses creates and upgrades until the template is upgraded to that version again. Each workspace shows the version last applied to it, an unknown one, and whether it needs an upgrade. An upgrade compares that stored version, and one whose version is unknown goes on only when the current version descends from its applied commit. Apply branch refuses a branch whose applied record would move the workspace backward. The first start of this release records each workspace's applied version from its applied record.
- Link Wyrwood's upgrade order and backup procedure from a dedicated upgrade section. The controller must be stopped before its /var/lib/wyrwood state directory is copied for rollback.
- Document widening Menagerie's existing Wyrwood operator grant with one edit and no new key.

## 0.2.4

### Fixes

- Usage and configuration describe **Ask every question again** on the upgrade screen.

## 0.2.3

### Fixes

- The demo, the screenshots and the highlights show provisioning, **Delete** and **T3 Code token**; the usage page has new images of a provisioned create, a pairing token and the delete form.

## 0.2.2

### Fixes

- Installation gives the order for the first install and replaces the repair after a host restart: the Wyrwood agent now starts a bound container. Usage says to stop a container an upgrade that failed left running before **Start**.
- Describe how a `mounts` string is read and the `create-container` phase.
- Describe how to prepare a host for the Wyrwood operator key: the Transit key, the policy all of a host's workspaces share, the approval and its grant.
- The installation command and the deployment example use the Menagerie 0.4.0 image.
- The configuration page says which entries the template service removes from its candidate folder at each start.
- Describe deleting a workspace, its phases and the `delete` time limit.
- State that a folder in the workspace root whose name starts with a dot is not a workspace.
- Describe how to get a T3 Code pairing token for a workspace.
- The time limit reference says that a template fetch cannot be interrupted and writes nothing once its job has ended.
- Describe the `host-prerequisites` phase, the `host-prerequisites-missing` job error, and the bind source folders Menagerie creates.
- Describe `provisioning.xolotl` and the Xolotl policies a host needs.
- Describe a workspace with a provisioning declaration: its phases, retries, retirement of what a definition no longer declares, `provisioning.readiness-wait` and the new job errors. A failed create after generation is now finished by **Start**.

## 0.2.1

### Fixes

- Use the published Menagerie 0.3.0 image in the installation command and deployment example.

## 0.2.0

### Features

- Document job time limits and alerts, scoped local environment, throwaway hook containers, signed interviews, workspace commits and sockets, reviewed branch application and GitHub App authentication. Publish every guide page in the docs navigation and retain the direct Docker socket host-root warning.

### Fixes

- Publish the public docs from docs/user in the wyrd.foo v2 format, with a product entry, highlights, screenshots and a demo.
