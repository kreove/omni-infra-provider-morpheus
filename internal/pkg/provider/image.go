// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/omni/client/pkg/imagefactory"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/ulikunitz/xz"
	"go.uber.org/zap"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/resources"
)

const (
	imageCachePrefix = "omni-talos-"

	// talosPlatform is the Talos platform this provider provisions. NoCloud is
	// what makes the guest read its machine config from the config drive
	// Morpheus writes.
	talosPlatform = "nocloud"

	// imageBuildTimeout bounds the whole download, decompress and upload
	// cycle. A Talos raw image decompresses to well over a gigabyte and the
	// upload crosses the operator's network twice, so this is generous.
	imageBuildTimeout = 90 * time.Minute

	// imageUploadTimeout bounds just the upload leg.
	imageUploadTimeout = 60 * time.Minute

	// imageDownloadTokenTTL is how long the provider asks the image factory
	// download URL to stay valid.
	//
	// It has to outlast the download, which runs detached from the step that
	// requested the URL. Omni's default assumes the fetch happens immediately,
	// which is not true here: the download starts promptly but a large image
	// on a slow link can still be in flight much later.
	imageDownloadTokenTTL = 60 * time.Minute

	// imageReadyTimeout bounds the wait for Morpheus to finish processing an
	// uploaded image.
	imageReadyTimeout = 30 * time.Minute

	imageReadyPollInterval = 15 * time.Second
)

// imageBuild tracks an in-progress or finished image import for one
// deterministic cache name.
//
// Builds run detached so ensureTalosImage returns immediately and the
// provisioning framework retries on its own schedule, rather than holding a
// single step open for the lifetime of a multi-gigabyte transfer.
type imageBuild struct {
	mu      sync.Mutex
	done    bool
	imageID int
	err     error
}

// imageSource is everything a detached import needs.
//
// It is copied out of the provision context on purpose. The context belongs to
// the step that created it, and the import outlives that step.
type imageSource struct {
	url          string
	headers      http.Header
	schematicID  string
	talosVersion string
}

// ensureTalosImage resolves a manually pinned Morpheus virtual image, or
// downloads, imports and caches the Talos image this machine needs. The
// returned boolean is false while an import is still running.
func (p *Provisioner) ensureTalosImage(
	ctx context.Context,
	logger *zap.Logger,
	pctx provision.Context[*resources.Machine],
	providerData data.Data,
) (int, bool, error) {
	if !providerData.Image.IsZero() {
		image, err := p.resolveExistingImage(ctx, providerData.Image)
		if err != nil {
			return 0, false, err
		}

		return image.ID, true, nil
	}

	spec, err := mediaSpecFor(providerData)
	if err != nil {
		return 0, false, err
	}

	// Omni owns the schematic upload and knows how its image factory spells a
	// medium, so the provider asks for one by description rather than building
	// a factory URL itself. This also keeps working against a factory that
	// authenticates downloads, which a hand-built URL would not.
	media, err := pctx.EnsureInstallationMedia(
		ctx,
		logger,
		spec,
		// Keep a serial console for hosts that offer one, but leave tty0 last
		// so it owns /dev/console. Morpheus VM Essentials gives a KVM guest a
		// VNC console by default and a serial port only when the layout asks
		// for one; without tty0 every message after early boot goes to a
		// device that may not exist, leaving the Morpheus console blank and a
		// boot failure invisible.
		provision.WithExtraKernelArgs("console=ttyS0,38400n8", "console=tty0"),
		provision.WithoutConnectionParams(),
	)
	if err != nil {
		return 0, false, fmt.Errorf("failed to resolve Talos installation media: %w", err)
	}

	pctx.State.TypedSpec().Value.Schematic = media.SchematicID
	pctx.State.TypedSpec().Value.TalosVersion = pctx.GetTalosVersion()

	source := imageSource{
		url:          media.URL,
		headers:      media.Headers,
		schematicID:  media.SchematicID,
		talosVersion: pctx.GetTalosVersion(),
	}

	return p.ensureCachedImage(ctx, logger, providerData, source, cacheNameFor(media))
}

