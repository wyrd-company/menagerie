# Deployment example

`compose.yml` runs Menagerie and its Docker proxy, Sockguard, as two
containers on a Linux host with Docker. `config.yml` is Menagerie's
configuration, `sockguard.yml` the proxy's, and `sockguard-exceptions/`
holds the workspace exceptions file the operator writes. Copy this folder
and edit `config.yml` and `sockguard.yml`.
[Installation](../docs/install.md) explains each mount and setting, the
Docker proxy, the identity provider, and the Wyrwood binding; create the
containers, bind Menagerie in Wyrwood, then start them:

```sh
docker compose create
docker compose up -d
```

[Configuration](../docs/configuration.md) describes every key in
`config.yml`.
