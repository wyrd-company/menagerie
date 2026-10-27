---
menagerie: minor
---

A devcontainer definition reads only the variables of Menagerie's environment that the new `local-env` setting lists, through `${localEnv:NAME}`, Compose interpolation and `initializeCommand`. Without `local-env` it reads none: a definition that used another variable now gets its default value, and the job log names the variable. Add the variables a definition needs to `local-env`.
