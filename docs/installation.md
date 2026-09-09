# Installation

## 1. Register an infrastructure provider in Omni

The Omni service-account name must match the provider ID, which defaults to `morpheus`.

```bash
omnictl infraprovider create morpheus
```

This prints `OMNI_ENDPOINT` and `OMNI_SERVICE_ACCOUNT_KEY`. **The key is shown only once.** If you lose it:

```bash
omnictl infraprovider renewkey morpheus
```

You can also create the provider from **Settings → Infra Providers** in the Omni UI, which shows the key once in the creation dialog.

To run under a different ID, pass `--id <name>` to the provider and create the service account under that same name.

## 2. Create a Morpheus account

Create a dedicated Morpheus user for the provider rather than reusing an administrator account. It needs to be able to:

- List clouds, groups, networks, instance types, layouts, service plans and resource pools
- Create, start, stop and delete instances
- Create, upload and delete virtual images

Then either generate an API token for it under **user settings → API access**, or use its username and password. A token is preferable: it can be revoked independently, and it does not require the account to have a local password.

## 3. Run the provider

### Docker Compose

```bash
cp deploy/example.env deploy/.env
```

Fill in the values, then:

```bash
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml logs -f
```

The Compose file mounts a 4 GB tmpfs at `/tmp`. The provider stages Talos images there on the way into Morpheus — a raw image decompresses to over a gigabyte, so this is not optional if you use `image_format: raw`.

### Kubernetes

Edit the Secret in `deploy/kubernetes.yaml`, then:

```bash
kubectl apply -f deploy/kubernetes.yaml
kubectl -n omni-infra-provider-morpheus logs -f deploy/omni-infra-provider-morpheus
```

The manifest runs a single replica with `strategy: Recreate`. Run only one — nothing serializes instance creation between replicas.

For a real deployment, keep the credentials out of the manifest and use an existing Secret, a sealed secret, or an external secrets operator.

### From source

```bash
make build
./_out/omni-infra-provider-morpheus --help
```

If the build fails with `undefined: http2.TrailerPrefix`, use the Go version the module targets:

```bash
GOTOOLCHAIN=go1.26.7 make build
```

See [Troubleshooting](troubleshooting.md#build-problems) for why.

## 4. Confirm registration

The provider appears in Omni under **Settings → Infra Providers** once it connects, with the name, description and icon it reports. If it does not appear, check the provider logs for authentication errors.

## 5. Create a Machine Class

Create a Machine Class in Omni backed by this provider, and give it provider data. Omni renders the form from the schema the provider reports.

See [Configuration](configuration.md#machine-class-provider-data) for the full schema and [`examples/machineclass-provider-data.yaml`](../examples/machineclass-provider-data.yaml) for a complete file.

A minimal class:

```yaml
cloud:
  name: MVM
group:
  name: Talos
layout:
  name: MVM VM
plan:
  name: Custom
network:
  name: vlan100
architecture: amd64
```

## 6. Create a cluster

Point a cluster at the Machine Class, either from the Omni UI or with a template:

```bash
omnictl cluster template sync -f examples/cluster-template.yaml
```

The first machine will take noticeably longer than the rest: it waits for the Talos image to be downloaded and imported into Morpheus. Every later machine on the same Talos version and schematic reuses the cached image.

## Upgrading

Pull the new image and restart. The provider holds no local state — everything it needs is in Omni or Morpheus — so a restart is safe at any point. An import interrupted by a restart is simply retried.