// cacheNameFor derives the Morpheus virtual image name for a medium.
//
// StorageKey exists for exactly this: it identifies the medium and changes only
// when the medium does. The URL must not be used instead -- it can carry
// credentials or a download token, so a name derived from it would change
// whenever those rotate and orphan the image already stored under the old name.
func cacheNameFor(media imagefactory.InstallationMedia) string {
	return imageCachePrefix + media.StorageKey
}

// mediaSpecFor describes the installation medium a Machine Class asks for.
func mediaSpecFor(providerData data.Data) (provision.MediaSpec, error) {
	var format string

	switch providerData.ImageFormat {
	case imageFormatQcow2:
		format = "qcow2"
	case imageFormatRaw:
		// The factory publishes raw only as xz; the provider decompresses it.
		format = "raw.xz"
	default:
		return provision.MediaSpec{}, fmt.Errorf("unsupported image format %q", providerData.ImageFormat)
	}

	return provision.MediaSpec{
		MediaSpec: imagefactory.MediaSpec{
			Kind:         imagefactory.InstallationMediaKindDisk,
			Platform:     talosPlatform,
			Architecture: providerData.Architecture,
			Format:       format,
		},
		DownloadTokenTTL: imageDownloadTokenTTL,
	}, nil
}

// resolveExistingImage looks up an operator-supplied virtual image.
func (p *Provisioner) resolveExistingImage(ctx context.Context, ref data.Ref) (*VirtualImage, error) {
	if ref.ID > 0 {
		image, err := p.client.GetVirtualImage(ctx, ref.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve Morpheus virtual image %d: %w", ref.ID, err)
		}

		return image, nil
	}

	name := strings.TrimSpace(ref.Name)

	images, err := p.client.ListVirtualImagesByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve Morpheus virtual image %q: %w", name, err)
	}

	switch len(images) {
	case 1:
		return &images[0], nil
	case 0:
		return nil, fmt.Errorf("Morpheus virtual image %q does not exist", name)
	default:
		return nil, fmt.Errorf("multiple Morpheus virtual images are named %q; set image.id instead", name)
	}
}

// ensureCachedImage returns the cached image for cacheName, starting an import
// if it is not there yet.
func (p *Provisioner) ensureCachedImage(
	ctx context.Context,
	logger *zap.Logger,
	providerData data.Data,
	source imageSource,
	cacheName string,
) (int, bool, error) {
	buildAny, loaded := p.imageBuilds.LoadOrStore(cacheName, &imageBuild{})

	build, ok := buildAny.(*imageBuild)
	if !ok {
		return 0, false, fmt.Errorf("invalid internal image build state for %q", cacheName)
	}

	if !loaded {
		// First time this provider process has seen this image. The cache
		// lives in Morpheus, not in memory, so a restarted provider must look
		// there before deciding to import anything.
		images, err := p.client.ListVirtualImagesByName(ctx, cacheName)
		if err != nil {
			p.imageBuilds.Delete(cacheName)

			return 0, false, fmt.Errorf("failed to inspect Morpheus image cache: %w", err)
		}

		switch {
		case len(images) > 1:
			p.imageBuilds.Delete(cacheName)

			return 0, false, fmt.Errorf("multiple Morpheus virtual images are named %q", cacheName)
		case len(images) == 1:
			build.mu.Lock()
			build.done = true
			build.imageID = images[0].ID
			build.mu.Unlock()
		default:
			// The media URL is deliberately absent from this line: it can
			// carry credentials or a download token.
			logger.Info(
				"starting Talos image import",
				zap.String("name", cacheName),
				zap.String("schematic", source.schematicID),
				zap.String("talos_version", source.talosVersion),
			)

			go p.runImageBuild(providerData, source, cacheName, build)
		}
	}

	build.mu.Lock()
	defer build.mu.Unlock()

	if build.err != nil {
		err := build.err
		// Drop the failed attempt so a later Machine Request retries the
		// import instead of being stuck behind it forever.
		p.imageBuilds.Delete(cacheName)

		return 0, false, err
	}

	if !build.done {
		return 0, false, nil
	}

	return build.imageID, true, nil
}

