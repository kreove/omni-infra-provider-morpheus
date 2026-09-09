# Images and system extensions

## Who decides what

Omni owns the Talos version and the system extensions. The Machine Class owns *where* a VM is built, not *what* it runs.

That split is deliberate: it means upgrading Talos or adding an extension is a cluster-level change in Omni, and the provider follows along without a Machine Class edit or a restart.

## How an image is produced

1. Omni resolves the cluster's Talos version and extensions into an **Image Factory schematic ID**.
2. The provider builds an artifact URL from that schematic:

   ```
   <factory>/image/<schematic>/<talos version>/nocloud-amd64.qcow2
   ```

3. It downloads that artifact, decompressing it when the format is compressed.
4. It creates a Morpheus virtual image and streams the file into it.
5. It waits for Morpheus to finish processing, then provisions from it.

Steps 2–5 happen once per unique combination of schematic, version, architecture and format. Everything after that is a cache hit.

## The cache name

```
omni-talos-<first 12 bytes of sha256(image url), hex>
```

The URL already encodes every input that changes the image, so hashing it gives a name that is stable across restarts and cannot collide between different images. Both properties are covered by tests — a collision would boot a machine from the wrong Talos build, which is not a failure you would notice quickly.

The name is opaque, so each image's **description** records the URL it came from. That is what tells you which Talos version and schematic a cached image represents.

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

**Point at your own Image Factory.** Set `TALOS_IMAGE_FACTORY_BASE_URL` to a self-hosted instance. Everything else is unchanged.

**Pin an image already in Morpheus.** Set `image` in the Machine Class and the provider imports nothing:

```yaml
image:
  name: talos-custom
```

It must still be a Talos NoCloud image — the join config is delivered through the NoCloud datasource, and an image without it will boot to nothing useful. With a pinned image you are also opting out of Omni's control over extensions and version: the image is used exactly as-is, whatever Omni thinks the cluster should be running.
