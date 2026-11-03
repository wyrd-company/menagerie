---
menagerie: minor
---

Run workspaces through devcontainer-builder 0.3.0, which honors more of a definition's `runArgs`, `mounts` and build options. A `mounts` string now follows Docker's `--mount`: without `type=` it is a volume, and a space around a field or a value is refused. A lifecycle command of an unsupported type now fails when the definition is read. A new `create-container` phase can take a time limit.
