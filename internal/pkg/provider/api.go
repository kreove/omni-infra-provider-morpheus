// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// listPageSize caps lookup listings. Morpheus paginates at 25 by default,
// which silently hides objects from a name lookup on any appliance with more
// than a handful of them.
const listPageSize = "1000"

// NamedObject is the identity subset shared by the Morpheus objects this
// provider resolves by ID or name.
type NamedObject struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Code string `json:"code"`
}

// Instance is the subset of a Morpheus instance this provider acts on.
type Instance struct {
	ID     int    `json:"id"`
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// VirtualImage is the subset of a Morpheus virtual image this provider acts on.
type VirtualImage struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ImageType string `json:"imageType"`
	Status    string `json:"status"`
	RawSize   int64  `json:"rawSize"`
}

// ResourcePoolOption is one entry from the zonePools option source.
type ResourcePoolOption struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	ProviderType string `json:"providerType"`
}

// Instance statuses Morpheus reports. Provisioning is asynchronous, so the
// provider polls these rather than blocking on the create call.
const (
	instanceStatusRunning      = "running"
	instanceStatusProvisioning = "provisioning"
	instanceStatusStopped      = "stopped"
	instanceStatusFailed       = "failed"
)

// ListInstancesByName returns instances whose name matches exactly.
//
// Morpheus's name filter is a substring match, so "talos-1" also returns
// "talos-10". The results are filtered down to exact matches here; without
// that, scaling a cluster past nine machines makes the provider believe a
// machine it has not created yet already exists.
func (c *Client) ListInstancesByName(ctx context.Context, name string) ([]Instance, error) {
	var result struct {
		Instances []Instance `json:"instances"`
	}

	if err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/instances",
		query:  url.Values{"name": {name}, "max": {listPageSize}},
		out:    &result,
	}); err != nil {
		return nil, err
	}

	exact := make([]Instance, 0, 1)

	for _, instance := range result.Instances {
		if instance.Name == name {
			exact = append(exact, instance)
		}
	}

	return exact, nil
}

// GetInstance reads a single instance by ID.
func (c *Client) GetInstance(ctx context.Context, id int) (*Instance, error) {
	var result struct {
		Instance *Instance `json:"instance"`
	}

	if err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/instances/" + itoa(id),
		out:    &result,
	}); err != nil {
		return nil, err
	}

	if result.Instance == nil {
		return nil, fmt.Errorf("Morpheus returned no instance for ID %d", id)
	}

	return result.Instance, nil
}

// CreateInstance provisions a new instance and returns it.
func (c *Client) CreateInstance(ctx context.Context, payload map[string]any) (*Instance, error) {
	var result struct {
		Instance *Instance `json:"instance"`
	}

	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/api/instances",
		body:   payload,
		out:    &result,
	}); err != nil {
		return nil, err
	}

	if result.Instance == nil {
		return nil, fmt.Errorf("Morpheus accepted the instance request but returned no instance")
	}

	return result.Instance, nil
}

// StartInstance powers an instance on.
func (c *Client) StartInstance(ctx context.Context, id int) error {
	return c.do(ctx, request{
		method: http.MethodPut,
		path:   "/api/instances/" + itoa(id) + "/start",
	})
}

// StopInstance powers an instance off.
func (c *Client) StopInstance(ctx context.Context, id int) error {
	return c.do(ctx, request{
		method: http.MethodPut,
		path:   "/api/instances/" + itoa(id) + "/stop",
	})
}

// DeleteInstance removes an instance and its volumes.
//
// removeVolumes is required or the boot disk is left behind on the datastore,
// filling it up one deprovisioned machine at a time. force lets an instance
// that failed to provision be removed as well; without it Morpheus refuses to
// delete an instance stuck in an error state, which is exactly the instance an
// operator most needs to clear.
func (c *Client) DeleteInstance(ctx context.Context, id int) error {
	return c.do(ctx, request{
		method: http.MethodDelete,
		path:   "/api/instances/" + itoa(id),
		query: url.Values{
			"removeVolumes": {"true"},
			"force":         {"true"},
		},
	})
}

// ListVirtualImagesByName returns virtual images whose name matches exactly.
func (c *Client) ListVirtualImagesByName(ctx context.Context, name string) ([]VirtualImage, error) {
	var result struct {
		VirtualImages []VirtualImage `json:"virtualImages"`
	}

	if err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/virtual-images",
		query:  url.Values{"name": {name}, "max": {listPageSize}},
		out:    &result,
	}); err != nil {
		return nil, err
	}

	exact := make([]VirtualImage, 0, 1)

	for _, image := range result.VirtualImages {
		if image.Name == name {
			exact = append(exact, image)
		}
	}

	return exact, nil
}

// GetVirtualImage reads a single virtual image by ID.
func (c *Client) GetVirtualImage(ctx context.Context, id int) (*VirtualImage, error) {
	var result struct {
		VirtualImage *VirtualImage `json:"virtualImage"`
	}

	if err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/virtual-images/" + itoa(id),
		out:    &result,
	}); err != nil {
		return nil, err
	}

	if result.VirtualImage == nil {
		return nil, fmt.Errorf("Morpheus returned no virtual image for ID %d", id)
	}

	return result.VirtualImage, nil
}

