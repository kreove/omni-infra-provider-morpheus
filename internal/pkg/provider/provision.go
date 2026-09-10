// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements the Morpheus Omni infrastructure provider.
package provider

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
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
)

// Provisioner provisions Talos VMs on Morpheus.
type Provisioner struct {
	client      *Client
	imageBuilds sync.Map
}

// NewProvisioner creates a Morpheus provisioner.
func NewProvisioner(client *Client) *Provisioner {
	return &Provisioner{client: client}
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
		provision.NewStep("syncMachine", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			providerData, err := unmarshalProviderData(pctx)
			if err != nil {
				return err
			}

			return p.syncMachine(ctx, logger, pctx, providerData)
		}),
	}
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

	payload := buildInstancePayload(name, joinConfig, imageID, resolved, providerData)

	created, err := p.client.CreateInstance(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create Morpheus instance %q: %w", name, err)
	}

	return created, nil
}

// buildInstancePayload renders the Morpheus provisioning request for a machine.
func buildInstancePayload(
	name, joinConfig string,
	imageID int,
	resolved *target,
	providerData data.Data,
) map[string]any {
	config := map[string]any{
		"imageId":          imageID,
		"poolProviderType": mvmProviderType,
		// Talos has no user accounts, no shell and no SSH server. Letting
		// Morpheus inject a login user rewrites the cloud-init user-data that
		// carries the join config, and the agent install can only fail against
		// a guest it cannot log into.
		"createUser": false,
		"noAgent":    true,
		// The Talos machine configuration, delivered verbatim through the
		// NoCloud datasource. This is not cloud-config YAML: Talos parses
		// user-data itself, so anything Morpheus adds to it is read as part of
		// the machine config and breaks the join.
		"userData": joinConfig,
	}

	if resolved.resourcePool.ID > 0 {
		config["resourcePoolId"] = resolved.resourcePool.ID
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
