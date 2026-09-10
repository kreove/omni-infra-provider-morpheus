// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{Endpoint: server.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	return client
}

func TestNewClientRequiresCredentials(t *testing.T) {
	_, err := NewClient(ClientConfig{Endpoint: "https://morpheus.example.com"})
	if err == nil || !strings.Contains(err.Error(), "MORPHEUS_TOKEN") {
		t.Fatalf("expected a credentials error, got %v", err)
	}
}

func TestNewClientRequiresEndpoint(t *testing.T) {
	_, err := NewClient(ClientConfig{Token: "x"})
	if err == nil || !strings.Contains(err.Error(), "endpoint is required") {
		t.Fatalf("expected an endpoint error, got %v", err)
	}
}

// An endpoint pasted without a scheme is common in .env files.
func TestNewClientDefaultsToHTTPS(t *testing.T) {
	client, err := NewClient(ClientConfig{Endpoint: "morpheus.example.com", Token: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if client.baseURL.Scheme != "https" {
		t.Errorf("scheme = %q, want https", client.baseURL.Scheme)
	}
}

// A trailing slash would produce "/api//instances", which Morpheus answers
// with a redirect to the login page rather than the resource.
func TestNewClientTrimsTrailingSlash(t *testing.T) {
	client, err := NewClient(ClientConfig{Endpoint: "https://morpheus.example.com/", Token: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.HasSuffix(client.baseURL.Path, "/") {
		t.Errorf("base path %q still ends with a slash", client.baseURL.Path)
	}
}

func TestClientSendsBearerToken(t *testing.T) {
	var got string

	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")

		json.NewEncoder(w).Encode(map[string]any{"instances": []any{}})
	}))

	if _, err := client.ListInstancesByName(t.Context(), "x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
}

func TestClientLogsInWithUsernameAndPassword(t *testing.T) {
	var loginCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			atomic.AddInt32(&loginCalls, 1)

			if r.URL.Query().Get("client_id") != morpheusClientID {
				t.Errorf("client_id = %q", r.URL.Query().Get("client_id"))
			}

			if r.URL.Query().Get("grant_type") != "password" {
				t.Errorf("grant_type = %q", r.URL.Query().Get("grant_type"))
			}

			if err := r.ParseForm(); err != nil {
				t.Errorf("failed to parse login form: %v", err)
			}

			if r.PostForm.Get("password") != "secret" {
				t.Errorf("password = %q", r.PostForm.Get("password"))
			}

			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "issued-token",
				"expires_in":   3600,
			})

			return
		}

		if r.Header.Get("Authorization") != "Bearer issued-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}

		json.NewEncoder(w).Encode(map[string]any{"instances": []any{}})
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Endpoint: server.URL, Username: "admin", Password: "secret"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	for range 3 {
		if _, err = client.ListInstancesByName(t.Context(), "x"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// The token is cached; re-authenticating on every call would hammer the
	// appliance and eventually exhaust its session table.
	if calls := atomic.LoadInt32(&loginCalls); calls != 1 {
		t.Errorf("logged in %d times, want 1", calls)
	}
}

// A revoked or expired session must repair itself rather than breaking the
// provider until the process is restarted.
func TestClientRetriesAfterUnauthorized(t *testing.T) {
	var (
		loginCalls int32
		apiCalls   int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			count := atomic.AddInt32(&loginCalls, 1)

			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "token-" + itoa(int(count)),
				"expires_in":   3600,
			})

			return
		}

		// Reject the first token once, then accept the reissued one.
		if atomic.AddInt32(&apiCalls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if r.Header.Get("Authorization") != "Bearer token-2" {
			t.Errorf("retry used %q, want the reissued token", r.Header.Get("Authorization"))
		}

		json.NewEncoder(w).Encode(map[string]any{"instances": []any{}})
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Endpoint: server.URL, Username: "admin", Password: "secret"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	if _, err = client.ListInstancesByName(t.Context(), "x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := atomic.LoadInt32(&loginCalls); calls != 2 {
		t.Errorf("logged in %d times, want 2", calls)
	}
}

// Morpheus reports some failures with HTTP 200 and success:false. Trusting the
// status code alone would silently accept a failed operation.
func TestClientRejectsUnsuccessfulEnvelope(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"msg":     "Validation failed",
			"errors":  map[string]string{"plan": "is required"},
		})
	}))

	_, err := client.CreateInstance(t.Context(), map[string]any{})
	if err == nil {
		t.Fatal("expected an error for success:false")
	}

	// The per-field errors are the only part that says what was actually wrong.
	if !strings.Contains(err.Error(), "plan: is required") {
		t.Errorf("error %q does not include the field errors", err.Error())
	}
}

