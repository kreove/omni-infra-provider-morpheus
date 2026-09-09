// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"
	"testing"
)

const (
	testSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	testVersion   = "v1.11.0"
)

func TestBuildTalosImageReference(t *testing.T) {
	for _, test := range []struct {
		name        string
		format      string
		wantURL     string
		wantErrPart string
	}{
		{
			name:    "qcow2 is fetched uncompressed",
			format:  imageFormatQcow2,
			wantURL: "https://factory.talos.dev/image/" + testSchematic + "/" + testVersion + "/nocloud-amd64.qcow2",
		},
		{
			name:    "raw is fetched as xz",
			format:  imageFormatRaw,
			wantURL: "https://factory.talos.dev/image/" + testSchematic + "/" + testVersion + "/nocloud-amd64.raw.xz",
		},
		{
			name:        "unknown format is rejected",
			format:      "vmdk",
			wantErrPart: "unsupported image format",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			imageURL, cacheName, err := buildTalosImageReference(
				"https://factory.talos.dev", testSchematic, testVersion, "amd64", test.format,
			)

			if test.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
					t.Fatalf("expected error containing %q, got %v", test.wantErrPart, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if imageURL != test.wantURL {
				t.Errorf("URL = %q, want %q", imageURL, test.wantURL)
			}

			if !strings.HasPrefix(cacheName, imageCachePrefix) {
				t.Errorf("cache name %q does not start with %q", cacheName, imageCachePrefix)
			}
		})
	}
}

// The cache is keyed by name, so two images that differ in any input must not
// collide -- a collision would boot a machine from the wrong Talos build.
func TestBuildTalosImageReferenceCacheNamesAreDistinct(t *testing.T) {
	seen := map[string]string{}

	for _, input := range []struct {
		schematic, version, arch, format string
	}{
		{testSchematic, testVersion, "amd64", imageFormatQcow2},
		{testSchematic, testVersion, "amd64", imageFormatRaw},
		{testSchematic, "v1.12.0", "amd64", imageFormatQcow2},
		{strings.Repeat("a", 64), testVersion, "amd64", imageFormatQcow2},
	} {
		_, cacheName, err := buildTalosImageReference(
			"https://factory.talos.dev", input.schematic, input.version, input.arch, input.format,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if previous, ok := seen[cacheName]; ok {
			t.Fatalf("cache name %q collides between %v and %s", cacheName, input, previous)
		}

		seen[cacheName] = input.version + "/" + input.format
	}
}

// The same inputs must always produce the same name, or a restarted provider
// re-imports an image it already has.
func TestBuildTalosImageReferenceIsStable(t *testing.T) {
	_, first, err := buildTalosImageReference("https://factory.talos.dev", testSchematic, testVersion, "amd64", imageFormatQcow2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, second, err := buildTalosImageReference("https://factory.talos.dev", testSchematic, testVersion, "amd64", imageFormatQcow2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if first != second {
		t.Errorf("cache name is not stable: %q then %q", first, second)
	}
}

func TestBuildTalosImageReferenceRejectsBadInput(t *testing.T) {
	for _, test := range []struct {
		name                                   string
		base, schematic, version, arch, format string
		wantErrPart                            string
	}{
		{"no schematic", "https://factory.talos.dev", "", testVersion, "amd64", imageFormatQcow2, "schematic"},
		{"no version", "https://factory.talos.dev", testSchematic, "", "amd64", imageFormatQcow2, "Talos version"},
		{"no architecture", "https://factory.talos.dev", testSchematic, testVersion, "", imageFormatQcow2, "architecture"},
		{"bad scheme", "ftp://factory.talos.dev", testSchematic, testVersion, "amd64", imageFormatQcow2, "HTTP or HTTPS"},
		{"no host", "https://", testSchematic, testVersion, "amd64", imageFormatQcow2, "no host"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := buildTalosImageReference(test.base, test.schematic, test.version, test.arch, test.format)
			if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
				t.Fatalf("expected error containing %q, got %v", test.wantErrPart, err)
			}
		})
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
