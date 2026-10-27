---
menagerie: minor
---

Menagerie commits and backs up workspace changes as a new `commit` job: it records every change as `git add --all` would, leaving ignored files and nested repositories out, and pushes the workspace branch. With `auto-commit: {every: 1h}` it commits every idle workspace that has changes on a schedule. With `workspace-socket: {folder: ...}` each devcontainer gets a socket at `/run/menagerie/workspace.sock` and the client `/run/menagerie/bin/workspace-commit`, which commits and backs up its own workspace; a second commit within `min-interval` (1m by default) is refused. Existing devcontainers get the socket at their next rebuild.
