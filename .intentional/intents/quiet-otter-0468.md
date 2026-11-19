---
menagerie: minor
menagerie-docs: patch
---

Menagerie finds new versions of each registered template by itself, when it starts and every `template-versions.every` (an hour by default), and on request with **Check versions**, with no restart. The Templates screen shows each template's newest found version and its last check, and a failed check keeps the versions found before. A template is upgraded to a version a check found, chosen from a list, and a moved tag refuses creates and upgrades until the template is upgraded to that version again. Each workspace shows the version last applied to it, an unknown one, and whether it needs an upgrade. An upgrade compares that stored version, and one whose version is unknown goes on only when the current version descends from its applied commit. Apply branch refuses a branch whose applied record would move the workspace backward. The first start of this release records each workspace's applied version from its applied record.
