# Omni Infrastructure Provider for Morpheus

A community infrastructure provider that lets [Sidero Omni](https://docs.siderolabs.com/omni/) create, scale, and delete [Talos Linux](https://www.talos.dev/) virtual machines on [HPE Morpheus VM Essentials](https://www.hpe.com/us/en/hpe-morpheus-vm-essentials.html) (MVM).

> [!IMPORTANT]
> This project is community maintained and is not an official Sidero Labs or HPE product. It is an **alpha release**, ported from [omni-infra-provider-vergeos](https://github.com/kreove/omni-infra-provider-vergeos) and [omni-infra-provider-xoa](https://github.com/kreove/omni-infra-provider-xoa).
>
> It has been exercised against a live Morpheus appliance with an MVM cloud, covering the full machine lifecycle: cluster creation, scale up and down, machine deletion, Talos and Kubernetes upgrades, and cluster deletion. Provider restart with machines in flight has not been observed. See [Compatibility and limitations](docs/compatibility.md) for what is covered and what is not.
>
> One genuine incompatibility was found and worked around: Morpheus does not pass cloud-init user data to the guest, so the provider serves the Talos config itself over HTTP. That needs `NOCLOUD_SERVER_URL` set and the port reachable from the VM network.

## Features

- Dynamic VM provisioning from Omni Machine Requests
- Clean scale-up, scale-down, and deprovisioning
- Automatic Talos Image Factory downloads, decompressed and imported into Morpheus by the provider
- Image caching by Talos version, architecture, and Omni schematic, reused as a clonable virtual image
- Omni-controlled system extensions and Talos versions
- Optional use of an existing Morpheus virtual image (manual override)
- Every Morpheus object addressable by name or by numeric ID
- Talos config delivered over HTTP from the provider, bypassing Morpheus's cloud-init templating
- Docker Compose and Kubernetes deployment examples
- API-token or username/password authentication to Morpheus

## How it works

```mermaid
flowchart LR
    A[Omni Machine Request] --> B[Morpheus provider]
    B --> C{Cached virtual image?}
    C -- No --> D[Provider downloads Image Factory image, decompresses if needed, uploads it into Morpheus]
    C -- Yes --> E[Provision instance from the cached image]
    D --> E
    E --> F[Attach NIC on the selected network]
    F --> G[VM's SMBIOS serial points at the provider's NoCloud server]
    G --> H[Boot Talos VM]
    H --> I[Talos fetches its join config from the provider over HTTP]
    I --> J[Machine connects to Omni]
```

Omni selects the Talos version and resolves the applicable system extensions into an Image Factory schematic. The provider converts that schematic into an image URL, downloads it, decompresses it when the requested format is compressed, and uploads it into Morpheus as a virtual image with a deterministic, content-derived name. Every later machine needing the same Talos version, architecture and schematic reuses that cached image.

Each machine is then provisioned as a Morpheus instance from that image. The join config is **not** handed to Morpheus to deliver: Morpheus renders its own cloud-config and folds user data into that document's `runcmd` list, which Talos discards silently. Instead the provider serves the config itself, and sets the guest's SMBIOS serial to `ds=nocloud-net;s=<provider URL>` so Talos fetches it over HTTP. This is per VM, so every machine still boots the same cached image. See [Compatibility](docs/compatibility.md#1-cloud-init-user-data-passthrough-confirmed-broken-worked-around).

When Omni no longer needs a machine, the provider powers the instance off and deletes it along with its volumes. Cached virtual images are retained for reuse.

## Requirements

- An Omni instance with administrator access
- A Morpheus appliance managing an MVM (KVM) cloud, reachable from the provider container
- Docker or Kubernetes to run the provider
- A dedicated Morpheus API token (or username/password)
- DNS and HTTPS access from the provider container to the configured Talos Image Factory
- Scratch disk space in the provider container for staging images (a few hundred MB for qcow2, a few GB for raw)
- Network access from provisioned Talos VMs to the Omni endpoints required by your deployment
- Network access from provisioned Talos VMs to the provider itself, which serves them their Talos config (default port `9080`)
- `amd64` virtualization hosts

The provider currently supports `amd64` only.

## Quick start

### 1. Register the provider in Omni

The service-account name must match the provider ID. The default provider ID is `morpheus`.

```bash
omnictl infraprovider create morpheus
```

Save the returned `OMNI_ENDPOINT` and `OMNI_SERVICE_ACCOUNT_KEY` values. **The key is shown only at creation and cannot be read back later from the Omni UI** — if you lose it, run `omnictl infraprovider renewkey morpheus` to issue a new one.

### 2. Create a Morpheus account for the provider

Create a dedicated Morpheus user with permission to provision instances and manage virtual images, then either generate an API token for it under **user settings → API access**, or use its username and password.

### 3. Configure and run

```bash
cp deploy/example.env deploy/.env
```

Fill in `OMNI_ENDPOINT`, `OMNI_SERVICE_ACCOUNT_KEY`, `MORPHEUS_ENDPOINT`, either `MORPHEUS_TOKEN` or `MORPHEUS_USERNAME`/`MORPHEUS_PASSWORD`, and `NOCLOUD_SERVER_URL` — the address **provisioned machines** reach the provider on, which is where they fetch their Talos config. Then:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

`PROVIDER_IMAGE` must name an explicit released version: releases are prereleases while this provider is alpha, so no `:latest` tag is published.

See [Installation](docs/installation.md) for Kubernetes and for building from source.

### 4. Create a Machine Class

In Omni, create a Machine Class backed by this provider and give it provider data describing where to build VMs. Every Morpheus object can be named instead of numbered:

```yaml
cloud:
  name: hvm.example.com
group:
  name: Homelab
layout:
  name: Single HVM
plan:
  name: 4 CPU, 4GB Memory
network:
  name: Compute VLAN 110

architecture: amd64
image_format: qcow2
```

If a name does not resolve, the provider fails the Machine Request with an error listing every object it *could* have matched, with IDs — so a wrong name is a copy-and-paste fix rather than an API hunt.

See [Configuration](docs/configuration.md) for the full schema, and [`examples/`](examples/) for complete files.

## Documentation

- [Installation](docs/installation.md)
- [Configuration](docs/configuration.md)
- [Usage](docs/usage.md)
- [Images and system extensions](docs/images-and-extensions.md)
- [Architecture](docs/architecture.md)
- [Compatibility and limitations](docs/compatibility.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Development](docs/development.md)

## License

[MPL-2.0](LICENSE).
