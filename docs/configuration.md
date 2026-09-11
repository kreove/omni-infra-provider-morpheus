# Configuration

## Provider process

The provider is configured with environment variables or the equivalent flags. Flags win over environment variables.

| Environment variable | Flag | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `OMNI_ENDPOINT` | `--omni-api-endpoint` | yes | | Omni API endpoint |
| `OMNI_SERVICE_ACCOUNT_KEY` | `--omni-service-account-key` | yes | | Key printed by `omnictl infraprovider create` |
| `MORPHEUS_ENDPOINT` (or `MORPHEUS_URL`) | `--morpheus-endpoint` | yes | | Morpheus appliance base URL |
| `MORPHEUS_TOKEN` | `--morpheus-token` | one of | | Morpheus API token |
| `MORPHEUS_USERNAME` | `--morpheus-username` | one of | | Morpheus username |
| `MORPHEUS_PASSWORD` | `--morpheus-password` | one of | | Morpheus password |
| | `--morpheus-insecure-skip-verify` | no | `false` | Skip Morpheus TLS verification |
| | `--id` | no | `morpheus` | Provider ID registered in Omni |
| | `--provider-name` | no | `Morpheus` | Display name in Omni |
| | `--provider-description` | no | | Description shown in Omni |
| | `--insecure-skip-verify` | no | `false` | Skip Omni TLS verification |
| `NOCLOUD_SERVER_URL` | `--nocloud-server-url` | in practice | | Base URL machines fetch their Talos config from |
| `NOCLOUD_SERVER_BIND_ADDRESS` | `--nocloud-server-bind-address` | no | `:9080` | Address the NoCloud server listens on |

> [!NOTE]
> The provider has no Image Factory setting. Omni resolves the installation medium and hands the provider a URL, so the factory — including a self-hosted or authenticated one — is configured in Omni, not here.

Set either `MORPHEUS_TOKEN` or both `MORPHEUS_USERNAME` and `MORPHEUS_PASSWORD`. When both are set, the token is used.

A username and password are exchanged for an OAuth access token at `/oauth/token`, which is cached and renewed automatically. If Morpheus later rejects the token — after an appliance restart, or when a session is revoked — the provider re-authenticates and retries the call once rather than failing until it is restarted.

If you change the provider ID with `--id`, the Omni service account name must match it.

## NoCloud server