func TestClientReportsHTTPErrors(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"msg": "boom"})
	}))

	_, err := client.GetInstance(t.Context(), 1)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected an HTTP 500 error mentioning the message, got %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	_, err := client.GetInstance(t.Context(), 1)
	if !IsNotFound(err) {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

// Morpheus filters names by substring, so "talos-1" also returns "talos-10".
// Without exact filtering the provider believes a machine it has not created
// already exists, and a cluster silently stops scaling past nine nodes.
func TestListInstancesByNameFiltersToExactMatches(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"instances": []map[string]any{
				{"id": 10, "name": "talos-10"},
				{"id": 1, "name": "talos-1"},
				{"id": 11, "name": "talos-11"},
			},
		})
	}))

	instances, err := client.ListInstancesByName(t.Context(), "talos-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(instances) != 1 || instances[0].ID != 1 {
		t.Fatalf("got %+v, want only the exact match", instances)
	}
}

func TestListVirtualImagesByNameFiltersToExactMatches(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"virtualImages": []map[string]any{
				{"id": 2, "name": "omni-talos-abc-old"},
				{"id": 1, "name": "omni-talos-abc"},
			},
		})
	}))

	images, err := client.ListVirtualImagesByName(t.Context(), "omni-talos-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(images) != 1 || images[0].ID != 1 {
		t.Fatalf("got %+v, want only the exact match", images)
	}
}

// Leaving the volume behind fills the datastore one deprovisioned machine at a
// time, and without force an instance stuck in an error state cannot be
// removed at all.
func TestDeleteInstanceRemovesVolumes(t *testing.T) {
	var query url.Values

	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()

		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))

	if err := client.DeleteInstance(t.Context(), 7); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := query.Get("removeVolumes"); got != "true" {
		t.Errorf("removeVolumes = %q, want true", got)
	}

	if got := query.Get("force"); got != "true" {
		t.Errorf("force = %q, want true", got)
	}
}

// The default page size of 25 would hide objects from a name lookup on any
// appliance with more than a handful of them.
func TestListingsRequestALargePage(t *testing.T) {
	var query url.Values

	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()

		json.NewEncoder(w).Encode(map[string]any{"zones": []any{}})
	}))

	if _, err := client.ListClouds(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := query.Get("max"); got != listPageSize {
		t.Errorf("max = %q, want %q", got, listPageSize)
	}
}

func TestListNamedDecodesEachEndpointKey(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
		call func(*Client) ([]NamedObject, error)
	}{
		{"clouds", "zones", func(c *Client) ([]NamedObject, error) { return c.ListClouds(t.Context()) }},
		{"groups", "groups", func(c *Client) ([]NamedObject, error) { return c.ListGroups(t.Context()) }},
		{"instance types", "instanceTypes", func(c *Client) ([]NamedObject, error) { return c.ListInstanceTypes(t.Context()) }},
		{"layouts", "instanceTypeLayouts", func(c *Client) ([]NamedObject, error) { return c.ListLayouts(t.Context()) }},
		{"plans", "servicePlans", func(c *Client) ([]NamedObject, error) { return c.ListServicePlans(t.Context(), 1, 2) }},
		{"networks", "networks", func(c *Client) ([]NamedObject, error) { return c.ListNetworks(t.Context(), 1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{
					test.key: []map[string]any{{"id": 3, "name": "Example", "code": "example"}},
				})
			}))

			objects, err := test.call(client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(objects) != 1 || objects[0].ID != 3 || objects[0].Name != "Example" {
				t.Fatalf("got %+v, want the single decoded object", objects)
			}
		})
	}
}

// A response missing the expected key usually means the request was answered by
// a login page, which is worth saying plainly.
func TestListNamedReportsMissingKey(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"somethingElse": []any{}})
	}))

	_, err := client.ListClouds(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no \"zones\" field") {
		t.Fatalf("expected a missing-key error, got %v", err)
	}
}

// An option source lists pools for every provider on the appliance; a non-MVM
// pool cannot host this provider's VMs.
func TestListResourcePoolsFiltersToMVM(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": 1, "name": "KVM Pool", "providerType": "mvm"},
				{"id": 2, "name": "vSphere Pool", "providerType": "vmware"},
			},
		})
	}))

	pools, err := client.ListResourcePools(t.Context(), 1, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pools) != 1 || pools[0].ID != 1 {
		t.Fatalf("got %+v, want only the MVM pool", pools)
	}
}

func TestCreateVirtualImageWrapsPayload(t *testing.T) {
	var body map[string]any

	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)

		json.NewEncoder(w).Encode(map[string]any{
			"success":      true,
			"virtualImage": map[string]any{"id": 9, "name": "img"},
		})
	}))

	image, err := client.CreateVirtualImage(t.Context(), map[string]any{"name": "img"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if image.ID != 9 {
		t.Errorf("image ID = %d, want 9", image.ID)
	}

	// Morpheus expects the record nested under a virtualImage key.
	if _, ok := body["virtualImage"]; !ok {
		t.Errorf("payload %v is not wrapped in a virtualImage key", body)
	}
}
