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
| `template-refused` | Toha refused the template, for example because the template could not be fetched (`template-resolve-failed`), with its own reason. Nothing was written. For a private template over SSH, check the Wyrwood binding, that Menagerie's SSH key reaches the repository, and the known hosts. |
| `template-service-unavailable` | The template service did not answer. Its own log is in the container's log. |
| `workspace-exists` | Another job created the folder while this interview ran. Nothing was written. |
| `generation-failed` | Toha or a template hook failed while writing a new workspace. The folder holds what was written. |
| `not-applied` | Toha reported that it wrote nothing. The workspace is unchanged. |
| `upgrade-failed` | Toha failed while applying an upgrade. Files written before the failure remain; the state before the upgrade is on the backup branch, and the message names its commit. Recovery is manual. |
| `merge-conflicted` | The upgrade wrote conflict markers. Resolve them in the named files, then **Rebuild**. |
| `backup-failed` | A commit or push of the workspace failed. The message says what was changed: before an upgrade, nothing; after it, the devcontainer was not rebuilt. Check the workspace repository, that Menagerie's SSH key may push to it, and the Wyrwood binding. |
| `initialize-command-failed` | The devcontainer's `initializeCommand` failed. The container was not started. |
| `registry-credentials-unavailable` | A registry credential could not be read. The pull or build did not start. See below. |
| `operation-failed` | Any other failure of a container operation, such as a failed build or pull; the message holds the cause. |
| `internal-error` | A defect in Menagerie. Report it with the container's log. |

A job that never ends keeps its workspace busy, because jobs have no time
limit. Restart Menagerie to end it: the workspace folder stays as the job
left it.

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

## Requests the API refuses

| Kind | Meaning |
| --- | --- |
| `workspace-busy` | The workspace already runs a job; the answer names it. |
| `unknown-template` | The template id is not in the configuration's list. |
| `invalid-workspace-name` | The name cannot be a folder directly in the workspace root. |
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
