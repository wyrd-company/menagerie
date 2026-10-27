---
menagerie: minor
---

Template hooks and initializeCommand run in throwaway containers that mount only the workspace folder, so they cannot reach the Docker socket, the Wyrwood sockets or Menagerie's environment. Hooks run in a template's runner-image, initializeCommand in the global runner-image, and both default to Menagerie's own image; hook output the template does not capture now appears in the job console. A job cancelled at a time limit removes its running hook containers before it ends.
