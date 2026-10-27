# Changelog

## 0.3.0

### Features

- Jobs can have configured time limits, per operation and per phase, that cancel a job as timed-out without counting time spent waiting for answers, and a webhook can be told about failed and long-running jobs.
- A devcontainer definition reads only the variables of Menagerie's environment that the new `local-env` setting lists, through `${localEnv:NAME}`, Compose interpolation and `initializeCommand`. Without `local-env` it reads none: a definition that used another variable now gets its default value, and the job log names the variable. Add the variables a definition needs to `local-env`.
- Template hooks and initializeCommand run in throwaway containers that mount only the workspace folder, so they cannot reach the Docker socket, the Wyrwood sockets or Menagerie's environment. Hooks run in a template's runner-image, initializeCommand in the global runner-image, and both default to Menagerie's own image; hook output the template does not capture now appears in the job console. A job cancelled at a time limit removes its running hook containers before it ends.
- Authenticate to GitHub with a GitHub App: backups push and private https templates fetch with short-lived installation tokens, minted through the App key held in OpenBao Transit, with no machine user and no SSH key. SSH stays for other hosts.
- The template service signs the interview session it hands to the browser. It refuses a session that was altered, is unsigned, or was issued for another workspace, template or flow, with the error `session-tampered`. The signing key is kept as `toha/session-key` in the `/var/lib/menagerie` volume, so interviews continue across a restart of the template service.
- Menagerie commits and backs up workspace changes as a new `commit` job: it records every change as `git add --all` would, leaving ignored files and nested repositories out, and pushes the workspace branch. With `auto-commit: {every: 1h}` it commits every idle workspace that has changes on a schedule. With `workspace-socket: {folder: ...}` each devcontainer gets a socket at `/run/menagerie/workspace.sock` and the client `/run/menagerie/bin/workspace-commit`, which commits and backs up its own workspace; a second commit within `min-interval` (1m by default) is refused. Existing devcontainers get the socket at their next rebuild.
- Apply branch brings a branch of the workspace repository, such as a reviewed pull request's head, into a workspace from its row in the workspace list or as the `apply-branch` job. Menagerie fetches only that branch, commits and backs up local changes, then fast-forwards the workspace or merges it file by file, backs up and rebuilds the devcontainer. A branch that would not apply cleanly (a file both sides changed, an untracked or ignored file in the way, a submodule) fails as `not-applicable` and lists the paths, with nothing applied. Propose changes to a workspace as a pull request whose base is its branch, do not merge it on the forge, and apply its head branch.

## 0.2.0

### Features

- The web UI has a new look: a plain operations console with the Menagerie logo, and a dark theme that follows the operating system setting.

## 0.1.0

### Features

- Menagerie's first release: a web UI where managers sign in through OpenID Connect, create workspaces from approved Toha templates, start, stop and rebuild their devcontainers, and upgrade them to newer template versions, with each workspace backed up to its own Git branch. It ships as one container image for Linux amd64 and arm64.
