# Menagerie

Menagerie manages workspaces and fleets of development containers through a web
UI. It runs as a regular container and can create a workspace from a template,
start its devcontainer, apply template updates, and rebuild the container.

Menagerie incorporates [Toha](https://github.com/wyrd-company/toha) for template
interviews and updates, and
[devcontainer-builder](https://github.com/wyrd-company/devcontainer-builder)
for building and running devcontainers.
