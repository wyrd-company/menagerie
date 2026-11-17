# Menagerie templates and policies mockup

Task 18, board `/workspace/apps/tasks/epics/menagerie-templates-policies`. Review artifact only: **do not merge this branch into the product**. Tasks 20–24 can use the files and judgment as their interface reference. The orchestrator brings the completed mockup to boblangley; approval is pending.

Based on `epic/templates-and-policies` at `0c3772bb27ef6cd9e050dba640dd896b3a608b0a`, particularly `docs/technical-designs/menagerie.yml` → `screens`, and [#87](https://github.com/wyrd-company/menagerie/issues/87) / [#88](https://github.com/wyrd-company/menagerie/issues/88). The [operator's Q1/Q3/Q6 answers](https://github.com/wyrd-company/menagerie/issues/88#issuecomment-6940881933) apply.

Open `index.html` directly, or serve this directory using `python3 -m http.server 8080`. No build or API is needed. For one portable file, run `python3 pack.py /tmp/menagerie-mockup.html`. The private hosted review URL and its expiry are in task 18's log; it is intentionally absent from the public PR.

The top **Mockup** selector is review tooling, outside the proposed product. It loads 31 fixed states; **Reset** restores the initial fixture. Main navigation preserves the current in-memory changes. Direct links such as `index.html#templates/moved` set up a state on a fresh load. Changes and jobs are simulated and reset on reload. Repository addresses, policy membership, workspace records and results are fixtures, not claims about real accounts or repositories.

## Review surface

- **Workspaces:** running, stopped, no container, no folder, applied/unknown version, needs upgrade, groups needing a choice; inline policy add/remove, required policies, outside grant, applied/pending/failed, and empty-selection outcomes.
- **New workspace:** registered template, workspace name, exactly one per single-select group, at least one per multi-select group, optional policies, validation refusals, interview and completion.
- **Upgrade:** current version of the same template, ask every question again, interview, completion and moved-tag refusal.
- **Templates:** register with Validate/Save, missing definition and bad address, current/newest version, check in progress, failed check retaining earlier versions, moved tag revalidation, forward version update, metadata edit, removal and refusal naming workspaces.
- **Policies:** grant scope, required checkboxes, editable single-/multi-select groups, group add/remove and settings validation.

The #87 flow works through ordinary controls: register a template; create a workspace at 1.0.0; check versions to discover 1.1.0; validate/save the newer template version; upgrade the workspace. The #88 flow refuses a missing group choice, permits creating with bob-ssh, then adding ira-ssh and removing an ordinary policy without a rebuild. An upgrade leaves those policy choices unchanged.

## Decisions kept visible

- Keep Menagerie's incumbent layout, emblem, colors, fonts and native controls. `incumbent.css` is a snapshot of its existing styles, not a proposed shared stylesheet change. Use checkboxes consistently for policy membership and multi-selection, radios for an exactly-one workspace choice.
- Save selections directly; no Review changes, confirmation stage, Cancel action, policy classification beyond required/groups, Xolotl policy marking or identity controls. Errors occur by their controls, not in explanatory notice boxes. Required policies are checked and locked in workspace panels.
- The saved selection and binding outcome stay distinct. An empty selection is stored and sent to Wyrwood; its refusal is shown with the binding unchanged. An unbound workspace's empty selection succeeds without a binding request. There is no Menagerie last-policy guard.
- A missing choice in a newly added group stays visible while unrelated policy edits remain possible. Removing the final choice from a group that already had one is refused.
- A selected policy that left the grant is visible and removable. A create that adds an outside-grant policy is refused before the interview or job. No prototype API runs.
- The incumbent design calls for a commit when an applied version is unknown; this mockup omits a SHA line under the operator's explicit no-SHA-lines standard. Product tasks should follow the operator's judgment on this presentation.

## Verification

`verify.mjs` exercises the mockup in Chromium. It renders every fixture at 1280px and 390px in light and dark, checks one h1 and no horizontal overflow, runs axe WCAG A/AA scans, then exercises the two acceptance flows and nine additional interactions (including keyboard focus). It writes `report.json` with `FAILED` until the entire run succeeds, plus screenshots. This verifies the mockup, not backend implementations.

Run dependencies outside the product checkout, preferably in a disposable sandbox:

```sh
mkdir -p /tmp/mockup-check
cd /tmp/mockup-check
npm init -y
npm install playwright @axe-core/playwright
npx playwright install --with-deps chromium
# Serve this directory separately, then run:
MOCKUP_TEST_DEPS=/tmp/mockup-check/package.json node /path/to/verify.mjs http://127.0.0.1:8080/ /tmp/mockup-evidence
```

For repeatable tool versions, the handoff records the exact installed browser/dependency versions. No product build, Go/Rust live test, real grant/binding operation, screen-reader session, or operator approval is claimed by this prototype. The operator judges visual fit and UX.
