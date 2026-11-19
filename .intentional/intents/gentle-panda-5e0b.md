---
menagerie: patch
---

A throwaway container, which runs a template hook, `initializeCommand` or a bind source check, has a two-minute bound on its create, apart from the 30-second bound on removing it. A create that passes it fails with "the Docker daemon did not answer in time while creating the throwaway container; retrying is safe".
