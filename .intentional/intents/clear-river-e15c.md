---
menagerie: minor
menagerie-docs: minor
---

A workspace's backups can move to another registered backup repository: **Move backups** on its row starts a `move-backup` job that checks Menagerie can write to the destination, refuses a destination whose branch of the workspace's name holds another history (`backup-branch-conflict`), commits and pushes a current backup there and reads it back, and only then makes the destination the workspace's repository. A failed move (`backup-move-failed`) keeps the earlier repository and says why, and the earlier repository keeps its branch for recovery. A workspace with no backup repository gets its first the same way. New `job-limits.operations.move-backup`.
