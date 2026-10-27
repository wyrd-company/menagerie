---
menagerie: minor
---

Apply branch brings a branch of the workspace repository, such as a reviewed pull request's head, into a workspace from its row in the workspace list or as the `apply-branch` job. Menagerie fetches only that branch, commits and backs up local changes, then fast-forwards the workspace or merges it file by file, backs up and rebuilds the devcontainer. A branch that would not apply cleanly (a file both sides changed, an untracked or ignored file in the way, a submodule) fails as `not-applicable` and lists the paths, with nothing applied. Propose changes to a workspace as a pull request whose base is its branch, do not merge it on the forge, and apply its head branch.