Morpheus does not deliver cloud-init user data to the guest — it renders its own `#cloud-config` and folds user data into that document's `runcmd` list, which Talos discards without an error. The provider therefore serves the Talos config itself over HTTP and points each VM at it through the guest's SMBIOS serial number. [Compatibility](compatibility.md#1-cloud-init-user-data-passthrough-confirmed-broken-worked-around) has the detail.

`NOCLOUD_SERVER_URL` is marked "in practice" rather than "yes" because the provider still sends `config.userData`, so an appliance that passes user data through untouched would work without it. No such appliance has been observed; assume it is required.

Two things matter:

- **The URL must be reachable from the machines**, not from the operator or from Omni. Provisioned VMs connect to it directly, on the network the Machine Class puts them on. It is the provider host's address on that network — never `localhost`, and never a name only resolvable elsewhere.
- **The port must be open to the VM network.** The deployment examples publish `9080`; a Kubernetes deployment needs a Service reachable from outside the cluster, since the VMs are not in it.

To verify from a machine on the VM network:

```bash
curl -v http://provider-host:9080/nocloud/test/user-data
```

A `503` is the correct answer — the token is unknown. Anything that times out or is refused means machines will not get their config.

The URL is not baked into the Talos image, so changing it does not invalidate the image cache. It is recorded per VM at creation, so a change only affects machines provisioned afterwards.

### What it serves

Each machine gets a random token, minted before its VM is created and stored in the provider's Omni state. The token addresses that machine's datasource:

```
http://provider-host:9080/nocloud/<token>/user-data
```

The token is the only thing guarding the join config, which carries a credential for registering a machine with Omni. It is visible in the VM's SMBIOS and in the Morpheus instance's QEMU arguments, so treat access to the VM network accordingly. The server serves only these three files and has no other endpoints.

## Machine Class provider data

Every Morpheus object is given as an object with `id`, `name`, or both. **The `id` wins when both are set.** This means a Machine Class can be written readably against names and pinned to IDs later without changing shape.

The `name` field also accepts the object's **code** — the value Morpheus labels *"used as a unique identifier in the API and CLI"*. A name is tried first, then a code, so either works:

```yaml
plan:
  name: 4 CPU, 4GB Memory   # the name
```
```yaml
plan:
  name: kvm-vm-4-4096       # the code, equally valid
```

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cloud` | ref | yes | | Morpheus cloud (zone) to provision into |
| `group` | ref | yes | | Morpheus group (site) that owns the instance |
| `layout` | ref | yes | | Library layout, which selects the hypervisor. Must be an MVM layout — see [What a layout is](#what-a-layout-is) |
| `plan` | ref | yes | | Service plan sizing the instance |
| `network` | ref | yes | | Network for the instance's primary NIC |
| `instance_type` | ref | no | | Narrows layout candidates only; the layout supplies the instance type used |
| `instance_type_code` | string | no | | Same narrowing as `instance_type`, by the type's stable code |
| `resource_pool` | ref | no | | MVM compute pool; Morpheus chooses when unset |
| `datastore` | string | no | | Datastore ID, or `auto` / `autoCluster` |
| `image` | ref | no | | Existing virtual image, bypassing the Image Factory download |
| `image_format` | enum | no | `qcow2` | `qcow2` or `raw` |
| `os_type` | string | no | | Library OS type name or code for imported images; omitted when unset |
| `uefi` | bool | no | | Firmware for imported images; platform default when unset |
| `architecture` | enum | yes | `amd64` | Only `amd64` is supported |
| `cores` | int | no | `0` | Core count override; `0` uses the plan |
| `memory` | int | no | `0` | RAM override in MiB; `0` uses the plan |
| `disk_size` | int | no | `0` | Boot disk override in GiB; `0` uses the plan |

A `ref` is:

```yaml
cloud:
  id: 3                      # optional, wins when set
  name: hvm.example.com    # optional
```

### What a layout is

`layout` is the field most likely to need explaining, because it is the one that decides **which hypervisor actually builds the VM**.

Morpheus's Library nests layouts under instance types, and each layout names a *provision type*:

```
Instance Type  "Morpheus VM"
  └── Layout   "Single HVM"   (provisionType.code: mvm)      <- this provider
  └── Layout   "VMware VM"    (provisionType.code: vmware)
  └── Layout   "Amazon VM"    (provisionType.code: amazon)
```

Same instance type, same plan, same image — pick the vSphere layout instead of the MVM one and Morpheus provisions on vSphere. The layout *is* the choice of platform, which is why it is required and why it cannot be defaulted: nothing else in the Machine Class implies it.

It also has to be an MVM layout specifically. This provider sends `config.poolProviderType: "mvm"` and `config.imageId`, both MVM-specific; pairing them with another provision type gives Morpheus a contradictory request.

To list only the layouts this provider can use:

```bash
curl -sk -H "Authorization: Bearer $MORPHEUS_TOKEN" \
  "$MORPHEUS_ENDPOINT/api/library/layouts?max=200" \
  | jq -r '.instanceTypeLayouts[]
           | select(.provisionType.code == "mvm")
           | "\(.id)\t\(.name)\t(instance type: \(.instanceType.code))"'
```

Then set either form:

```yaml
layout:
  name: Single HVM      # readable
```

```yaml
layout:
  id: 42            # pinned; wins if both are set
```

### The layout supplies the instance type

You do not need to set `instance_type` or `instance_type_code`. Morpheus reports the instance type on every layout, so the provider reads it off the layout you chose. Whatever the `instance type:` column showed above, that layout is selectable.

This matters because instance types are not generic. HPE's own MVM example pairs the instance type *Ubuntu* with the layout *Single KVM VM*, and your appliance may differ again — there is no dependable "plain VM" type to assume. Following the layout to its instance type avoids assuming one.

Both fields remain, as an optional **narrowing** for a single situation: an appliance where the same layout name exists under more than one instance type, making the name ambiguous. Setting either restricts the candidates before matching. The matched layout still supplies the instance type used, so the two can never disagree.

```yaml
# Only needed when a layout name is ambiguous.
instance_type:
  name: Morpheus VM
layout:
  name: Single HVM
```

### The other Morpheus objects, briefly

| Field | What it is |
| --- | --- |
| `cloud` | The Morpheus cloud (called a *zone* in the API) holding the MVM hypervisor. |
| `group` | The Morpheus group (a *site* in the API) that owns the instance. Groups scope visibility and permissions; they do not affect placement. |
| `plan` | The service plan, which sizes the instance. See [Sizing is the service plan's job](#sizing-is-the-service-plans-job). |
| `instance_type` | The Library instance type a layout belongs to. Read off the layout, so you rarely set it — see [The layout supplies the instance type](#the-layout-supplies-the-instance-type). |
| `resource_pool` | The MVM compute pool. Optional; Morpheus picks one when unset. |
| `network` | The network attached to the VM's primary NIC. |

### Sizing is the service plan's job

This is the one place the Morpheus model differs meaningfully from the VergeOS and Xen Orchestra providers, where `cores`, `memory` and `disk_size` were required and authoritative.

Morpheus sizes an instance from its **service plan**. A plan may be fixed (`2 CPU / 8GB Memory`) or allow custom sizing. This provider therefore treats `cores`, `memory` and `disk_size` as *optional overrides*:

- Left at `0`, nothing is sent and the plan decides.
- Set, they are sent as `servicePlanOptions` (`maxCores`, `maxMemory`) and a root volume size.

**Overrides only take effect on a plan that permits custom sizing.** On a fixed plan Morpheus ignores them, and the VM comes up at the plan's size. If you want the Machine Class to control sizing, point `plan` at a custom-sizing plan.

### Finding IDs

```bash
curl -sk -H "Authorization: Bearer $MORPHEUS_TOKEN" \
  "$MORPHEUS_ENDPOINT/api/zones?max=100" | jq '.zones[] | {id, name}'
```

The same pattern works for `/api/groups`, `/api/library/instance-types`, `/api/service-plans` and `/api/networks`. Layouts need the response key `instanceTypeLayouts` and are worth filtering by provision type — see [What a layout is](#what-a-layout-is).

In practice you rarely need any of this. Name a thing wrongly and the provider fails the Machine Request with every candidate listed, name and ID, so the fastest way to discover a value is to guess and read the error.

## Image format

| Format | Factory artifact | Downloaded | Notes |
| --- | --- | --- | --- |
| `qcow2` | `nocloud-amd64.qcow2` | ~100 MB | Native for MVM/KVM; no decompression needed |
| `raw` | `nocloud-amd64.raw.xz` | ~80 MB compressed | Decompressed by the provider to >1 GB before upload |

`qcow2` is the default and the right choice for MVM. Use `raw` only if your appliance rejects qcow2 uploads.

Whichever you pick, the provider needs scratch space in `/tmp` to stage the image: a few hundred megabytes for qcow2, a few gigabytes for raw. The Compose and Kubernetes examples both provision this.
