// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements the Morpheus Omni infrastructure provider.
package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/nocloud"
	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/resources"
)

const (
	// managedLabel marks every object this provider creates, so an operator
	// can tell Omni-managed instances apart from hand-built ones.
	managedLabel = "omni-managed"

	rootVolumeName = "root"

	// newVolumeID is the sentinel Morpheus expects for a volume it should
	// create rather than reuse.
	newVolumeID = -1

	// configFetchGracePeriod is how long provisioning waits for a machine to
	// collect its config from the NoCloud server before giving up on seeing it
	// happen.
	//
	// Waiting at all is what keeps Omni reconciling the request, which is what
	// re-registers the machine after a provider restart. The wait is bounded
	// because a machine that already joined Omni on an earlier boot will never
	// ask again, and that must not hold its request open forever.
	configFetchGracePeriod = 10 * time.Minute
)

// Provisioner provisions Talos VMs on Morpheus.
type Provisioner struct {
	client         *Client
	nocloudServer  *nocloud.Server
	nocloudBaseURL string
	imageBuilds    sync.Map
}

// NewProvisioner creates a Morpheus provisioner.
//
// The NoCloud server and its base URL are optional, and when either is unset
// the provider falls back to handing the join config to Morpheus as
// config.userData. That only works on an appliance that passes user data
// through untouched; see docs/compatibility.md.
func NewProvisioner(client *Client, nocloudServer *nocloud.Server, nocloudBaseURL string) *Provisioner {
	return &Provisioner{
		client:         client,
		nocloudServer:  nocloudServer,
		nocloudBaseURL: nocloudBaseURL,
	}
}

// nocloudEnabled reports whether machines are pointed at this provider's
// NoCloud server instead of the config drive Morpheus writes.
func (p *Provisioner) nocloudEnabled() bool {
	return p.nocloudServer != nil && p.nocloudBaseURL != ""
}

// ProvisionSteps implements infra.Provisioner.
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("validateRequest", func(_ context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			if len(pctx.GetRequestID()) > 63 {
				return fmt.Errorf("machine request name cannot be longer than 63 characters")
			}

			providerData, err := unmarshalProviderData(pctx)
			if err != nil {
				return err
			}

			return validateProviderData(providerData)
		}),
		provision.NewStep("ensureTarget", func(ctx context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			providerData, err := unmarshalProviderData(pctx)
			if err != nil {
				return err
			}

			// Resolved and thrown away deliberately. This runs before any VM
			// is created so a Machine Class naming an object that does not
			// exist fails here, with a message listing what does, rather than
			// halfway through provisioning.
			_, err = p.resolveTarget(ctx, providerData)

			return err
		}),
		// Resolving the installation medium also ensures the schematic exists
		// and reports its ID, so there is no separate schematic step. Keeping
		// one would mean asking Omni for the same medium twice per reconcile,
		// and the download URL it returns is short-lived -- it belongs in the
		// step that actually fetches it, not in an earlier one.
		provision.NewStep("ensureImage", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			providerData, err := unmarshalProviderData(pctx)
			if err != nil {
				return err
			}

			imageID, ready, err := p.ensureTalosImage(ctx, logger, pctx, providerData)
			if err != nil {
				return err
			}

			if !ready {
				return provision.NewRetryInterval(15 * time.Second)
			}

			pctx.State.TypedSpec().Value.ImageId = strconv.Itoa(imageID)

			return nil
		}),
		// Publishing the datasource is its own step so that the token reaches
		// Omni's state before any VM exists. The token is baked into the VM's
		// SMBIOS serial at creation and cannot be changed afterwards, so a
		// token that was minted but not recorded would strand the machine it
		// was minted for. Each step's state changes are committed before the
		// next one runs, which createInstance relies on.
		provision.NewStep("publishConfig", func(_ context.Context, _ *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			return p.publishMachineConfig(pctx)
		}),
		provision.NewStep("syncMachine", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			providerData, err := unmarshalProviderData(pctx)
			if err != nil {
				return err
			}

			return p.syncMachine(ctx, logger, pctx, providerData)
		}),
	}
}

// publishMachineConfig mints this machine's NoCloud token if it does not have
// one yet, and publishes its datasource.
//
// Republishing on every reconcile is deliberate: the registry is in memory, so
// a provider that restarted while a machine was still booting has to put the
// entry back before that machine gives up asking for it.
func (p *Provisioner) publishMachineConfig(pctx provision.Context[*resources.Machine]) error {
	if !p.nocloudEnabled() {
		return nil
	}

	joinConfig := pctx.ConnectionParams.JoinConfig
	if joinConfig == "" {
		return fmt.Errorf("Omni supplied an empty join config for machine %q", pctx.GetRequestID())
	}

	state := pctx.State.TypedSpec().Value

	if state.ConfigToken == "" {
		token, err := nocloud.NewToken()
		if err != nil {
			return err
		}

		state.ConfigToken = token
	}

	p.nocloudServer.Register(state.ConfigToken, nocloud.Machine{
		JoinConfig: joinConfig,
		// Morpheus lowercases the guest hostname it derives from the instance
		// name, and a Talos hostname has to be a valid DNS label regardless, so
		// the request ID is lowercased here rather than passed through as Omni
		// spells it.
		Hostname: strings.ToLower(pctx.GetRequestID()),
	})

	return nil
}

