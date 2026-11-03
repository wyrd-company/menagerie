---
menagerie: patch
---

A folder in the workspace root whose name starts with a dot, such as `.shared`, is not a workspace: it is not listed, and an operation that names it is refused as `invalid-workspace-name`.
