# Deployment example

`compose.yml` runs Menagerie as one container on a Linux host with Docker,
and `config.yml` is its configuration. Copy this folder and edit both.
[Installation](../docs/user/install.md) explains each mount and setting, the
identity provider, and the Wyrwood binding. Create the consumer directory and the
container, bind it in Wyrwood, then start it:

```sh
sudo install -d -o wyrwood -g wyrwood -m 0700 /run/wyrwood/containers/menagerie
docker compose create
# Bind the container "menagerie" in Wyrwood now.
docker compose up -d
```

[Configuration](../docs/user/configuration.md) describes every key in
`config.yml`.