func (p *Provisioner) syncMachine(
	ctx context.Context,
	logger *zap.Logger,
	pctx provision.Context[*resources.Machine],
	providerData data.Data,
) error {
	name := pctx.GetRequestID()

	instance, err := p.findInstance(ctx, name)
	if err != nil {
		return err
	}

	if instance == nil {
		created, createErr := p.createInstance(ctx, pctx, providerData)
		if createErr != nil {
			return createErr
		}

		pctx.State.TypedSpec().Value.Uuid = created.UUID
		pctx.State.TypedSpec().Value.InstanceId = strconv.Itoa(created.ID)

		logger.Info(
			"created Morpheus instance",
			zap.String("name", created.Name),
			zap.Int("id", created.ID),
		)

		return provision.NewRetryInterval(15 * time.Second)
	}

	if pctx.State.TypedSpec().Value.Uuid == "" {
		pctx.State.TypedSpec().Value.Uuid = instance.UUID
	}

	if pctx.State.TypedSpec().Value.InstanceId == "" {
		pctx.State.TypedSpec().Value.InstanceId = strconv.Itoa(instance.ID)
	}

	switch instance.Status {
	case instanceStatusRunning:
		if retry := p.awaitConfigFetch(logger, pctx.State.TypedSpec().Value.ConfigToken, instance); retry != nil {
			return retry
		}

		logger.Info(
			"machine is running",
			zap.String("name", instance.Name),
			zap.Int("id", instance.ID),
		)

		return nil
	case instanceStatusFailed:
		// Reported rather than retried: Morpheus has finished and failed, so
		// polling it again cannot change the outcome. The instance is left in
		// place for an operator to inspect, and removed on deprovision.
		return fmt.Errorf(
			"Morpheus instance %q (id %d) failed to provision; inspect it in Morpheus for the underlying error",
			instance.Name, instance.ID,
		)
	case instanceStatusStopped:
		if err = p.client.StartInstance(ctx, instance.ID); err != nil {
			return fmt.Errorf("failed to power on Morpheus instance %q: %w", instance.Name, err)
		}

		return provision.NewRetryInterval(10 * time.Second)
	default:
		// provisioning, pending, resizing and anything else Morpheus is in the
		// middle of doing.
		logger.Info(
			"waiting for Morpheus instance",
			zap.String("name", instance.Name),
			zap.Int("id", instance.ID),
			zap.String("status", instance.Status),
		)

		return provision.NewRetryInterval(15 * time.Second)
	}
}

// awaitConfigFetch holds the request open until the machine has collected its
// Talos config, or until the grace period runs out.
//
// A running VM is not a provisioned machine: Morpheus reports "running" as soon
// as the VM is powered on, long before Talos has read its config. Reporting
// success at that point would hide the failure this whole mechanism exists to
// avoid -- a machine that boots perfectly and then sits in maintenance mode
// because it never got a config.
func (p *Provisioner) awaitConfigFetch(logger *zap.Logger, token string, instance *Instance) error {
	if !p.nocloudEnabled() || token == "" {
		return nil
	}

	status, ok := p.nocloudServer.Status(token)
	if !ok || !status.ServedAt.IsZero() {
		return nil
	}

	if time.Since(status.RegisteredAt) < configFetchGracePeriod {
		logger.Info(
			"waiting for the machine to fetch its Talos config",
			zap.String("name", instance.Name),
			zap.Int("id", instance.ID),
		)

		return provision.NewRetryInterval(15 * time.Second)
	}

	// Not an error: a machine that joined Omni on an earlier boot never asks
	// again, and this provider process may simply not have been the one that
	// served it.
	logger.Warn(
		"machine never fetched its Talos config from this provider; "+
			"if it is not in Omni, check that the machine can reach the NoCloud server URL",
		zap.String("name", instance.Name),
		zap.Int("id", instance.ID),
		zap.String("nocloud_url", nocloud.MachineURL(p.nocloudBaseURL, token)),
	)

	return nil
}

