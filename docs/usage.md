# Usage

## Scaling

Omni owns the machine count. Change the size of a control plane or worker pool and the provider reconciles:

- **Scale up** — Omni issues new Machine Requests; the provider provisions instances from the cached image.
- **Scale down** — Omni marks machines for removal; the provider powers off and deletes them along with their volumes.

Cached virtual images are never removed by scaling. They are shared by every machine on the same Talos version, architecture and schematic.

## Changing the Talos version

Change the Talos version on the cluster in Omni. Omni resolves a new schematic, and the provider imports the matching image on first use. Existing machines are upgraded by Omni in place — the new image is used for machines created from then on.

The first machine on a new version waits for the import. Later ones do not.

## Changing system extensions

System extensions are Omni's concern, not the Machine Class's. Add or remove them on the cluster; Omni folds them into the schematic, and the provider treats the result as a new image to import and cache.

See [Images and system extensions](images-and-extensions.md).

## Changing the Machine Class

Provider data is read fresh on every reconcile, so edits take effect without restarting the provider.

Edits apply to **machines created after the change**. Existing instances are not resized or moved — Omni scales by replacing machines, so to roll out a change, scale down and back up.

## Pinning an existing image

To bypass the Image Factory entirely — for an air-gapped appliance, or a hand-built Talos image:

```yaml
image:
  name: talos-custom
```

or by ID:

```yaml
image:
  id: 42
```

The provider uses that image as-is and imports nothing. It must still be a Talos NoCloud image, or the join config will not be read.

## Watching progress

The provider logs one line per state change per machine:

```
starting Talos image import       name=omni-talos-3f2a... url=https://factory.talos.dev/...
created Morpheus instance         name=talos-cp-1 id=118
waiting for Morpheus instance     name=talos-cp-1 id=118 status=provisioning
machine is running                name=talos-cp-1 id=118
```

Errors are also returned to Omni and shown on the Machine Request, so you do not have to read logs to see why a machine is stuck.

## What the provider creates in Morpheus

| Object | Naming | Lifetime |
| --- | --- | --- |
| Instance | The Omni Machine Request ID | Deleted on deprovision |
| Virtual image | `omni-talos-<hash>` | Retained and shared |

Instances are labelled `omni-managed`, so Omni-managed VMs can be told apart from hand-built ones.

## Cleaning up cached images

Cached images are deliberately retained — they are the reason machines after the first provision quickly. If you want to reclaim the space, delete `omni-talos-*` virtual images in Morpheus once no machine depends on them. The provider re-imports on demand.

Each image's description records the Image Factory URL it was built from, which names the Talos version and schematic — the thing you actually need to know when deciding whether a cached image is still wanted.
