// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"
	"testing"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

// validData is a Machine Class that should pass validation, used as the
// starting point for the rejection cases below.
func validData() data.Data {
	value := data.Data{
		Cloud:   data.Ref{ID: 1},
		Group:   data.Ref{Name: "Talos"},
		Layout:  data.Ref{ID: 7},
		Plan:    data.Ref{Name: "Custom"},
		Network: data.Ref{ID: 12},
	}

	applyDefaults(&value)

	return value
}

func TestValidateProviderDataAcceptsMinimalConfig(t *testing.T) {
	if err := validateProviderData(validData()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateProviderDataRejects(t *testing.T) {
	for _, test := range []struct {
		name        string
		mutate      func(*data.Data)
		wantErrPart string
	}{
		{"missing cloud", func(d *data.Data) { d.Cloud = data.Ref{} }, "cloud"},
		{"missing group", func(d *data.Data) { d.Group = data.Ref{} }, "group"},
		{"missing layout", func(d *data.Data) { d.Layout = data.Ref{} }, "layout"},
		{"missing plan", func(d *data.Data) { d.Plan = data.Ref{} }, "plan"},
		{"missing network", func(d *data.Data) { d.Network = data.Ref{} }, "network"},
		{"bad architecture", func(d *data.Data) { d.Architecture = "arm64" }, "architecture"},
		{"bad image format", func(d *data.Data) { d.ImageFormat = "vhd" }, "image_format"},
		{"negative cores", func(d *data.Data) { d.Cores = -1 }, "cores"},
		{"tiny memory", func(d *data.Data) { d.Memory = 512 }, "memory"},
		{"tiny disk", func(d *data.Data) { d.DiskSize = 1 }, "disk_size"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := validData()
			test.mutate(&value)

			err := validateProviderData(value)
			if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
				t.Fatalf("expected error containing %q, got %v", test.wantErrPart, err)
			}
		})
	}
}

// Zero means "let the service plan decide", so it must not be rejected the way
// an implausible non-zero value is.
func TestValidateProviderDataAllowsUnsetSizing(t *testing.T) {
	value := validData()
	value.Cores = 0
	value.Memory = 0
	value.DiskSize = 0

	if err := validateProviderData(value); err != nil {
		t.Fatalf("unexpected error for unset sizing: %v", err)
	}
}

func TestApplyDefaults(t *testing.T) {
	var value data.Data

	applyDefaults(&value)

	if value.Architecture != "amd64" {
		t.Errorf("architecture = %q, want amd64", value.Architecture)
	}

	if value.ImageFormat != imageFormatQcow2 {
		t.Errorf("image_format = %q, want %q", value.ImageFormat, imageFormatQcow2)
	}

	// os_type is deliberately not defaulted. Sending one requires resolving it
	// to an id first, and Morpheus does not require the field at all.
	if value.OSType != "" {
		t.Errorf("os_type = %q, want it left unset", value.OSType)
	}

	// instance_type_code is deliberately not defaulted. The layout supplies the
	// instance type, and the previous default guessed at a code ("vm") that is
	// not guaranteed to exist on any given appliance.
	if value.InstanceTypeCode != "" {
		t.Errorf("instance_type_code = %q, want it left unset", value.InstanceTypeCode)
	}
}

// An explicitly chosen instance type must survive defaulting untouched.
func TestApplyDefaultsLeavesExplicitInstanceType(t *testing.T) {
	value := data.Data{InstanceType: data.Ref{ID: 5}}

	applyDefaults(&value)

	if value.InstanceType.ID != 5 {
		t.Errorf("instance_type.id = %d, want 5", value.InstanceType.ID)
	}

	if value.InstanceTypeCode != "" {
		t.Errorf("instance_type_code = %q, want empty when instance_type is set", value.InstanceTypeCode)
	}
}

func TestApplyDefaultsPreservesExplicitValues(t *testing.T) {
	value := data.Data{
		Architecture:     "amd64",
		ImageFormat:      imageFormatRaw,
		OSType:           "ubuntu.22.04",
		InstanceTypeCode: "custom",
	}

	applyDefaults(&value)

	if value.ImageFormat != imageFormatRaw {
		t.Errorf("image_format = %q, want %q", value.ImageFormat, imageFormatRaw)
	}

	if value.OSType != "ubuntu.22.04" {
		t.Errorf("os_type = %q, want ubuntu.22.04", value.OSType)
	}

	if value.InstanceTypeCode != "custom" {
		t.Errorf("instance_type_code = %q, want custom", value.InstanceTypeCode)
	}
}

func TestRefIsZero(t *testing.T) {
	if !(data.Ref{}).IsZero() {
		t.Error("empty ref should be zero")
	}

	if (data.Ref{ID: 1}).IsZero() {
		t.Error("ref with an id should not be zero")
	}

	if (data.Ref{Name: "x"}).IsZero() {
		t.Error("ref with a name should not be zero")
	}
}

func TestDescribeOptions(t *testing.T) {
	if got := describeOptions(nil); got != "none exist" {
		t.Errorf("describeOptions(nil) = %q", got)
	}

	got := describeOptions([]NamedObject{
		{ID: 2, Name: "Beta"},
		{ID: 1, Name: "Alpha", Code: "alpha"},
	})

	if !strings.Contains(got, `"Alpha" (id 1, code "alpha")`) {
		t.Errorf("missing coded entry in %q", got)
	}

	if !strings.Contains(got, `"Beta" (id 2)`) {
		t.Errorf("missing uncoded entry in %q", got)
	}
}

// A long listing is truncated so one bad Machine Class field cannot bury the
// actual error in hundreds of names.
func TestDescribeOptionsTruncates(t *testing.T) {
	objects := make([]NamedObject, 0, 40)
	for i := range 40 {
		objects = append(objects, NamedObject{ID: i + 1, Name: string(rune('a'+i%26)) + itoa(i)})
	}

	got := describeOptions(objects)
	if !strings.Contains(got, "and 15 more") {
		t.Errorf("expected truncation notice, got %q", got)
	}
}
