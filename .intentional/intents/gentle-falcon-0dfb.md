---
menagerie: minor
---

The template service signs the interview session it hands to the browser. It refuses a session that was altered, is unsigned, or was issued for another workspace, template or flow, with the error `session-tampered`. The signing key is kept as `toha/session-key` in the `/var/lib/menagerie` volume, so interviews continue across a restart of the template service.
