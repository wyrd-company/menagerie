# Changelog

## 0.4.0

### Breaking

- A workspace whose rendered definition has a `customizations.menagerie` block is provisioned by its jobs, with no step in a CLI or a web interface: Menagerie issues its Xolotl client key, makes its Wyrwood binding, creates its container, waits for its sockets and only then starts it. A create, start, rebuild, upgrade and apply-branch run the steps, each its own phase in the job console, and **Start** retries a job that failed without rotating the key or writing the binding again. A new create no longer overwrites a deleted workspace's backup branch: it copies the branch to `archive/<name>-<UTC stamp>` first. New configuration `provisioning.readiness-wait`, new phases `issue-key`, `bind`, `wait-for-sockets`, `verify` and `retire`, and new job errors `provisioning-invalid`, `provisioning-unavailable` and `provisioning-not-ready`.

### Features

- Run templates that declare empty directories and user sections, through Toha 0.4.1. Honor a container name from `runArgs` and read-only mount object targets, through devcontainer-builder 0.2.1.
- Add `provisioning.wyrwood`, which locates the Wyrwood controller and selects the operator key that will manage workspaces' bindings. Menagerie offers the controller only that key, pins its host key, and names the key's fingerprint when it has no approval. Provisioning needs Wyrwood 0.6.1 or later.
- Add a delete operation. **Delete** on a workspace's row asks for its name, stops the devcontainer, commits and backs up, removes its Wyrwood binding, containers and Xolotl key, then removes its socket folder and folder. A failed delete can be retried. Ignored files are not backed up and are lost.
- Create the missing bind source folders inside a workspace before each start and rebuild. Check a new workspace's bind sources outside its folder and its network before its first commit, and report all that are missing at once as `host-prerequisites-missing`.
- Mint a T3 Code pairing token for a running workspace from its row in the workspace list, or with `POST /api/workspaces/{name}/t3-code-pairing-tokens`. The token is minted in the workspace's container through Docker exec, and Menagerie keeps no copy.
- Run workspaces through devcontainer-builder 0.3.0, which honors more of a definition's `runArgs`, `mounts` and build options. A `mounts` string now follows Docker's `--mount`: without `type=` it is a volume, and a space around a field or a value is refused. A lifecycle command of an unsupported type now fails when the definition is read. A new `create-container` phase can take a time limit.
- Accept `provisioning.xolotl`, the Xolotl URL and the OpenBao secret of the scoped key that issues and revokes workspace client keys. Requires Xolotl 0.6.0 or later.

### Fixes

- A generate or upgrade whose job was cancelled or reached its time limit while a template fetch ran no longer writes to the workspace folder when that fetch returns.
- The deployment example mounts the Wyrwood consumer directory in long form with `create_host_path: false`, so Docker never creates a missing directory as root and Menagerie returns after a host restart with no step by hand. A first install creates the directory once before `docker compose create`.
- The template service removes what a killed upgrade left in its candidate folder when it starts: Toha's temporary entries and the workspace service's copy-back folders, each named in its log. It leaves every other entry.
- A folder in the workspace root whose name starts with a dot, such as `.shared`, is not a workspace: it is not listed, and an operation that names it is refused as `invalid-workspace-name`.

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
