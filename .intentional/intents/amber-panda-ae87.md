---
menagerie: patch
menagerie-docs: patch
---

A template address on a host with a port takes no ref: in an `https://` or `ssh://` address no `:` may come before an `@`, so a password that holds a `/` is refused before any fetch. Register such a template without its ref, which takes the newest version; the first start skips a `templates` entry of that shape and names it by id.
