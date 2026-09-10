# Images and system extensions

## Who decides what

Omni owns the Talos version and the system extensions. The Machine Class owns *where* a VM is built, not *what* it runs.

That split is deliberate: it means upgrading Talos or adding an extension is a cluster-level change in Omni, and the provider follows along without a Machine Class edit or a restart.

## How an image is produced

1. Omni resolves the cluster's Talos version and extensions into an **Image Factory schematic**.
2. The provider asks Omni for the installation medium it wants — a NoCloud disk image, in the configured architecture and format. Omni ensures the schematic exists on its image factory and returns a URL, any headers needed to fetch it, the schematic ID, and a storage key.
3. The provider downloads that medium, decompressing it when the format is compressed.
4. It creates a Morpheus virtual image and streams the file into it.
5. It waits for Morpheus to finish processing, then provisions from it.

Steps 2–5 happen once per unique medium. Everything after that is a cache hit.

The provider asks for a medium by description rather than building a factory URL itself. That keeps it working against a factory that authenticates downloads, and against future changes to how the factory spells a filename — neither of which a hand-built URL survives.

## The cache name

```
omni-talos-<StorageKey>
```

`StorageKey` is Omni's identifier for the medium. It is stable across restarts and distinct per medium, and — unlike the download URL — it survives a credential rotation. A name derived from the URL would change whenever a token in it did, orphaning the image already stored under the old name.

The name is opaque, so each image's **description** records the Talos version, architecture, format and schematic ID. That is what tells you which build a cached image represents. The URL is not recorded, because it can carry credentials.

## Formats

| `image_format` | Artifact | Transfer | Decompressed by |
| --- | --- | --- | --- |
| `qcow2` (default) | `nocloud-amd64.qcow2` | ~100 MB | nobody — served uncompressed |
| `raw` | `nocloud-amd64.raw.xz` | ~80 MB | the provider, to >1 GB |

`qcow2` is native for MVM/KVM and is the right default. `raw` exists for appliances that reject qcow2 uploads; it costs far more scratch space and a much longer upload.

Either way the provider stages the image on its own filesystem before uploading, so it needs room in `/tmp`. Both deployment examples provision this.

## Kernel arguments

The provider adds these to every schematic it requests:

```
console=ttyS0,38400n8 console=tty0
```

`tty0` is last on purpose: the last `console=` argument owns `/dev/console`. A KVM guest gets a serial port only if the layout provides one, and without `tty0` every message after early boot — including a kernel panic — would be written to a device that may not exist, leaving the Morpheus console blank and the failure invisible.

## Air-gapped and custom images

Two options.

**Point Omni at your own Image Factory.** The provider has no Image Factory setting of its own: it asks Omni for the medium it wants, and Omni returns a URL plus any headers needed to fetch it. A self-hosted or authenticated factory is configured in Omni, and the provider follows automatically — including sending whatever authentication headers Omni returns.

The provider container must still reach the factory over HTTPS and trust its certificate, since it performs the download itself.

**Pin an image already in Morpheus.** Set `image` in the Machine Class and the provider imports nothing:

```yaml
image:
  name: talos-custom
```

It must still be a Talos NoCloud image — the join config is delivered through the NoCloud datasource, and an image without it will boot to nothing useful. With a pinned image you are also opting out of Omni's control over extensions and version: the image is used exactly as-is, whatever Omni thinks the cluster should be running.
