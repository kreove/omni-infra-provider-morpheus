// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

const testJoinConfig = "#!talos\nmachine:\n  install:\n    disk: /dev/vda\n"

func testTarget() *target {
	return &target{
		cloud:        NamedObject{ID: 3, Name: "MVM"},
		group:        NamedObject{ID: 4, Name: "Talos"},
		instanceType: NamedObject{ID: 5, Name: "Morpheus VM", Code: "vm"},
		layout:       NamedObject{ID: 6, Name: "MVM VM", Code: "mvm-1.0"},
		plan:         NamedObject{ID: 7, Name: "Custom", Code: "custom"},
		network:      NamedObject{ID: 8, Name: "vlan100"},
	}
}

func buildTestPayload(t *testing.T, mutate func(*data.Data)) map[string]any {
	t.Helper()

	providerData := validData()
	if mutate != nil {
		mutate(&providerData)
	}

	return buildInstancePayload("talos-cp-1", testJoinConfig, "", 42, testTarget(), providerData)
}

func subMap(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()

	value, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("payload key %q is %T, want a map", key, payload[key])
	}

	return value
}

func TestBuildInstancePayloadTopLevel(t *testing.T) {
	payload := buildTestPayload(t, nil)

	if payload["zoneId"] != 3 {
		t.Errorf("zoneId = %v, want 3", payload["zoneId"])
	}

	instance := subMap(t, payload, "instance")

	if instance["name"] != "talos-cp-1" {
		t.Errorf("name = %v", instance["name"])
	}

	// Morpheus identifies the instance type by code, not by ID, in this field.
	if instance["type"] != "vm" {
		t.Errorf("type = %v, want the instance type code", instance["type"])
	}

	if site := subMap(t, instance, "site"); site["id"] != 4 {
		t.Errorf("site.id = %v, want 4", site["id"])
	}

	for _, key := range []string{"plan", "layout"} {
		nested := subMap(t, instance, key)

		// Morpheus wants all three; sending only the ID has been observed to
		// make it fall back to a default rather than fail.
		for _, field := range []string{"id", "code", "name"} {
			if _, ok := nested[field]; !ok {
				t.Errorf("instance.%s is missing %q", key, field)
			}
		}
	}
}

// Without a NoCloud server the join config has nowhere else to go, so it is
// still handed to Morpheus -- the fallback for an appliance that passes user
// data through untouched.
func TestBuildInstancePayloadCarriesJoinConfigWithoutNoCloud(t *testing.T) {
	config := subMap(t, buildTestPayload(t, nil), "config")

	if config["userData"] != testJoinConfig {
		t.Errorf("userData = %q, want the join config verbatim", config["userData"])
	}
}

// With a NoCloud server the guest reads its config over HTTP and never looks at
// the config drive. Sending user data then delivers nothing, while writing the
// join config -- and the join token in it -- onto a drive readable by anyone
// with access to the instance in Morpheus.
func TestBuildInstancePayloadOmitsJoinConfigWithNoCloud(t *testing.T) {
	serial := "ds=nocloud-net;s=http://10.0.0.5:9080/nocloud/deadbeef/"

	payload := buildInstancePayload("talos-cp-1", testJoinConfig, serial, 42, testTarget(), validData())
	config := subMap(t, payload, "config")

	if _, ok := config["userData"]; ok {
		t.Errorf("userData is sent alongside a NoCloud serial: %v", config["userData"])
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to encode payload: %v", err)
	}

	// The whole payload, not just that one key: the join config must not reach
	// Morpheus by any route once the NoCloud server is delivering it.
	if strings.Contains(string(encoded), "/dev/vda") {
		t.Error("the join config reaches Morpheus despite the NoCloud server")
	}
}