// CreateVirtualImage registers a virtual image record, which the image file is
// then uploaded into.
func (c *Client) CreateVirtualImage(ctx context.Context, payload map[string]any) (*VirtualImage, error) {
	var result struct {
		VirtualImage *VirtualImage `json:"virtualImage"`
	}

	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/api/virtual-images",
		body:   map[string]any{"virtualImage": payload},
		out:    &result,
	}); err != nil {
		return nil, err
	}

	if result.VirtualImage == nil {
		return nil, fmt.Errorf("Morpheus accepted the virtual image but returned no record")
	}

	return result.VirtualImage, nil
}

// UploadVirtualImageFile streams an image file into an existing virtual image.
func (c *Client) UploadVirtualImageFile(ctx context.Context, id int, filename, filePath string, timeout time.Duration) error {
	return c.uploadFile(
		ctx,
		"/api/virtual-images/"+itoa(id)+"/upload",
		url.Values{"filename": {filename}},
		filePath,
		timeout,
	)
}

// DeleteVirtualImage removes a virtual image.
func (c *Client) DeleteVirtualImage(ctx context.Context, id int) error {
	return c.do(ctx, request{
		method: http.MethodDelete,
		path:   "/api/virtual-images/" + itoa(id),
	})
}

// listNamed reads a list endpoint into a slice of NamedObject.
func (c *Client) listNamed(ctx context.Context, path, key string, query url.Values) ([]NamedObject, error) {
	if query == nil {
		query = url.Values{}
	}

	query.Set("max", listPageSize)

	// The response shape differs per endpoint only in the key holding the
	// array, so it is decoded generically and the key picked out afterwards.
	var raw map[string]json.RawMessage

	if err := c.do(ctx, request{
		method: http.MethodGet,
		path:   path,
		query:  query,
		out:    &raw,
	}); err != nil {
		return nil, err
	}

	return decodeNamedList(raw, key, path)
}

// ListGroups returns Morpheus groups (sites).
func (c *Client) ListGroups(ctx context.Context) ([]NamedObject, error) {
	return c.listNamed(ctx, "/api/groups", "groups", nil)
}

// ListClouds returns Morpheus clouds (zones).
func (c *Client) ListClouds(ctx context.Context) ([]NamedObject, error) {
	return c.listNamed(ctx, "/api/zones", "zones", nil)
}

// ListInstanceTypes returns library instance types.
func (c *Client) ListInstanceTypes(ctx context.Context) ([]NamedObject, error) {
	return c.listNamed(ctx, "/api/library/instance-types", "instanceTypes", nil)
}

// ListLayouts returns library layouts for an instance type.
func (c *Client) ListLayouts(ctx context.Context, instanceTypeID int) ([]NamedObject, error) {
	query := url.Values{}
	if instanceTypeID > 0 {
		query.Set("instanceTypeId", itoa(instanceTypeID))
	}

	return c.listNamed(ctx, "/api/library/layouts", "instanceTypeLayouts", query)
}

// ListServicePlans returns service plans available for a layout and cloud.
//
// The filters matter: an unfiltered listing includes plans belonging to other
// hypervisors, so a name lookup can resolve to a plan Morpheus will then
// reject for this layout.
func (c *Client) ListServicePlans(ctx context.Context, layoutID, cloudID int) ([]NamedObject, error) {
	query := url.Values{}
	if layoutID > 0 {
		query.Set("layoutId", itoa(layoutID))
	}

	if cloudID > 0 {
		query.Set("zoneId", itoa(cloudID))
	}

	return c.listNamed(ctx, "/api/service-plans", "servicePlans", query)
}

// ListNetworks returns networks visible to a cloud.
func (c *Client) ListNetworks(ctx context.Context, cloudID int) ([]NamedObject, error) {
	query := url.Values{}
	if cloudID > 0 {
		query.Set("zoneId", itoa(cloudID))
	}

	return c.listNamed(ctx, "/api/networks", "networks", query)
}

// ListResourcePools returns the compute pools offered for a layout, filtered to
// the MVM provider type.
func (c *Client) ListResourcePools(ctx context.Context, layoutID, cloudID int) ([]NamedObject, error) {
	query := url.Values{}
	if layoutID > 0 {
		query.Set("layoutId", itoa(layoutID))
	}

	if cloudID > 0 {
		query.Set("zoneId", itoa(cloudID))
	}

	var result struct {
		Data []ResourcePoolOption `json:"data"`
	}

	if err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/options/zonePools",
		query:  query,
		out:    &result,
	}); err != nil {
		return nil, err
	}

	pools := make([]NamedObject, 0, len(result.Data))

	for _, pool := range result.Data {
		// An option source lists pools for every provider the appliance knows
		// about. Anything that is not MVM cannot host this provider's VMs.
		if pool.ProviderType != "" && pool.ProviderType != mvmProviderType {
			continue
		}

		pools = append(pools, NamedObject{ID: pool.ID, Name: pool.Name})
	}

	return pools, nil
}
