---
menagerie: minor
---

Add a delete operation. **Delete** on a workspace's row asks for its name, stops the devcontainer, commits and backs up, removes its Wyrwood binding, containers and Xolotl key, then removes its socket folder and folder. A failed delete can be retried. Ignored files are not backed up and are lost.
