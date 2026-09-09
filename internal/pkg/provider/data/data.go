// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package data defines Morpheus MachineClass configuration.
package data

import _ "embed"

// Schema is reported to Omni for MachineClass rendering and validation.
//
//go:embed schema.json
var Schema []byte

// Ref identifies a Morpheus object either by numeric ID or by name.
//
// Morpheus provisioning needs a handful of objects that XCP-ng and VergeOS
// have no equivalent of -- a group, a service plan, an instance type and a
// layout -- and their IDs differ between every Morpheus appliance. Requiring
// IDs makes a Machine Class unportable and forces an operator to go look each
// one up; requiring names makes it break whenever an object is recreated. Both
// are accepted, and the ID wins when they are both set, so a Machine Class can
// be written the readable way and pinned later without changing shape.
type Ref struct {
	ID   int    `yaml:"id,omitempty"`
	Name string `yaml:"name,omitempty"`
}

// IsZero reports whether the reference selects nothing.
func (r Ref) IsZero() bool {
	return r.ID == 0 && r.Name == ""
}

// Data and schema.json must remain in sync.
type Data struct {
	// Cloud is the Morpheus cloud (zone) the instance is provisioned into.
	Cloud Ref `yaml:"cloud"`
	// Group is the Morpheus group (site) that owns the instance.
	Group Ref `yaml:"group"`
	// InstanceType selects the library instance type. Code is accepted in
	// addition to ID and name because instance type codes are stable across
	// appliances in a way IDs are not.
	InstanceType     Ref    `yaml:"instance_type,omitempty"`
	InstanceTypeCode string `yaml:"instance_type_code,omitempty"`
	// Layout is the library layout, which determines the provision type. It
	// must be an MVM layout for this provider.
	Layout Ref `yaml:"layout"`
	// Plan is the service plan that sizes the instance.
	Plan Ref `yaml:"plan"`
	// Network is attached to the instance's primary NIC.
	Network Ref `yaml:"network"`
	// ResourcePool is the MVM compute pool. Optional; Morpheus picks one when
	// it is not set.
	ResourcePool Ref `yaml:"resource_pool,omitempty"`
	// Datastore is the target datastore ID, or "auto"/"autoCluster" to let
	// Morpheus choose. Optional.
	Datastore string `yaml:"datastore,omitempty"`
	// Image optionally pins an existing Morpheus virtual image, bypassing the
	// automatic Image Factory download and cache.
	Image Ref `yaml:"image,omitempty"`
	// ImageFormat selects which Image Factory artifact is downloaded and
	// uploaded. qcow2 is native for MVM and needs no decompression; raw is
	// fetched as raw.xz and decompressed by the provider.
	ImageFormat string `yaml:"image_format,omitempty"`
	// OSType is the Morpheus OS type code recorded on imported images.
	// Morpheus has no Talos entry, so this defaults to a generic Linux.
	OSType string `yaml:"os_type,omitempty"`
	// UEFI sets the firmware on imported images. Left unset, the platform
	// default applies; the Talos nocloud image boots either way.
	UEFI *bool `yaml:"uefi,omitempty"`

	Architecture string `yaml:"architecture"`

	// Cores, Memory and DiskSize are optional overrides. Morpheus sizes an
	// instance from its service plan; these are only sent when set, and only
	// take effect on a plan that permits custom sizing.
	Cores    int    `yaml:"cores,omitempty"`
	Memory   uint64 `yaml:"memory,omitempty"`
	DiskSize int64  `yaml:"disk_size,omitempty"`
}