// Talos has no users, no shell and no package manager. Letting Morpheus create
// a login user rewrites the user-data carrying the join config, and the agent
// install can only fail against a guest it cannot log into.
func TestBuildInstancePayloadDisablesGuestCustomization(t *testing.T) {
	config := subMap(t, buildTestPayload(t, nil), "config")

	if config["createUser"] != false {
		t.Errorf("createUser = %v, want false", config["createUser"])
	}

	if config["noAgent"] != true {
		t.Errorf("noAgent = %v, want true", config["noAgent"])
	}
}

// Morpheus does not pass user data through to the guest: it renders its own
// cloud-config and folds config.userData into that document's runcmd list,
// which Talos discards. Pointing the guest's SMBIOS serial at the provider's
// NoCloud server is what actually delivers the config, so the QEMU argument
// carrying it has to survive into the payload intact -- semicolon included.
func TestBuildInstancePayloadCarriesNoCloudSerial(t *testing.T) {
	serial := "ds=nocloud-net;s=http://10.0.0.5:9080/nocloud/deadbeef/"

	payload := buildInstancePayload("talos-cp-1", testJoinConfig, serial, 42, testTarget(), validData())

	qemuArgs, ok := subMap(t, payload, "config")["qemuArgs"].(string)
	if !ok {
		t.Fatalf("config.qemuArgs is missing")
	}

	if qemuArgs != "-smbios type=1,serial="+serial {
		t.Errorf("qemuArgs = %q", qemuArgs)
	}
}

// Without a NoCloud server the guest keeps whatever serial Morpheus assigns,
// and no QEMU override may be sent: an empty serial= would blank the field.
func TestBuildInstancePayloadOmitsQemuArgsWithoutNoCloud(t *testing.T) {
	if _, ok := subMap(t, buildTestPayload(t, nil), "config")["qemuArgs"]; ok {
		t.Error("config.qemuArgs is set without a NoCloud serial")
	}
}

func TestBuildInstancePayloadTargetsMVM(t *testing.T) {
	config := subMap(t, buildTestPayload(t, nil), "config")

	if config["poolProviderType"] != mvmProviderType {
		t.Errorf("poolProviderType = %v, want %q", config["poolProviderType"], mvmProviderType)
	}

	if config["imageId"] != 42 {
		t.Errorf("imageId = %v, want 42", config["imageId"])
	}
}

// A resource pool is optional, and sending a zero ID would pin the instance to
// a pool that does not exist.
func TestBuildInstancePayloadOmitsUnsetResourcePool(t *testing.T) {
	config := subMap(t, buildTestPayload(t, nil), "config")

	if _, ok := config["resourcePoolId"]; ok {
		t.Errorf("resourcePoolId should be absent when no pool is configured")
	}
}

func TestBuildInstancePayloadIncludesResourcePool(t *testing.T) {
	resolved := testTarget()
	resolved.resourcePool = NamedObject{ID: 11, Name: "Pool"}

	payload := buildInstancePayload("talos-cp-1", testJoinConfig, "", 42, resolved, validData())

	if config := subMap(t, payload, "config"); config["resourcePoolId"] != 11 {
		t.Errorf("resourcePoolId = %v, want 11", config["resourcePoolId"])
	}
}

// Morpheus expects a prefixed string here. A bare integer is silently ignored
// and the VM comes up with no network at all.
func TestBuildInstancePayloadNetworkIDIsPrefixed(t *testing.T) {
	payload := buildTestPayload(t, nil)

	interfaces, ok := payload["networkInterfaces"].([]map[string]any)
	if !ok || len(interfaces) != 1 {
		t.Fatalf("networkInterfaces = %v, want one interface", payload["networkInterfaces"])
	}

	network := subMap(t, interfaces[0], "network")
	if network["id"] != "network-8" {
		t.Errorf("network.id = %v, want \"network-8\"", network["id"])
	}
}

