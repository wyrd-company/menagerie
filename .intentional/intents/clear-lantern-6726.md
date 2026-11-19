---
menagerie: major
menagerie-docs: patch
---

Upgrading from 0.4.2 needs steps on the host. Before you pull the image: upgrade the Wyrwood controller and agents to 0.7.1 first, in Wyrwood's upgrade order; back up the `/var/lib/menagerie` volume, as going back needs it; repair a workspace definition that no longer parses; set `provisioning.xolotl.key-policy`; mark as required, or put in groups, the policies your templates used to declare; and make each `templates` entry name a `MAJOR.MINOR.PATCH` tag. After the first start: read the start log for the entries it skipped and register those templates in the interface, then remove the `templates` key; stop sending `templateId` on an upgrade. On a host with provisioning, a new workspace name is 1 to 63 lowercase letters, digits, `_` or `-`, starting with a letter or digit.
