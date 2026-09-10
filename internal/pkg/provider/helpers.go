// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

// mvmProviderType is the Morpheus provider type for Morpheus VM Manager, the
// KVM hypervisor behind HPE VM Essentials. It is the only provider type this
// provider targets.
const mvmProviderType = "mvm"

// imageFormat values accepted in a Machine Class.
const (
	imageFormatQcow2 = "qcow2"
	imageFormatRaw   = "raw"
)

func decodeNamedList(raw map[string]json.RawMessage, key, path string) ([]NamedObject, error) {
	payload, ok := raw[key]
	if !ok {
		// Morpheus answers an unauthenticated request with a login page rather
		// than a 401 in some configurations, so a missing key is more often a
		// misdirected request than a schema change.
		return nil, fmt.Errorf("Morpheus response for %s has no %q field; check that the endpoint and credentials are correct", path, key)
	}

	var objects []NamedObject
	if err := json.Unmarshal(payload, &objects); err != nil {
		return nil, fmt.Errorf("failed to decode %q from %s: %w", key, path, err)
	}

	return objects, nil
}

func sortStrings(values []string) {
	sort.Strings(values)
}

// describeOptions renders what a lookup could have matched.
//
// Every object this provider resolves lives on the operator's own appliance
// with IDs that are theirs alone, so "layout X not found" is not actionable on
// its own. Listing the candidates turns a failed Machine Class into a
// copy-and-paste fix.
func describeOptions(objects []NamedObject) string {
	if len(objects) == 0 {
		return "none exist"
	}

	descriptions := make([]string, 0, len(objects))

	for _, object := range objects {
		if object.Code != "" {
			descriptions = append(descriptions, fmt.Sprintf("%q (id %d, code %q)", object.Name, object.ID, object.Code))

			continue
		}

		descriptions = append(descriptions, fmt.Sprintf("%q (id %d)", object.Name, object.ID))
	}

	sortStrings(descriptions)

	const limit = 25
	if len(descriptions) > limit {
		remaining := len(descriptions) - limit
		descriptions = descriptions[:limit]

		return strings.Join(descriptions, ", ") + fmt.Sprintf(" and %d more", remaining)
	}

	return strings.Join(descriptions, ", ")
}

func validateProviderData(value data.Data) error {
	if value.Cloud.IsZero() {
		return fmt.Errorf("cloud must set either id or name")
	}

	if value.Group.IsZero() {
		return fmt.Errorf("group must set either id or name")
	}

	if value.Layout.IsZero() {
		return fmt.Errorf("layout must set either id or name")
	}

	if value.Plan.IsZero() {
		return fmt.Errorf("plan must set either id or name")
	}

	if value.Network.IsZero() {
		return fmt.Errorf("network must set either id or name")
	}

	if value.Architecture != "amd64" {
		return fmt.Errorf("architecture %q is not supported by this alpha provider; use amd64", value.Architecture)
	}

	if value.ImageFormat != imageFormatQcow2 && value.ImageFormat != imageFormatRaw {
		return fmt.Errorf("image_format %q is not supported; use %q or %q", value.ImageFormat, imageFormatQcow2, imageFormatRaw)
	}

	if value.Cores < 0 {
		return fmt.Errorf("cores cannot be negative")
	}

	if value.Memory > math.MaxInt {
		return fmt.Errorf("memory value is too large")
	}

	// Zero means "use the service plan", so only a set-but-implausible value is
	// rejected. Talos needs 2 GiB to boot.
	if value.Memory != 0 && value.Memory < 2048 {
		return fmt.Errorf("memory must be at least 2048 MiB when set")
	}

	if value.DiskSize != 0 && value.DiskSize < 5 {
		return fmt.Errorf("disk_size must be at least 5 GiB when set")
	}

	return nil
}

func applyDefaults(value *data.Data) {
	if value.Architecture == "" {
		value.Architecture = "amd64"
	}

	if value.ImageFormat == "" {
		value.ImageFormat = imageFormatQcow2
	}
}
