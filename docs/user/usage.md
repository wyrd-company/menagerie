---
title: Usage
order: 4
product: menagerie
relationships:
  references:
    - workspace
    - workspace-root
    - workspace-api
---

Sign in at Menagerie's address with your identity provider. Members of
`menagerie_managers` see the workspace list; anyone else sees a page that
says they are not a manager, and can change nothing.

## The workspace list

Every folder in the workspace root is a row, with its container's state:
**Running**, **Stopped**, or **No container** for a folder whose
devcontainer was never created. Containers that belong to no folder in the
root do not appear. **Refresh** reads the folders and containers again.

Each row has **Stop**, **Start**, **Rebuild**, **Apply branch** and
**Upgrade**. A commit,
from the [schedule or the workspace itself](#commit-from-inside-a-workspace),
shows on its row as **committing**. Rebuild asks
for confirmation, then removes the container and creates it again from the
workspace's definition. While a job runs on a workspace, its row is busy and
its actions wait: a workspace runs one job at a time. Other workspaces stay
usable.

**Jobs since Menagerie started** lists every job and its outcome. Open one to
see its console. Jobs live in memory, so the list starts empty after a
restart.

![The workspace list after a rebuild](assets/list.png)

## Create a workspace

Choose **New workspace**, pick a template and enter the workspace's name,
which becomes its folder. **Begin interview** asks the template's questions.
Answers that the template refuses come back marked on their questions, with
your answers kept, so you can correct them and send again. Nothing is
written until the interview is complete.

The job console then shows each phase:

1. `initialize-repository`: the folder becomes a Git repository.
2. `generate`: Toha writes the files and runs the template's hooks, each in
   a throwaway container that sees only the workspace folder. Output a hook
   does not capture appears in the console, each line prefixed `hook: `.
3. `commit` and `backup`: the first commit, pushed to the workspace's own
   orphan branch in the workspace repository.
4. `initialize-command`: the devcontainer's `initializeCommand`, in
   throwaway containers that see only the workspace folder, with only the
   variables of [`local-env`](configuration.md#local-environment) beside
   the [runner image](configuration.md#runner-images)'s own.
5. `build` when the definition builds an image, then `up`: the devcontainer
   starts.

The answers are kept in the workspace, in `.toha.json`.

![A template interview](assets/create-interview.png)

## Upgrade a workspace

**Upgrade** offers the approved templates. Pick the newer version and choose
**Begin upgrade**. Menagerie first commits any local changes and backs the
workspace up, so the state before the upgrade is on its branch. Then it asks
only the questions the new version adds; earlier answers come from
`.toha.json`. Toha applies the new version to a separate checkout of the
workspace and runs the template's hooks there, each in a throwaway
container that sees only that checkout; then it merges the result into
the files, keeping your edits, and Menagerie commits, backs up and
rebuilds the devcontainer.

When the new version and a local edit change the same lines, the upgrade
writes conflict markers, commits and backs them up, and stops before the
rebuild with `merge-conflicted`. Resolve the conflicts in the workspace,
then choose **Rebuild**.

![An upgrade that asks one new question](assets/upgrade-interview.png)

## Apply a reviewed branch

A change to a workspace, such as its `.devcontainer/` content, can be
proposed and reviewed as a pull request, then brought into the workspace
with **Apply branch**:

1. Push the change to a branch of the workspace repository and open a pull
   request whose base is the workspace's branch.
2. Review it, but do not merge it on the forge: the workspace's next backup
   would replace the merge.
3. In the workspace's row, choose **Apply branch**, enter the pull
   request's head branch, for example `review/devcontainer`, and choose
   **Apply**.

The job console shows each phase:

1. `fetch`: Menagerie fetches only that branch from the workspace
   repository.
2. `backup-local-changes` and `backup`: it commits every local change, as
   a [commit](#commit-from-inside-a-workspace) does, and backs the
   workspace up. When the branch is the workspace's own, it commits but
   does not push, and there is no `backup` phase here, so that the
   branch's commits stay on the workspace repository.
3. `plan`: Menagerie works out the apply without writing anything. When
   the branch extends the workspace's branch, the workspace fast-forwards
   to it. When both have changed, Menagerie merges them file by file and
   commits the merge as `Apply <branch> to workspace <workspace>`. A plan
   that cannot apply fails here and leaves the devcontainer running.
4. `stop`: the devcontainer stops, with every service of its Compose
   project, so that nothing writes to the workspace while the apply does.
   What it wrote before it stopped is committed (and backed up, for
   another branch than the workspace's own), and the plan is made again.
5. `apply`: Menagerie refuses while another running container mounts the
   workspace folder (the console names it by its short id), then writes
   only the files that change.
6. `backup`, then the rebuild of the devcontainer.

When the apply is refused after the stop, nothing is applied and the
devcontainer returns to its state before the job: Menagerie starts it
again if it was running. Whenever a job leaves the devcontainer stopped,
its console ends with "the devcontainer is stopped; start or rebuild
it".

The backup after the apply pushes the branch's commits to the workspace's
branch, and the forge marks the pull request merged. A branch already in
the workspace's history changes nothing and does not rebuild; when it is
the workspace's own branch, the job still backs the workspace up.

Menagerie writes only through real folders of the workspace, never
through a symbolic link, and moves the files it replaces to
`.menagerie-apply-…` names first. When the job fails as `apply-failed`,
the devcontainer stays stopped, and the message and console name each
file still under such a name with its path.

The job fails, with nothing applied, when:

| Kind | Meaning |
| --- | --- |
| `branch-not-found` | The workspace repository has no branch of that name. Nothing changed, local changes included. |
| `fetch-failed` | The workspace repository could not be reached. Nothing changed. |
| `not-applicable` | The branch cannot be applied cleanly. The console lists the paths, one `not applicable:` line each, and the message says why: both sides changed the same file, the branch and the workspace share no history (or more than one merge base), an untracked or ignored file, folder or link, or a nested repository, is where the branch writes, the change has a submodule or a path Menagerie does not write (a `.git` part in any spelling, `git~1`, an absolute path, a tab or a newline), a file changed while the job ran (the change is kept), another running container mounts the workspace folder, the workspace repository is shallow, or part of the branch's history is missing. Local changes are committed, and backed up unless the branch is the workspace's own. Rebase the branch on the workspace's branch, or move the files in the way, and apply again. |
| `backup-failed` | The backup before or after the apply failed. The message says which; a failed backup after the apply leaves the applied files and does not rebuild. |
| `apply-failed` | Writing the files failed partway. The branch did not move; the message names the commit to recover from by hand. |

A merge never writes conflict markers: a file both sides changed is
`not-applicable`. The applied record `.toha.json` is merged like any other
file. Prefer a branch that is not the workspace's own. When an apply of
the workspace's own branch is refused, the workspace repository keeps the
pushed commits, and the message names the fetched commit, until the next
backup of the workspace replaces them.

## Backups

Each workspace is backed up to a branch named after its folder, created
without a parent and replaced by force at every backup. Commits pushed to
that branch from elsewhere survive only when
[Apply branch](#apply-a-reviewed-branch) brings them in before the next
backup. To restore a workspace, clone its branch into a folder in the
workspace root by hand; Menagerie lists it like any other folder.

## Commit from inside a workspace

When Menagerie has [workspace sockets](configuration.md#workspace-sockets),
every devcontainer has `/run/menagerie/bin/workspace-commit`. It commits
every change of the workspace, as `git add --all` and a commit would, and
backs the workspace's branch up:

```sh
/run/menagerie/bin/workspace-commit -m "feat: add the build scripts"
```

It waits for the job and prints its id, the commit, and whether the backup
pushed it. A commit job also appears in **Jobs since Menagerie started**,
with its whole log. The exit status says what happened:

| Status | Meaning |
| --- | --- |
| 0 | Committed (or nothing to commit) and backed up. |
| 1 | The job failed, as `commit-failed` or `backup-failed` for example. The job's console has the detail. |
| 2 | Usage error, or a message that is blank, longer than 4096 bytes, or holds a NUL. |
| 3 | The workspace has a job in progress, committed less than the minimum interval ago, or Menagerie is shutting down. It prints when to retry. |
| 4 | The socket cannot be reached: the container was created before Menagerie had workspace sockets, or Menagerie is not running. |

A container gets the socket when it is created or rebuilt, so a workspace
whose container was created before sockets were configured needs one
**Rebuild**; a **Start** keeps that container as it is, and its console
says the socket attaches at the next rebuild. A **Start** that recreates
the container for a changed definition also leaves the socket out until
the next rebuild. A restart of
Menagerie does not: the socket comes back in the same mounted folder.
`MENAGERIE_WORKSPACE_SOCKET` names the socket inside the devcontainer.

With [automatic commits](configuration.md#automatic-commits), Menagerie
also commits a workspace with changes on a schedule, with the message
`chore(menagerie): automatic commit`.

## Stop, start and rebuild

Start runs `initializeCommand` before it starts the container, as rebuild
does. When `initializeCommand` fails, the job fails with
`initialize-command-failed` and the container stays stopped.

## Time limits

The job console shows how long a job has run. When the configuration sets
[job time limits](configuration.md#job-time-limits), it also shows the
limit that ends first and the time left; while a job waits for your
answers the limit pauses. A job past its limit fails with `timed-out`, and
the message names the phase and the limit.
