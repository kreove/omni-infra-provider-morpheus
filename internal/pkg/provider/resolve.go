// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

// target holds the Morpheus objects a Machine Class resolves to. It is rebuilt
// on each reconcile rather than cached: an operator who repoints a Machine
// Class at a different cloud or layout should not have to restart the provider
// for it to take effect, and the lookups are cheap next to provisioning a VM.
type target struct {
	cloud        NamedObject
	group        NamedObject
	instanceType NamedObject
	layout       NamedObject
	plan         NamedObject
	network      NamedObject
	resourcePool NamedObject
}

// resolveTarget turns a Machine Class into concrete Morpheus IDs.
//
// The layout is authoritative. It selects the hypervisor and names its own
// instance type, so the instance type is read off it rather than resolved
// separately -- which also means plans, networks and pools can be filtered by
// the resolved layout and cloud. Resolving in this order narrows each lookup
// and reports the first genuinely wrong field rather than a downstream symptom
// of it.
func (p *Provisioner) resolveTarget(ctx context.Context, providerData data.Data) (*target, error) {
	resolved := &target{}

	clouds, err := p.client.ListClouds(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Morpheus clouds: %w", err)
	}

	resolved.cloud, err = matchRef(providerData.Cloud, clouds, "cloud")
	if err != nil {
		return nil, err
	}

	groups, err := p.client.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Morpheus groups: %w", err)
	}

	resolved.group, err = matchRef(providerData.Group, groups, "group")
	if err != nil {
		return nil, err
	}

	layouts, err := p.client.ListLayouts(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Morpheus layouts: %w", err)
	}

	// An explicitly configured instance type only narrows the candidates, for
	// the uncommon case of one layout name existing under two instance types.
	// It is never the source of truth.
	if !providerData.InstanceType.IsZero() || providerData.InstanceTypeCode != "" {
		layouts, err = p.filterLayoutsByInstanceType(ctx, layouts, providerData)
		if err != nil {
			return nil, err
		}
	}

	resolved.layout, err = matchRef(providerData.Layout, layouts, "layout")
	if err != nil {
		return nil, err
	}

	// The layout names the instance type it belongs to, and Morpheus
	// provisions from that pair. Reading it off the layout is what stops the
	// two disagreeing: resolving an instance type separately and using it to
	// find a layout meant a layout under any other type could not be selected
	// at all, however correct it was.
	resolved.instanceType = NamedObject{
		ID:   resolved.layout.InstanceType.ID,
		Name: resolved.layout.InstanceType.Name,
		Code: resolved.layout.InstanceType.Code,
	}

	if resolved.instanceType.Code == "" {
		return nil, fmt.Errorf(
			"Morpheus layout %q (id %d) reports no instance type code, which is required to provision from it",
			resolved.layout.Name, resolved.layout.ID,
		)
	}

	plans, err := p.client.ListServicePlans(ctx, resolved.layout.ID, resolved.cloud.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list Morpheus service plans: %w", err)
	}

	resolved.plan, err = matchRef(providerData.Plan, plans, "plan")
	if err != nil {
		return nil, err
	}

	networks, err := p.client.ListNetworks(ctx, resolved.cloud.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list Morpheus networks: %w", err)
	}

	resolved.network, err = matchRef(providerData.Network, networks, "network")
	if err != nil {
		return nil, err
	}

	if !providerData.ResourcePool.IsZero() {
		pools, perr := p.client.ListResourcePools(ctx, resolved.layout.ID, resolved.cloud.ID)
		if perr != nil {
			return nil, fmt.Errorf("failed to list Morpheus resource pools: %w", perr)
		}

		resolved.resourcePool, err = matchRef(providerData.ResourcePool, pools, "resource_pool")
		if err != nil {
			return nil, err
		}
	}

	return resolved, nil
}

// matchRef resolves a reference against a listing, preferring the ID.
func matchRef(ref data.Ref, objects []NamedObject, field string) (NamedObject, error) {
	if ref.ID > 0 {
		for _, object := range objects {
			if object.ID == ref.ID {
				return object, nil
			}
		}

		return NamedObject{}, fmt.Errorf(
			"%s id %d does not exist in Morpheus; available: %s",
			field, ref.ID, describeOptions(objects),
		)
	}

	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return NamedObject{}, fmt.Errorf("%s must set either id or name", field)
	}

	var matches []NamedObject

	for _, object := range objects {
		if strings.EqualFold(object.Name, name) {
			matches = append(matches, object)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return NamedObject{}, fmt.Errorf(
			"%s %q does not exist in Morpheus; available: %s",
			field, name, describeOptions(objects),
		)
	default:
		// Morpheus does not enforce unique names, and silently taking the
		// first match would make provisioning depend on listing order.
		return NamedObject{}, fmt.Errorf(
			"%s %q is ambiguous in Morpheus, it matches %s; set an id instead",
			field, name, describeOptions(matches),
		)
	}
}

// matchCode resolves an instance type by its stable code.
func matchCode(code string, objects []NamedObject) (NamedObject, error) {
	code = strings.TrimSpace(code)

	for _, object := range objects {
		if strings.EqualFold(object.Code, code) {
			return object, nil
		}
	}

	return NamedObject{}, fmt.Errorf(
		"instance_type_code %q does not exist in Morpheus; available: %s",
		code, describeOptions(objects),
	)
}

// filterLayoutsByInstanceType narrows layout candidates to one instance type.
//
// This is a disambiguator, not a selector. It exists for an appliance where the
// same layout name appears under more than one instance type; the matched
// layout still supplies the instance type actually used.
func (p *Provisioner) filterLayoutsByInstanceType(
	ctx context.Context,
	layouts []NamedObject,
	providerData data.Data,
) ([]NamedObject, error) {
	instanceTypes, err := p.client.ListInstanceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Morpheus instance types: %w", err)
	}

	var wanted NamedObject

	if providerData.InstanceType.IsZero() {
		wanted, err = matchCode(providerData.InstanceTypeCode, instanceTypes)
	} else {
		wanted, err = matchRef(providerData.InstanceType, instanceTypes, "instance_type")
	}

	if err != nil {
		return nil, err
	}

	filtered := make([]NamedObject, 0, len(layouts))

	for _, layout := range layouts {
		if layout.InstanceType.ID == wanted.ID {
			filtered = append(filtered, layout)
		}
	}

	if len(filtered) == 0 {
		return nil, fmt.Errorf(
			"no Morpheus layout belongs to instance type %q (id %d); "+
				"leave instance_type unset to choose from every layout",
			wanted.Name, wanted.ID,
		)
	}

	return filtered, nil
}