// runImageBuild performs the import and records the outcome on build.
func (p *Provisioner) runImageBuild(providerData data.Data, source imageSource, cacheName string, build *imageBuild) {
	// Detached from the request context on purpose: the provisioning step that
	// started this returns immediately, and cancelling its context must not
	// abort an import other machines are already waiting on.
	ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
	defer cancel()

	imageID, err := p.importImage(ctx, providerData, source, cacheName)

	build.mu.Lock()
	defer build.mu.Unlock()

	if err != nil {
		build.err = err

		return
	}

	build.imageID = imageID
	build.done = true
}

func (p *Provisioner) importImage(
	ctx context.Context,
	providerData data.Data,
	source imageSource,
	cacheName string,
) (int, error) {
	localPath, err := downloadImage(ctx, source, providerData.ImageFormat)
	if err != nil {
		return 0, fmt.Errorf("failed to download Talos image: %w", err)
	}

	defer os.Remove(localPath)

	osTypeID := 0

	// Sent only when configured, and only ever as a reference to an existing
	// record. See buildVirtualImagePayload.
	if providerData.OSType != "" {
		osType, oerr := p.resolveOSType(ctx, providerData.OSType)
		if oerr != nil {
			return 0, oerr
		}

		osTypeID = osType.ID
	}

	payload := buildVirtualImagePayload(cacheName, source, providerData, osTypeID)

	image, err := p.client.CreateVirtualImage(ctx, payload)
	if err != nil {
		// Two Machine Requests can race to create the same cached image, and
		// separate provider replicas share the Morpheus-side cache. Re-read by
		// the deterministic name before treating creation as failed.
		existing, listErr := p.client.ListVirtualImagesByName(ctx, cacheName)
		if listErr == nil && len(existing) == 1 {
			return existing[0].ID, nil
		}

		return 0, fmt.Errorf("failed to create Morpheus virtual image %q: %w", cacheName, err)
	}

	filename := cacheName + "." + providerData.ImageFormat

	if err = p.client.UploadVirtualImageFile(ctx, image.ID, filename, localPath, imageUploadTimeout); err != nil {
		// An image record with no file is worse than no record: the name
		// lookup finds it on the next reconcile and every machine provisions
		// from an empty image. Remove it so the import is retried cleanly.
		if deleteErr := p.client.DeleteVirtualImage(ctx, image.ID); deleteErr != nil {
			return 0, fmt.Errorf(
				"failed to upload Talos image into Morpheus: %w (the incomplete virtual image %d could not be removed either: %v)",
				err, image.ID, deleteErr,
			)
		}

		return 0, fmt.Errorf("failed to upload Talos image into Morpheus: %w", err)
	}

	if err = p.waitForImageReady(ctx, image.ID); err != nil {
		return 0, err
	}

	return image.ID, nil
}

// buildVirtualImagePayload renders the virtual image record for a cached image.
func buildVirtualImagePayload(
	cacheName string,
	source imageSource,
	providerData data.Data,
	osTypeID int,
) map[string]any {
	payload := map[string]any{
		"name":      cacheName,
		"imageType": providerData.ImageFormat,
		// Talos reads its machine config from the NoCloud datasource, so
		// Morpheus must deliver user-data through a config drive rather than
		// trying to reach the guest over SSH, which Talos does not serve.
		"isCloudInit": true,
		// The Morpheus agent is a package installed into the guest. Talos has
		// no package manager and no shell, so an agent install can only fail
		// and hold provisioning open until it times out.
		"installAgent":    false,
		"virtioSupported": true,
		"visibility":      "private",
		// The cache name is a digest and says nothing to a human. Record what
		// the image actually is, so an operator deciding whether a cached
		// image is still needed can tell. The source URL is deliberately not
		// recorded: it can carry credentials.
		"description": describeImage(source, providerData),
	}

	// Only ever a reference to an existing record. A nested {"code": ...}
	// makes Morpheus bind the object as a *new* OS type and validate it as
	// one, which fails with "code must be unique; name is required; platform
	// is required" -- the code being already taken by the very entry that was
	// meant to be selected. Morpheus treats the field as optional on a virtual
	// image, so omitting it is a valid choice rather than a workaround.
	if osTypeID > 0 {
		payload["osType"] = map[string]any{"id": osTypeID}
	}

	if providerData.UEFI != nil {
		payload["uefi"] = *providerData.UEFI
	}

	return payload
}

