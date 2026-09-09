// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/ulikunitz/xz"
	"go.uber.org/zap"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/resources"
)

const (
	imageCachePrefix = "omni-talos-"

	// imageBuildTimeout bounds the whole download, decompress and upload
	// cycle. A Talos raw image decompresses to well over a gigabyte and the
	// upload crosses the operator's network twice, so this is generous.
	imageBuildTimeout = 90 * time.Minute

	// imageUploadTimeout bounds just the upload leg.
	imageUploadTimeout = 60 * time.Minute

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

	imageURL, cacheName, err := buildTalosImageReference(
		p.imageFactoryBaseURL,
		pctx.State.TypedSpec().Value.Schematic,
		pctx.GetTalosVersion(),
		providerData.Architecture,
		providerData.ImageFormat,
	)
	if err != nil {
		return 0, false, err
	}

	return p.ensureCachedImage(ctx, logger, providerData, imageURL, cacheName)
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
	imageURL, cacheName string,
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
			logger.Info(
				"starting Talos image import",
				zap.String("name", cacheName),
				zap.String("url", imageURL),
			)

			go p.runImageBuild(providerData, imageURL, cacheName, build)
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
func (p *Provisioner) runImageBuild(providerData data.Data, imageURL, cacheName string, build *imageBuild) {
	// Detached from the request context on purpose: the provisioning step that
	// started this returns immediately, and cancelling its context must not
	// abort an import other machines are already waiting on.
	ctx, cancel := context.WithTimeout(context.Background(), imageBuildTimeout)
	defer cancel()

	imageID, err := p.importImage(ctx, providerData, imageURL, cacheName)

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
	imageURL, cacheName string,
) (int, error) {
	localPath, err := downloadImage(ctx, imageURL, providerData.ImageFormat)
	if err != nil {
		return 0, fmt.Errorf("failed to download Talos image from %q: %w", imageURL, err)
	}

	defer os.Remove(localPath)

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
		"osType":          map[string]any{"code": providerData.OSType},
		"description": fmt.Sprintf(
			"Talos image managed by Sidero Omni. Imported from %s", imageURL,
		),
	}

	if providerData.UEFI != nil {
		payload["uefi"] = *providerData.UEFI
	}

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

// downloadImage streams the Image Factory artifact to a scratch file,
// decompressing it when the requested format arrives compressed.
func downloadImage(ctx context.Context, imageURL, format string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}

	var source io.Reader = resp.Body

	// The factory publishes qcow2 uncompressed but only ships raw as raw.xz,
	// so only the raw path needs decompressing.
	if format == imageFormatRaw {
		xzReader, xerr := xz.NewReader(resp.Body)
		if xerr != nil {
			return "", fmt.Errorf("failed to initialize xz decompression: %w", xerr)
		}

		source = xzReader
	}

	out, err := os.CreateTemp("", "omni-talos-*."+format)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err = io.Copy(out, source); err != nil {
		os.Remove(out.Name())

		return "", fmt.Errorf("failed to write image: %w", err)
	}

	return out.Name(), nil
}

// buildTalosImageReference derives the Image Factory URL and the deterministic
// cache name for one schematic, version, architecture and format.
func buildTalosImageReference(
	baseURL,
	schematic,
	talosVersion,
	architecture,
	format string,
) (imageURL, cacheName string, err error) {
	if strings.TrimSpace(schematic) == "" {
		return "", "", fmt.Errorf("cannot build Talos image URL without a schematic ID")
	}

	if strings.TrimSpace(talosVersion) == "" {
		return "", "", fmt.Errorf("cannot build Talos image URL without a Talos version")
	}

	if strings.TrimSpace(architecture) == "" {
		return "", "", fmt.Errorf("cannot build Talos image URL without an architecture")
	}

	var artifact string

	switch format {
	case imageFormatQcow2:
		artifact = fmt.Sprintf("nocloud-%s.qcow2", architecture)
	case imageFormatRaw:
		artifact = fmt.Sprintf("nocloud-%s.raw.xz", architecture)
	default:
		return "", "", fmt.Errorf("unsupported image format %q", format)
	}

	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid Image Factory URL %q: %w", baseURL, err)
	}

	if base.Scheme != "https" && base.Scheme != "http" {
		return "", "", fmt.Errorf("Image Factory URL must use HTTP or HTTPS")
	}

	if base.Host == "" {
		return "", "", fmt.Errorf("Image Factory URL %q has no host", baseURL)
	}

	base.Path = path.Join(base.Path, "image", schematic, talosVersion, artifact)
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""

	imageURL = base.String()

	// The name has to be stable for the cache to work and unique per image, so
	// it is derived from the URL, which already encodes schematic, version,
	// architecture and format.
	hash := sha256.Sum256([]byte(imageURL))
	cacheName = imageCachePrefix + hex.EncodeToString(hash[:12])

	return imageURL, cacheName, nil
}

// parseImageID converts a stored image ID back to an int.
func parseImageID(value string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid Morpheus virtual image ID %q", value)
	}

	return id, nil
}
