---
menagerie: minor
---

Menagerie keeps what it provisioned in its own data, a SQLite database in the data volume, in place of the definition and the retirement records: a delete removes each Wyrwood binding and Xolotl key Menagerie made for a workspace, also when its definition no longer parses or declares them, and any job that ends with the devcontainer started retires what the definition no longer declares. A binding that Wyrwood answers is outside the operator key's grant stays recorded and fails the job as the new error `binding-outside-grant`. A workspace whose folder is gone and that still holds a binding or a key is listed as `no-folder` and can be deleted, and a create is refused `workspace-exists` while Menagerie holds a workspace of that name. The first start takes over each workspace, its retirement records and what its definition declares, with no step by hand. Back up the `/var/lib/menagerie` volume with the host.
