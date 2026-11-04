# Changelog

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
