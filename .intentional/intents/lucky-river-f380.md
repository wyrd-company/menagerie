---
menagerie: major
menagerie-docs: patch
---

Choose workspace policies at creation and edit them on the workspace row. Apply stored selections immediately through Wyrwood 0.7.1 without a rebuild, retain imported identities, and show pending or failed application. Policies a template declares (`customizations.menagerie.wyrwood.policies`, `.values` and `.xolotl`) are no longer read: mark the policies every workspace needs as required, or put them in groups, on the Policies screen before anyone creates a workspace. A create that needs a new Xolotl key fails as `provisioning-unavailable` until `provisioning.xolotl.key-policy` is set, as the key policy no longer comes from a template. Menagerie needs Wyrwood 0.7.1 or later, up from 0.6.1: upgrade the Wyrwood controller and agents first. The configuration guide covers the host's key policy and policy recovery.
