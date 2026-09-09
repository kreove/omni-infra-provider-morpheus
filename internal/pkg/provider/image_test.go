// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"
	"testing"

	"github.com/siderolabs/omni/client/pkg/imagefactory"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

const testSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

func TestMediaSpecFor(t *testing.T) {
	for _, test := range []struct {
		name        string
		format      string
		wantFormat  string
		wantErrPart string
	}{
		// qcow2 is native for MVM and is served uncompressed.
		{"qcow2", imageFormatQcow2, "qcow2", ""},
		// The factory only publishes raw as xz; the provider decompresses it.
		{"raw maps to raw.xz", imageFormatRaw, "raw.xz", ""},
		{"unknown format is rejected", "vmdk", "", "unsupported image format"},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerData := validData()
			providerData.ImageFormat = test.format

			spec, err := mediaSpecFor(providerData)

			if test.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
					t.Fatalf("expected error containing %q, got %v", test.wantErrPart, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if spec.Format != test.wantFormat {
				t.Errorf("Format = %q, want %q", spec.Format, test.wantFormat)
			}

			if spec.Kind != imagefactory.InstallationMediaKindDisk {
				t.Errorf("Kind = %q, want a disk image", spec.Kind)
			}

			// NoCloud is what makes Talos read its config from the drive
			// Morpheus writes; any other platform would ignore it.
			if spec.Platform != talosPlatform {
				t.Errorf("Platform = %q, want %q", spec.Platform, talosPlatform)
			}

			if spec.Architecture != "amd64" {
				t.Errorf("Architecture = %q, want amd64", spec.Architecture)
			}
		})
	}
}

// The medium is fetched by a goroutine that outlives the step which requested
// the URL, so the default token lifetime -- which assumes an immediate fetch --
// is not enough.
func TestMediaSpecForRequestsALongDownloadToken(t *testing.T) {
	spec, err := mediaSpecFor(validData())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if spec.DownloadTokenTTL < imageUploadTimeout {
		t.Errorf("DownloadTokenTTL = %s, want at least the upload timeout %s", spec.DownloadTokenTTL, imageUploadTimeout)
	}
}

// The spec must validate against the image factory's own rules, or the medium
// is rejected at resolve time rather than here.
func TestMediaSpecForIsValid(t *testing.T) {
	for _, format := range []string{imageFormatQcow2, imageFormatRaw} {
		providerData := validData()
		providerData.ImageFormat = format

		spec, err := mediaSpecFor(providerData)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if err = spec.Validate(); err != nil {
			t.Errorf("format %q produced an invalid media spec: %v", format, err)
		}
	}
}

// The name must come from StorageKey, never the URL: the URL can carry
// credentials or a download token, so a name derived from it would change when
// those rotate and orphan the image already stored under the old name.
func TestCacheNameForUsesStorageKey(t *testing.T) {
	media := imagefactory.InstallationMedia{
		StorageKey:  "abc123",
		URL:         "https://factory.example.com/image/x/y/z?token=secret",
		SchematicID: testSchematic,
	}

	got := cacheNameFor(media)

	if got != imageCachePrefix+"abc123" {
		t.Errorf("cacheNameFor() = %q, want %q", got, imageCachePrefix+"abc123")
	}

	if strings.Contains(got, "secret") || strings.Contains(got, "token") {
		t.Errorf("cache name %q leaks the download URL", got)
	}
}

// Two media that differ must not share a name, or a machine could be built
// from the wrong Talos image.
func TestCacheNameForIsDistinctPerMedium(t *testing.T) {
	first := cacheNameFor(imagefactory.InstallationMedia{StorageKey: "aaa"})
	second := cacheNameFor(imagefactory.InstallationMedia{StorageKey: "bbb"})

	if first == second {
		t.Errorf("distinct media share the cache name %q", first)
	}
}

// The same medium must always produce the same name, or a restarted provider
// re-imports an image it already has.
func TestCacheNameForIsStable(t *testing.T) {
	media := imagefactory.InstallationMedia{StorageKey: "stable-key"}

	if cacheNameFor(media) != cacheNameFor(media) {
		t.Error("cache name is not stable across calls")
	}
}

// The description is what tells an operator which Talos build a digest-named
// image actually holds.
func TestDescribeImage(t *testing.T) {
	providerData := validData()

	got := describeImage(imageSource{
		schematicID:  testSchematic,
		talosVersion: "v1.11.0",
		url:          "https://factory.example.com/image?token=secret",
	}, providerData)

	for _, want := range []string{"v1.11.0", "amd64", imageFormatQcow2, testSchematic} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q is missing %q", got, want)
		}
	}

	// The URL can carry credentials and must not be persisted into Morpheus.
	if strings.Contains(got, "secret") || strings.Contains(got, "http") {
		t.Errorf("description %q leaks the download URL", got)
	}
}

func TestIsImageReady(t *testing.T) {
	for _, test := range []struct {
		name  string
		image *VirtualImage
		want  bool
	}{
		{"nil", nil, false},
		{"no id", &VirtualImage{Status: "Active"}, false},
		{"active", &VirtualImage{ID: 1, Status: "Active"}, true},
		{"active lowercase", &VirtualImage{ID: 1, Status: "active"}, true},
		{"still saving", &VirtualImage{ID: 1, Status: "Saving"}, false},
		{"converting", &VirtualImage{ID: 1, Status: "Converting"}, false},
		{"failed", &VirtualImage{ID: 1, Status: "Failed"}, false},
		// Older appliances leave the status empty once the upload lands, so a
		// non-zero size stands in as evidence the file arrived.
		{"empty status with size", &VirtualImage{ID: 1, RawSize: 1024}, true},
		{"empty status without size", &VirtualImage{ID: 1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isImageReady(test.image); got != test.want {
				t.Errorf("isImageReady() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestIsImageFailed(t *testing.T) {
	if !isImageFailed(&VirtualImage{ID: 1, Status: "Failed"}) {
		t.Error("expected Failed to be reported as failed")
	}

	if isImageFailed(&VirtualImage{ID: 1, Status: "Active"}) {
		t.Error("expected Active not to be reported as failed")
	}

	if isImageFailed(nil) {
		t.Error("expected nil not to be reported as failed")
	}
}

func TestParseImageID(t *testing.T) {
	id, err := parseImageID(" 42 ")
	if err != nil || id != 42 {
		t.Fatalf("parseImageID(\" 42 \") = %d, %v; want 42, nil", id, err)
	}

	for _, bad := range []string{"", "0", "-1", "abc"} {
		if _, err = parseImageID(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

// A pinned image bypasses the Image Factory entirely, so an unresolvable one
// must fail loudly rather than silently falling back to an import.
func TestMediaSpecForRejectsEmptyArchitecture(t *testing.T) {
	providerData := data.Data{ImageFormat: imageFormatQcow2}

	spec, err := mediaSpecFor(providerData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err = spec.Validate(); err == nil {
		t.Error("expected an empty architecture to fail media spec validation")
	}
}