// resolveOSType looks up a library OS type by name or code.
func (p *Provisioner) resolveOSType(ctx context.Context, nameOrCode string) (NamedObject, error) {
	osTypes, err := p.client.ListOSTypes(ctx)
	if err != nil {
		return NamedObject{}, fmt.Errorf("failed to list Morpheus OS types: %w", err)
	}

	return matchRef(data.Ref{Name: nameOrCode}, osTypes, "os_type")
}

// describeImage renders the human-readable description stored on a cached image.
func describeImage(source imageSource, providerData data.Data) string {
	return fmt.Sprintf(
		"Talos %s %s (%s), schematic %s, managed by Sidero Omni",
		source.talosVersion,
		providerData.Architecture,
		providerData.ImageFormat,
		source.schematicID,
	)
}

// waitForImageReady blocks until Morpheus has finished processing the upload.
func (p *Provisioner) waitForImageReady(ctx context.Context, id int) error {
	deadline := time.Now().Add(imageReadyTimeout)

	for {
		image, err := p.client.GetVirtualImage(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to check Morpheus virtual image %d: %w", id, err)
		}

		switch {
		case isImageFailed(image):
			return fmt.Errorf("Morpheus reported virtual image %d as failed (status %q)", id, image.Status)
		case isImageReady(image):
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"Morpheus virtual image %d was still %q after %s",
				id, image.Status, imageReadyTimeout,
			)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(imageReadyPollInterval):
		}
	}
}

// isImageReady reports whether Morpheus has finished processing an image.
//
// Morpheus spells the active status differently across versions and cloud
// types, and older appliances leave it empty on a completed upload, so a
// non-empty size is accepted as evidence on its own rather than matching an
// exhaustive list of status strings.
func isImageReady(image *VirtualImage) bool {
	if image == nil || image.ID < 1 {
		return false
	}

	status := strings.ToLower(strings.TrimSpace(image.Status))

	switch status {
	case "active", "complete", "completed", "":
		return image.RawSize > 0 || status != ""
	default:
		return false
	}
}

func isImageFailed(image *VirtualImage) bool {
	if image == nil {
		return false
	}

	return strings.EqualFold(strings.TrimSpace(image.Status), "failed")
}

// downloadImage streams the installation medium to a scratch file,
// decompressing it when the requested format arrives compressed.
func downloadImage(ctx context.Context, source imageSource, format string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.url, nil)
	if err != nil {
		return "", err
	}

	// A factory that authenticates downloads returns them here. They are sent
	// whenever present rather than decided from configuration, because the
	// same factory may authenticate by header or inside the URL depending on
	// how Omni is set up.
	for key, values := range source.headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The URL is not included: it can carry a download token.
		return "", fmt.Errorf("unexpected HTTP status %d from the image factory", resp.StatusCode)
	}

	var reader io.Reader = resp.Body

	// The factory publishes qcow2 uncompressed but only ships raw as raw.xz,
	// so only the raw path needs decompressing.
	if format == imageFormatRaw {
		xzReader, xerr := xz.NewReader(resp.Body)
		if xerr != nil {
			return "", fmt.Errorf("failed to initialize xz decompression: %w", xerr)
		}

		reader = xzReader
	}

	out, err := os.CreateTemp("", "omni-talos-*."+format)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err = io.Copy(out, reader); err != nil {
		os.Remove(out.Name())

		return "", fmt.Errorf("failed to write image: %w", err)
	}

	return out.Name(), nil
}

// parseImageID converts a stored image ID back to an int.
func parseImageID(value string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid Morpheus virtual image ID %q", value)
	}

	return id, nil
}
