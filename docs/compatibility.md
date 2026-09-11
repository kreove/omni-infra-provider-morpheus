# Compatibility and limitations

## Status

This provider is **alpha**. It has now been exercised against a live Morpheus appliance (MVM on KVM with Ceph-backed storage), which turned up one genuine incompatibility — cloud-init user data is not passed to the guest, see below — that the provider now works around. Areas other than provisioning and config delivery remain lightly exercised.

What it *is* built on:

- The official [Morpheus Go SDK](https://github.com/gomorpheus/morpheus-go-sdk), for endpoint paths, response envelopes and JSON keys
- The official [Morpheus Terraform provider](https://github.com/gomorpheus/terraform-provider-morpheus), specifically `resource_mvm_instance.go`, for the MVM provisioning payload
- The [Morpheus API reference](https://apidocs.morpheusdata.com/reference/addinstance), for the `config` block and its per-cloud variants
- The [Morpheus CLI](https://github.com/gomorpheus/morpheus-cli), for `servicePlanOptions` semantics and units

Everything the provider sends is unit-tested for shape. None of it has been acknowledged by a real appliance.

## Known-uncertain areas

These are the places most likely to need adjustment on first contact, roughly in order of risk.

### 1. Cloud-init user data passthrough — confirmed broken, worked around

**This was the highest-risk part of the port, and it turned out to be real.** It has since been reproduced against a live appliance and worked around; what follows is what actually happens, because it explains why the provider does something unusual.

Morpheus does **not** pass `config.userData` to the guest. It renders its own `#cloud-config` document and folds whatever the API supplied into that document's `runcmd` list, one YAML line per list entry:

```yaml
#cloud-config
hostname: cluster-05-...
users:
- name: sysadmin
  ...
runcmd:
- 'apiVersion: v1alpha1'
- 'kind: SideroLinkConfig'
- 'apiUrl: https://omni.example.com:8090/?jointoken=...'
- '---'
...
```

Talos then discards the file, because its NoCloud platform explicitly refuses cloud-config:

```go
case bytes.Equal(firstLine, []byte("#cloud-config")):
	// ignore cloud-config, Talos does not support it
	return nil, errors.ErrNoConfigSource
```

This is returned as *"no config source"* rather than an error, so **nothing is logged**: the machine reports that it found the config disk and fetched `user-data`, then boots into maintenance mode with no SideroLink interface and no explanation.

`createUser: false` and `noAgent: true` are both **ignored** on an appliance whose cloud has its agent install mode set to `cloudInit` — the rendered document still contains a login user and a Morpheus agent callback. The API echoes `config.userData` back verbatim when queried, which makes this look like it worked; only the config drive on the hypervisor shows otherwise.

**The workaround.** Talos's NoCloud platform also accepts its datasource over HTTP, selected by options in the guest's SMBIOS system serial number:

```
ds=nocloud-net;s=http://provider:9080/nocloud/<token>/
```

The provider serves the datasource itself and sets that serial per VM through `config.qemuArgs`, which Morpheus passes into the libvirt domain verbatim. A later `-smbios type=1,serial=` overrides the UUID-derived serial libvirt emits from `<sysinfo>`. The machine then fetches its config from the provider and never reads the mangled config drive at all.

Because the serial is a per-VM setting rather than an image property, the schematic is unchanged and the image cache still works — every machine boots the same cached image. See [Configuration](configuration.md#nocloud-server) for the settings this requires.

`config.userData` is still sent, so an appliance that genuinely passes user data through untouched keeps working without the NoCloud server. No such appliance has been observed.

### 2. Layout selection

The layout is the only thing you must name, and it is authoritative: it selects the hypervisor and reports the instance type the provider provisions from.

Nothing is defaulted here. An earlier version defaulted `instance_type_code` to `vm` on the assumption that Morpheus ships a generic "plain VM" type; that was never verified, and HPE's own MVM example pairs *Ubuntu* with *Single KVM VM* instead. Reading the instance type off the layout removed the need to assume anything.

What remains uncertain is only which layouts your appliance actually offers, and whether the MVM one is usable for a Talos image. A wrong or missing layout fails at `ensureTarget` with every candidate listed.

### 3. Sizing overrides

`cores`, `memory` and `disk_size` are sent as `servicePlanOptions` and a root volume, and are only honoured on a service plan that permits custom sizing. On a fixed plan they are ignored. This differs from the VergeOS and Xen Orchestra providers, where those fields were authoritative. See [Configuration](configuration.md#sizing-is-the-service-plans-job).

Memory is stated in MiB in the Machine Class and converted to bytes, which is what Morpheus expects in this field.

### 4. OS type on imported images

`os_type` is unset by default and the field is then omitted, because Morpheus does not require it on a virtual image and the agent behaviour it drives is disabled here anyway.

Set it and the provider resolves the name or code against `/api/library/operating-systems/os-types` and sends the resulting **id**. It must be a reference: a nested `{"code": ...}` makes Morpheus bind the object as a *new* OS type and validate it as one, failing with `code must be unique; name is required; platform is required` — the code being already taken by the entry that was meant to be selected.

### 5. Image readiness

After upload, Morpheus processes an image asynchronously. The provider polls the virtual image until it reports an active status. Morpheus spells that status differently across versions and cloud types, and some versions leave it empty on a completed upload — so a non-empty size is accepted as evidence on its own. If imports hang at "waiting", check what your appliance actually reports in `status`.

### 6. Boot firmware

The Talos nocloud image boots under both BIOS and UEFI, so `uefi` is left unset by default and the platform default applies. If VMs power on and immediately halt without console output, set `uefi: true` (or `false`) explicitly.

## Supported

| | |
| --- | --- |
| Architecture | `amd64` only |
| Morpheus cloud type | MVM (Morpheus VM Manager / HPE VM Essentials) only |
| Image formats | `qcow2` (default), `raw` |
| Authentication | API token, or username and password |
| Provider replicas | One |

## Not supported

- **`arm64`.** The Machine Class schema rejects it. Adding it is mostly a matter of removing the guard and testing.
- **Other Morpheus cloud types.** The `config` block is MVM-specific: `poolProviderType: "mvm"` and `imageId`. vSphere via Morpheus uses `config.template` for the image and adds `vmwareFolderId`; AWS, Azure and GCP differ further. Supporting them means branching the config block per cloud type.
- **Multiple provider replicas.** Nothing serializes instance creation between replicas. The image cache is keyed by a deterministic name and tolerates a race, but instance creation does not. Run one replica.
- **Multiple NICs**, static IP assignment, and instance resize after creation. Omni scales by adding and removing machines, so resize has not been needed.
- **Snapshots and backups.** Out of scope; manage them in Morpheus.

## Version compatibility

Developed against the Morpheus API as documented for 8.x. The endpoints used are long-standing and present well before that, but `/api/options/zonePools` and the MVM provision type require a Morpheus version that supports MVM clouds.
