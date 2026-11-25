---
menagerie: minor
menagerie-docs: minor
---

Workspaces can back up to different Git repositories. A new **Backups** screen registers a backup repository with a display name and a Git URL once Menagerie has proved it can write to it, and removes one no workspace backs up to. **New workspace** asks which backup repository the workspace backs up to, choosing the only registered one for you, and the workspace list shows where each workspace backs up to. The create job request takes an optional `backupRepositoryId`. The first start of this release registers the configured `workspace-repository` once and keeps every existing workspace backing up to it; the key is then optional and no longer read, and each start that finds it logs a warning, so remove it after that start.