func (p *Provisioner) createInstance(
	ctx context.Context,
	pctx provision.Context[*resources.Machine],
	providerData data.Data,
) (*Instance, error) {
	name := pctx.GetRequestID()

	resolved, err := p.resolveTarget(ctx, providerData)
	if err != nil {
		return nil, err
	}

	imageID, err := parseImageID(pctx.State.TypedSpec().Value.ImageId)
	if err != nil {
		return nil, err
	}

	joinConfig := pctx.ConnectionParams.JoinConfig
	if joinConfig == "" {
		return nil, fmt.Errorf("Omni supplied an empty join config for instance %q", name)
	}

	var nocloudSerial string

	if p.nocloudEnabled() {
		token := pctx.State.TypedSpec().Value.ConfigToken
		if token == "" {
			return nil, fmt.Errorf("machine %q has no NoCloud config token", name)
		}

		nocloudSerial = nocloud.SMBIOSSerial(p.nocloudBaseURL, token)
	}

	payload := buildInstancePayload(name, joinConfig, nocloudSerial, imageID, resolved, providerData)

	created, err := p.client.CreateInstance(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create Morpheus instance %q: %w", name, err)
	}

	return created, nil
}

// buildInstancePayload renders the Morpheus provisioning request for a machine.
//
// nocloudSerial, when set, is the SMBIOS system serial number that sends the
// guest to this provider's NoCloud server instead of the config drive Morpheus
// writes.
func buildInstancePayload(
	name, joinConfig, nocloudSerial string,
	imageID int,
	resolved *target,
	providerData data.Data,
) map[string]any {
	config := map[string]any{
		"imageId":          imageID,
		"poolProviderType": mvmProviderType,
		// Talos has no user accounts, no shell and no SSH server, so a login
		// user is useless and the agent cannot install against a guest it
		// cannot log into. Neither of these stops Morpheus rewriting the
		// config drive -- on a cloud whose agent install mode is cloudInit the
		// rendered document still gets a user and an agent callback -- so they
		// are sent as the correct request, not as a mitigation. What actually
		// delivers the config is the NoCloud server; see nocloudSerial below.
		"createUser": false,
		"noAgent":    true,
	}

	// Sent only when the machine is not being pointed at the NoCloud server.
	//
	// Morpheus does not pass this to the guest: it renders its own
	// #cloud-config and folds this value into that document's runcmd list,
	// which Talos discards wholesale, returning ErrNoConfigSource for anything
	// beginning with #cloud-config. So on a machine using the NoCloud server
	// it delivers nothing -- while still writing the join config, and the join
	// token in it, onto a config drive readable by anyone with access to the
	// instance in Morpheus.
	//
	// It is kept for the case where no NoCloud server is configured, so an
	// appliance that genuinely passes user data through untouched still works.
	// No such appliance has been observed.
	if nocloudSerial == "" {
		config["userData"] = joinConfig
	}

	if resolved.resourcePool.ID > 0 {
		config["resourcePoolId"] = resolved.resourcePool.ID
	}

	if nocloudSerial != "" {
		// libvirt already emits -smbios type=1 from the domain's <sysinfo>,
		// where Morpheus sets the serial to the VM's UUID. A later -smbios
		// option overrides the fields it names, so this replaces the serial and
		// leaves the rest of the table alone.
		config["qemuArgs"] = "-smbios type=1,serial=" + nocloudSerial
	}

	instance := map[string]any{
		"name":        name,
		"hostName":    name,
		"type":        resolved.instanceType.Code,
		"description": "Talos machine managed by Sidero Omni",
		"site":        map[string]any{"id": resolved.group.ID},
		"plan": map[string]any{
			"id":   resolved.plan.ID,
			"code": resolved.plan.Code,
			"name": resolved.plan.Name,
		},
		"layout": map[string]any{
			"id":   resolved.layout.ID,
			"code": resolved.layout.Code,
			"name": resolved.layout.Name,
		},
	}

	payload := map[string]any{
		"zoneId":   resolved.cloud.ID,
		"instance": instance,
		"config":   config,
		"labels":   []string{managedLabel},
		// Morpheus identifies a NIC's network by a prefixed string rather than
		// a bare ID; a plain integer is silently ignored and the VM comes up
		// with no network at all.
		"networkInterfaces": []map[string]any{
			{"network": map[string]any{"id": fmt.Sprintf("network-%d", resolved.network.ID)}},
		},
	}

	if volume := rootVolumePayload(providerData); volume != nil {
		payload["volumes"] = []map[string]any{volume}
	}

	if options := servicePlanOptions(providerData); options != nil {
		payload["servicePlanOptions"] = options
	}

	return payload
}