func TestBuildInstancePayloadOmitsSizingByDefault(t *testing.T) {
	payload := buildTestPayload(t, nil)

	// Unset sizing must defer to the service plan rather than sending zeros,
	// which Morpheus would take as a request for a zero-core VM.
	if _, ok := payload["servicePlanOptions"]; ok {
		t.Errorf("servicePlanOptions should be absent when sizing is unset")
	}

	if _, ok := payload["volumes"]; ok {
		t.Errorf("volumes should be absent when disk_size is unset")
	}
}

func TestBuildInstancePayloadSizing(t *testing.T) {
	payload := buildTestPayload(t, func(d *data.Data) {
		d.Cores = 4
		d.Memory = 8192
	})

	options := subMap(t, payload, "servicePlanOptions")

	if options["maxCores"] != 4 {
		t.Errorf("maxCores = %v, want 4", options["maxCores"])
	}

	// The Machine Class states memory in MiB to match the other Omni
	// providers; Morpheus takes bytes.
	if options["maxMemory"] != uint64(8192)*1024*1024 {
		t.Errorf("maxMemory = %v, want %d", options["maxMemory"], uint64(8192)*1024*1024)
	}
}

func TestBuildInstancePayloadRootVolume(t *testing.T) {
	payload := buildTestPayload(t, func(d *data.Data) {
		d.DiskSize = 64
		d.Datastore = "auto"
	})

	volumes, ok := payload["volumes"].([]map[string]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("volumes = %v, want one volume", payload["volumes"])
	}

	volume := volumes[0]

	if volume["size"] != int64(64) {
		t.Errorf("size = %v, want 64", volume["size"])
	}

	if volume["rootVolume"] != true {
		t.Errorf("rootVolume = %v, want true", volume["rootVolume"])
	}

	// Morpheus creates a volume only when the ID is this sentinel; anything
	// else is read as a reference to an existing volume.
	if volume["id"] != newVolumeID {
		t.Errorf("id = %v, want %d", volume["id"], newVolumeID)
	}

	if volume["datastoreId"] != "auto" {
		t.Errorf("datastoreId = %v, want auto", volume["datastoreId"])
	}
}

// A datastore can be pinned without resizing the disk.
func TestRootVolumePayloadDatastoreOnly(t *testing.T) {
	volume := rootVolumePayload(data.Data{Datastore: "12"})
	if volume == nil {
		t.Fatal("expected a volume when a datastore is set")
	}

	if _, ok := volume["size"]; ok {
		t.Errorf("size should be absent when disk_size is unset")
	}

	if volume["datastoreId"] != "12" {
		t.Errorf("datastoreId = %v, want \"12\"", volume["datastoreId"])
	}
}

func TestRootVolumePayloadNilWhenUnset(t *testing.T) {
	if volume := rootVolumePayload(data.Data{}); volume != nil {
		t.Errorf("expected nil, got %v", volume)
	}
}

func TestServicePlanOptionsNilWhenUnset(t *testing.T) {
	if options := servicePlanOptions(data.Data{}); options != nil {
		t.Errorf("expected nil, got %v", options)
	}
}

// The payload is sent as JSON, so it must survive encoding -- a value Go can
// hold but encoding/json rejects would fail only at provisioning time.
func TestBuildInstancePayloadIsJSONSerializable(t *testing.T) {
	payload := buildTestPayload(t, func(d *data.Data) {
		d.Cores = 2
		d.Memory = 4096
		d.DiskSize = 32
		d.Datastore = "auto"
	})

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to encode payload: %v", err)
	}

	var round map[string]any
	if err = json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}

	if _, ok := round["instance"]; !ok {
		t.Errorf("round-tripped payload lost the instance key")
	}
}

func TestBuildInstancePayloadLabelsManagedInstances(t *testing.T) {
	payload := buildTestPayload(t, nil)

	labels, ok := payload["labels"].([]string)
	if !ok || len(labels) != 1 || labels[0] != managedLabel {
		t.Errorf("labels = %v, want [%s]", payload["labels"], managedLabel)
	}
}
