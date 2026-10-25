# Deployment example

`compose.yml` runs Menagerie as one container on a Linux host with Docker,
and `config.yml` is its configuration. Copy this folder and edit both.
[Installation](../docs/install.md) explains each mount and setting, the
identity provider, and the Wyrwood binding; create the container, bind it in
Wyrwood, then start it:

```sh
docker compose create
docker compose up -d
```

[Configuration](../docs/configuration.md) describes every key in
`config.yml`.