// rootVolumePayload builds the boot volume override, or nil to let the service
// plan decide.
func rootVolumePayload(providerData data.Data) map[string]any {
	if providerData.DiskSize <= 0 && providerData.Datastore == "" {
		return nil
	}

	volume := map[string]any{
		"id":         newVolumeID,
		"rootVolume": true,
		"name":       rootVolumeName,
	}

	if providerData.DiskSize > 0 {
		volume["size"] = providerData.DiskSize
	}

	if providerData.Datastore != "" {
		// Passed through as written: Morpheus accepts a numeric datastore ID
		// and the strings "auto" and "autoCluster" in the same field.
		volume["datastoreId"] = providerData.Datastore
	}

	return volume
}

// servicePlanOptions builds the sizing override, or nil to let the service plan
// decide.
//
// Morpheus sizes an instance from its service plan. These fields only take
// effect on a plan that allows custom sizing; on a fixed plan Morpheus ignores
// them, which is why they are optional rather than defaulted.
func servicePlanOptions(providerData data.Data) map[string]any {
	options := map[string]any{}

	if providerData.Cores > 0 {
		options["maxCores"] = providerData.Cores
	}

	if providerData.Memory > 0 {
		// Morpheus takes memory in bytes here, while the Machine Class states
		// it in MiB to match the other Omni providers.
		options["maxMemory"] = providerData.Memory * 1024 * 1024
	}

	if len(options) == 0 {
		return nil
	}

	return options
}

// findInstance returns the instance for a machine request, or nil.
func (p *Provisioner) findInstance(ctx context.Context, name string) (*Instance, error) {
	instances, err := p.client.ListInstancesByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to query Morpheus instance %q: %w", name, err)
	}

	switch len(instances) {
	case 0:
		return nil, nil
	case 1:
		return &instances[0], nil
	default:
		return nil, fmt.Errorf("multiple Morpheus instances are named %q", name)
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(
	ctx context.Context,
	logger *zap.Logger,
	state *resources.Machine,
	machineRequest *infra.MachineRequest,
) error {
	if p.nocloudEnabled() && state != nil && state.TypedSpec().Value.ConfigToken != "" {
		p.nocloudServer.Forget(state.TypedSpec().Value.ConfigToken)
	}

	instance, err := p.resolveInstanceForRemoval(ctx, state, machineRequest.Metadata().ID())
	if err != nil {
		return err
	}

	if instance == nil {
		logger.Info("machine deprovisioned")

		return nil
	}

	// A machine still being built cannot be removed cleanly; Morpheus rejects
	// the delete or leaves the volume behind. Wait for it to settle first.
	if instance.Status == instanceStatusProvisioning {
		logger.Info(
			"waiting for Morpheus instance to finish provisioning before removing it",
			zap.String("name", instance.Name),
			zap.Int("id", instance.ID),
		)

		return provision.NewRetryInterval(15 * time.Second)
	}

	if instance.Status == instanceStatusRunning {
		if err = p.client.StopInstance(ctx, instance.ID); err != nil {
			return fmt.Errorf("failed to power off Morpheus instance %q: %w", instance.Name, err)
		}

		return provision.NewRetryInterval(10 * time.Second)
	}

	if err = p.client.DeleteInstance(ctx, instance.ID); err != nil {
		// Already gone, which is the desired end state.
		if IsNotFound(err) {
			logger.Info("machine deprovisioned")

			return nil
		}

		return fmt.Errorf("failed to delete Morpheus instance %q: %w", instance.Name, err)
	}

	// Deletion is asynchronous. Come back to confirm the instance is really
	// gone rather than reporting success on the request being accepted.
	return provision.NewRetryInterval(10 * time.Second)
}

// resolveInstanceForRemoval finds the instance to delete, preferring the ID
// recorded at creation.
//
// The name lookup alone is not enough: an operator who renames an instance in
// Morpheus would otherwise strand it, with Omni reporting the machine as
// deprovisioned while the VM keeps running.
func (p *Provisioner) resolveInstanceForRemoval(
	ctx context.Context,
	state *resources.Machine,
	requestID string,
) (*Instance, error) {
	if state != nil && state.TypedSpec().Value.InstanceId != "" {
		id, err := strconv.Atoi(state.TypedSpec().Value.InstanceId)
		if err == nil && id > 0 {
			instance, gerr := p.client.GetInstance(ctx, id)
			if gerr == nil {
				return instance, nil
			}

			if !IsNotFound(gerr) {
				return nil, fmt.Errorf("failed to read Morpheus instance %d: %w", id, gerr)
			}

			// Recorded but gone from Morpheus: nothing left to remove.
			return nil, nil
		}
	}

	return p.findInstance(ctx, requestID)
}

// unmarshalProviderData decodes and normalizes a Machine Class.
func unmarshalProviderData(pctx provision.Context[*resources.Machine]) (data.Data, error) {
	var providerData data.Data

	if err := pctx.UnmarshalProviderData(&providerData); err != nil {
		return providerData, err
	}

	applyDefaults(&providerData)

	return providerData, nil
}
