---
title: Troubleshooting
order: 5
product: menagerie
relationships:
  references:
    - workspace-api
    - authentication
    - configuration
---

A failed job shows its kind and message at the end of its console. The kind
is stable; the message names what failed and what was left behind.

## Failed jobs

| Kind | Meaning |
| --- | --- |
| `template-refused` | Toha refused the template, for example because the template could not be fetched (`template-resolve-failed`), with its own reason, or Menagerie refused to apply it: the template is no longer registered, or an upgrade would not move the workspace forward (the workspace already has that version, its applied version and commit cannot be compared with the current version, the version's tag moved, or its commit does not descend from the applied one). The message says which. Nothing was written. For a private template over SSH, check the Wyrwood binding, that Menagerie's SSH key reaches the repository, and the known hosts. Over `https` with a [GitHub App](configuration.md#github-app), check that the App is installed on the repository with Contents read. |
| `template-service-unavailable` | The template service did not answer. Its own log is in the container's log. |
| `workspace-exists` | Another job created the folder while this interview ran, or Menagerie holds a workspace of that name: one being created, or one whose folder is gone and that still holds a binding or a key, which its **Delete** removes. Nothing was written. |
| `generation-failed` | Toha or a template hook failed while writing a new workspace. The folder holds what was written. A hook that cannot find a program needs a [runner image](configuration.md#runner-images) that has it; the hook's own output is in the console, prefixed `hook: `. When the hook's container itself failed, the console line `a template hook's throwaway container failed:` names the stage that failed and the class of the Engine's error. |
| `not-applied` | Toha reported that it wrote nothing. The workspace is unchanged. |
| `upgrade-failed` | Toha or a template hook failed while applying an upgrade. Files written before the failure remain; the state before the upgrade is on the backup branch, and the message names its commit. Recovery is manual. An upgrade's hooks run in Toha's candidate, so a failed hook wrote nothing to the workspace folder. A container failure has the same console line as for `generation-failed`. |
| `merge-conflicted` | The upgrade wrote conflict markers. Resolve them in the named files, then **Rebuild**. |
| `backup-failed` | A commit or push of the workspace failed. The message says what was changed: before an upgrade, nothing; after it, the devcontainer was not rebuilt. Check the workspace repository, that its credential, the SSH key or the [GitHub App](configuration.md#github-app) that **Backups** names for it, may push to it, and the Wyrwood binding; see [Backup repository credentials](#backup-repository-credentials). |
| `backup-move-failed` | A [move of a workspace's backups](usage.md#move-a-workspaces-backups) could not check, write or read back the destination, or the workspace's create has not finished. The message names the step and says the workspace still backs up to its earlier repository. Fix the cause and move again. |
| `backup-branch-conflict` | The destination already has a branch with the workspace's name, from another history, such as another host's workspace of that name. Nothing was pushed. Rename or remove that branch in the destination, or choose another repository. |
| `timed-out` | The job passed a configured [time limit](configuration.md#job-time-limits); the message names the phase and the limit. A hook still running was stopped and its container removed before the job ended. A create keeps its folder, as every failed create does. A build or up can leave a half-created container: **Start** reuses it and **Rebuild** removes it. |
| `host-prerequisites-missing` | The host lacks bind sources outside the workspace folder or the networks of `runArgs` that the new workspace's definition needs. The message names them all, and nothing was committed or started. Prepare the host, then **Start** the workspace, which finishes the create. |
| `provisioning-invalid` | The definition's `customizations.menagerie` block cannot be used: it is not valid, the definition has no `--name` in `runArgs`, it is a Compose definition, or Menagerie holds the container name or the Xolotl key for another workspace, or a container of another workspace has the name, or, with `provisioning.xolotl`, the workspace needs a new Xolotl key and its name is not 1 to 63 lower case letters, digits, `_` or `-`, starting with a letter or digit. The message names the field, the holder or the rule. Correct the template or the definition; a workspace whose name breaks the rule is created again under a valid name. |
| `provisioning-unavailable` | The definition declares a binding or a key, or Menagerie holds one for the workspace, and [`provisioning`](configuration.md#provisioning) lacks `wyrwood` or `xolotl`, or `provisioning.xolotl` lacks `key-policy` and the workspace needs a new key, or a name was imported with no `provisioning.wyrwood`, or Menagerie could not read a workspace's existing binding from Wyrwood to take over its policies (the message holds Wyrwood's answer). Configure it, restart Menagerie, then **Start** or **Delete** the workspace again. |
| `binding-outside-grant` | A delete, or the `retire` of a job that started the devcontainer, asked Wyrwood to remove a binding that the operator key's grant does not cover, such as one holding a policy outside it. Menagerie keeps the binding's name, and a delete removed no container, key or folder. Widen the grant or remove the binding in the Wyrwood web interface, then **Delete** or **Start** the workspace again. |
| `provisioning-not-ready` | The binding's status did not report what the step needs within [`readiness-wait`](configuration.md#provisioning), or the agent reports a failure on this container, such as an access group it cannot apply. The message holds the status and the agent's last error. A denied OpenBao login reads as a wait with a last error: check the shared Wyrwood policy. Then **Start** the workspace. |
| `initialize-command-failed` | The devcontainer's `initializeCommand` failed. The container was not started. |
| `registry-credentials-unavailable` | A registry credential could not be read. The pull or build did not start. See below. |
| `runner-image-unavailable` | No image could be found or pulled for the throwaway container of a template hook or `initializeCommand`. With no runner image on the template or in `runner-image`, Menagerie could not find its own container; set the template's runner image (**Edit** on the Templates screen) for its hooks, or the top-level [`runner-image`](configuration.md#runner-images), which `initializeCommand` uses. When an image is set, the image, the stage that failed (inspect or pull) and the class of the Engine's error, such as `not-found` or `unauthorized`, are named: for `initializeCommand` in the message, which starts `initializeCommand failed:`; for a template hook in the console line `a template hook found no runner image:`, while the message names the hook. |
| `operation-failed` | Any other failure of a container operation, such as a failed build or pull, a Xolotl or Wyrwood refusal, or a failed `retire` with the devcontainer running; the message holds the cause. |
| `internal-error` | A defect in Menagerie. Report it with the container's log. |

Without [job time limits](configuration.md#job-time-limits), a job that
never ends keeps its workspace busy. A timed-out job also keeps it busy
until its cancelled work stops. Restart Menagerie to end such a job: the
workspace folder stays as the job left it.

## Registry credentials

`registry-credentials-unavailable` carries one fixed reason. Credential
values and OpenBao's response text never appear.

| Reason | What to check |
| --- | --- |
| the secret does not exist | The secret at `<kv-mount>/<path>`. |
| OpenBao denied reading the secret | The Wyrwood policy grants `read` on `<kv-mount>/data/<path>`. |
| Wyrwood holds no OpenBao token for Menagerie (state not-bound) | The container `menagerie` is bound in Wyrwood. |
| Wyrwood holds no OpenBao token for Menagerie (state no-token) | The binding's token expired and the agent could not get a new one, for example while the Wyrwood controller is down. |
| the secret has no current version | The secret's latest version was deleted. |
| the secret needs non-empty string fields username and password | The secret's fields. |
| OpenBao returned a malformed response | OpenBao and the Wyrwood agent. |
| OpenBao answered status N | OpenBao's own log for that status. |
| the Wyrwood vault socket could not be reached | The binding exists and the consumer directory is mounted at `wyrwood-directory`. |
| the operation ended before OpenBao answered | The job ended first, for example at a restart. |
| the credential source failed | Any other failure of the credential source. |

## Backup repository credentials

**Validate** on a registration or on **Change credential** refuses a
credential that does not fit the URL, with the check `credential`:

| Refusal | What to do |
| --- | --- |
| choose the SSH key that reaches the repository | An SSH URL needs one of the keys the agent offers. With none listed, offer one in Menagerie's [Wyrwood policy](install.md#policy). |
| an SSH URL takes an SSH key as its credential, not a GitHub App | Choose a key, or register the repository's `https` URL to use an App. |
| choose the GitHub App for the repository | The URL is on a configured App's server: choose the App that is installed on the repository. |
| an https URL takes a GitHub App as its credential, not an SSH key | Choose an App, or register the repository's SSH URL to use a key. |
| GitHub App NAME does not serve the URL | The App's `server` is another; the message names the one it serves. |
| no GitHub App named NAME is configured | Add the App to `github-apps`, or choose another. |
| the SSH key is provisioning's operator key | Choose a Git key; no Git host is given the operator key. |

A backup or a move to a repository whose credential no longer works fails
with one of these:

| Message | What to check |
| --- | --- |
| ssh agent: the agent does not offer the repository's key SHA256:… | Menagerie's Wyrwood policy still offers that key, by its Transit key name. If the key was replaced, use **Change credential** to choose the new one, and give its public key to the Git host. |
| ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey] | The Git host does not know the repository's key: add the public key that **Change credential** shows to the repository as a deploy key with write access, or to an account that can write to it. For a repository that shows **Credential chosen by URL**, every key the agent offers was refused: choose the repository's key with **Change credential**. |
| workspace repository credentials: no GitHub App named NAME is configured | The App was renamed or removed in the configuration: restore it, or use **Change credential**. |
| workspace repository credentials: GitHub App NAME does not serve the repository's URL | The App's `server` changed: use **Change credential**. |

An Apply branch fetch fails with
`fetching branch BRANCH from the workspace repository failed; nothing was changed`
and no reason. To find it, open **Backups** and read the credential on the
workspace's repository:

- **The App's name, or the key's name with its fingerprint**: the
  credential is still offered. Choose **Change credential**, keep it, and
  choose **Validate**. The refusal gives the reason, and Validate changes
  nothing.
- **A key's fingerprint alone, "which the SSH agent no longer offers"**:
  the key is gone from the agent. Change credential would offer another
  key, so do not use it to diagnose. Offer the key again in Menagerie's
  [Wyrwood policy](install.md#policy), or choose its replacement with
  **Change credential** and give its public key to the Git host.
- **An App "which is no longer configured"**: restore the App in
  `github-apps`, or choose another with **Change credential**.

A failed **Validate** names the credential the Git host refused, as in
`Menagerie could not write to the repository with SSH key SHA256:…` or
`with GitHub App NAME`. A repository that shows **Credential chosen by URL**
names none.

## GitHub App credentials

A backup, an Apply branch fetch or a template fetch that uses the
[GitHub App](configuration.md#github-app) fails with
`GitHub App credentials are unavailable:` and one fixed reason. A token,
the App's JWT, a URL and GitHub's or OpenBao's response text never appear.

| Reason | What to check |
| --- | --- |
| OpenBao denied signing with the Transit key | The Wyrwood policy grants `update` on `transit/sign/<transit-key>/sha2-256`. |
| OpenBao answered status N to the signing request | The Transit key `transit-key` exists and is an RSA key. |
| OpenBao returned a malformed signature | OpenBao and the Wyrwood agent. |
| Wyrwood holds no OpenBao token for Menagerie (state …) | As for registry credentials above. |
| the Wyrwood vault socket could not be reached | The binding exists and the consumer directory is mounted at `wyrwood-directory`. |
| GitHub refused the App's JWT | `app-id` is the App's id, the Transit key holds the App's private key, and the host's clock is right. |
| the App is not installed for the repository | The App is installed on the repository's owner with access to the repository. |
| the App's installation was not found | The `installations` entry for the owner is the App's installation id. |
| GitHub refused the token: the App lacks the permission | The App has the Contents permission. |
| GitHub refused the token: the repository is not in the installation, or the permission is not granted | The installation includes the repository, and the App has Contents read and write for backups. |
| GitHub answered status N to the installation lookup or the token request | GitHub's status for that code. |
| GitHub could not be reached | Network access to the API, `server`, and the certificate of GitHub Enterprise Server. |
| GitHub returned a malformed response | `server` names a GitHub server. |
| the operation ended before GitHub answered, or before OpenBao answered | The job ended first, for example at a restart or a time limit. |
| the workspace service's credential route could not be reached | A template fetch: the workspace service is running, and `template-service.hook-runner-socket` names its socket. |
| the workspace service's credential route answered with an unexpected status | A template fetch: the template service and the workspace service come from the same image. |
| the workspace service's credential route gave a malformed answer | As for an unexpected status. |
| the token could not be minted | A template fetch: the workspace service's log. |
| the operation ended before credentials arrived | A template fetch: the job was cancelled or reached its time limit while its token was minted. |

A template operation that fails after the template service received a
GitHub App token shows only a fixed sentence for its error kind, such as
`the template could not be fetched; the Git host refused or failed the
request`. The Git host's answer is not shown or recorded anywhere,
because it can hold the token. Check the template's address and ref, the
App's installation and Contents permission on the template repository,
and the Git host's status.

A push or fetch over `https` names a fixed reason too:

| Reason | What to check |
| --- | --- |
| the workspace repository did not accept the credentials | The App's installation and permissions. |
| the workspace repository denied access | The App has Contents read and write. |
| the workspace repository was not found | The workspace's backup repository URL, on [Backup repositories](usage.md#backup-repositories). |
| the workspace repository answered status N | The server's status for that code. |
| the workspace repository answered with a redirect; register the repository at its current address | The repository moved: give its current address. |
| the workspace repository address is not a valid Git URL | The workspace's backup repository URL. |
| the workspace repository's credentials could not be prepared | The credential source for that URL. |
| the workspace repository could not be reached or refused the transfer | Network access, and the remote's text in the job console. |

The job console shows the remote's own text, such as a hook's rejection,
with the URL and the token replaced by `[redacted]`.

## Requests the API refuses

| Kind | Meaning |
| --- | --- |
| `workspace-busy` | The workspace already runs a job; the answer names it. |
| `unknown-template` | No registered template has this id; register it on the Templates screen, or use the id of a registered one. |
| `invalid-workspace-name` | The name cannot be a workspace's folder directly in the workspace root; for example, it starts with a dot. On a host with provisioning, a create also refuses a name that is not 1 to 63 lower case letters, digits, `_` or `-`, starting with a letter or digit. |
| `workspace-not-found` | No such folder. |
| `docker-unavailable` | The Docker daemon did not answer. |
| `workspace-root-unreadable` | The workspace root cannot be listed. |
| `unauthenticated` | No session: sign in again. A restart of Menagerie signs everyone out. |

## Sign-in

| Page | Meaning |
| --- | --- |
| Not a manager | The identity provider signed the person in, but their `groups` claim does not contain `menagerie_managers`. |
| Sign-in expired | The sign-in was started in another browser, or was already used. Sign in again. |
| Sign-in refused | The identity provider returned an error. |
| Sign-in rejected | The ID token, the code exchange or the `groups` claim failed verification. Check `oidc.issuer` and `oidc.client-id`. |
| Identity provider unavailable | Menagerie could not reach the issuer. |

## Start-up

When the container stops at start-up, its log names the cause:

| Log line | What to do |
| --- | --- |
| the workspace root ... is not mounted | Mount the workspace root at its own path. |
| ... is not readable by uid N | Make `config.yml` readable by the workspace root's owner. |
| /var/lib/menagerie belongs to uid N ... | Use a new volume, or the volume this owner's Menagerie wrote. |
| /var/lib/menagerie/... is a symbolic link / is not a folder | Replace it with a real folder, or use a new volume. |
| ... belongs to uid N, but Menagerie runs as uid M | Data written by another user: use a new volume. |
| the configuration in /etc/menagerie/config.yml is not valid | The line before it, `menagerie: ...`, names the invalid key. |

When either service stops later, the container stops with that service's
exit code and the log says `menagerie: <service> stopped (...); stopping the
container`. Docker's restart policy starts both services again.
