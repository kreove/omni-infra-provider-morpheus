// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package nocloud

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"
)

const (
	testToken      = "0123456789abcdef"
	testJoinConfig = "apiVersion: v1alpha1\nkind: SideroLinkConfig\napiUrl: https://omni.example.com:8090/\n"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()

	server := NewServer(zaptest.NewLogger(t))
	httpServer := httptest.NewServer(server.Handler())

	t.Cleanup(httpServer.Close)

	return server, httpServer
}

func get(t *testing.T, httpServer *httptest.Server, path string) (int, string) {
	t.Helper()

	resp, err := http.Get(httpServer.URL + path) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return resp.StatusCode, string(body)
}

// Talos parses user-data as a machine config, so anything added to it -- a
// header, a trailing newline, a cloud-config wrapper -- is read as part of the
// config. This is the exact failure this package exists to route around.
func TestServesJoinConfigVerbatim(t *testing.T) {
	server, httpServer := testServer(t)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig, Hostname: "talos-cp-1"})

	status, body := get(t, httpServer, "/nocloud/"+testToken+"/user-data")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	if body != testJoinConfig {
		t.Errorf("user-data = %q, want the join config verbatim", body)
	}
}

// Talos treats 404 on user-data as a definitive "no config exists", gives up
// and drops into maintenance mode, while it retries every other status for
// three minutes. A machine that asks before the provider registered it -- or
// after the provider restarted -- must get a retryable answer, or it is
// stranded until someone reboots it.
func TestUnknownMachineIsRetryable(t *testing.T) {
	_, httpServer := testServer(t)

	status, _ := get(t, httpServer, "/nocloud/unknown/user-data")

	if status == http.StatusNotFound {
		t.Fatal("user-data returned 404 for an unknown machine, which makes Talos give up")
	}

	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
}

func TestForgottenMachineIsRetryable(t *testing.T) {
	server, httpServer := testServer(t)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig})
	server.Forget(testToken)

	if status, _ := get(t, httpServer, "/nocloud/"+testToken+"/user-data"); status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 after the machine was forgotten", status)
	}
}

// Once a machine uses the network datasource it never reads the config drive,
// so the hostname Morpheus wrote there is not applied and has to be served
// here instead.
func TestServesHostnameInMetaData(t *testing.T) {
	server, httpServer := testServer(t)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig, Hostname: "talos-cp-1"})

	status, body := get(t, httpServer, "/nocloud/"+testToken+"/meta-data")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	if !strings.Contains(body, "local-hostname: talos-cp-1") {
		t.Errorf("meta-data = %q, want a local-hostname entry", body)
	}
}

func TestServesNetworkConfig(t *testing.T) {
	server, httpServer := testServer(t)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig})

	status, body := get(t, httpServer, "/nocloud/"+testToken+"/network-config")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	if !strings.Contains(body, "version: 1") {
		t.Errorf("network-config = %q, want a version 1 document", body)
	}
}

// Fetching user-data is the only evidence the provider gets that a machine
// actually received its config, so provisioning waits on it.
func TestRecordsWhenConfigWasFetched(t *testing.T) {
	server, httpServer := testServer(t)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig})

	status, ok := server.Status(testToken)
	if !ok {
		t.Fatal("machine is not registered")
	}

	if !status.ServedAt.IsZero() {
		t.Error("machine is marked served before it fetched anything")
	}

	if status.RegisteredAt.IsZero() {
		t.Error("RegisteredAt is zero after registering")
	}

	get(t, httpServer, "/nocloud/"+testToken+"/user-data")

	if status, _ = server.Status(testToken); status.ServedAt.IsZero() {
		t.Error("machine is not marked served after fetching user-data")
	}
}

// Re-registering happens on every reconcile, and must not reset the record of
// a machine that already booted.
func TestReRegisterKeepsServedAt(t *testing.T) {
	server, httpServer := testServer(t)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig})
	get(t, httpServer, "/nocloud/"+testToken+"/user-data")

	before, _ := server.Status(testToken)

	server.Register(testToken, Machine{JoinConfig: testJoinConfig, Hostname: "talos-cp-1"})

	after, _ := server.Status(testToken)
	if !after.ServedAt.Equal(before.ServedAt) {
		t.Errorf("ServedAt changed on re-register: %v -> %v", before.ServedAt, after.ServedAt)
	}
}

// The token is what guards the join config, which carries a credential for
// registering a machine with Omni.
func TestNewTokenIsRandom(t *testing.T) {
	first, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	second, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	if first == second {
		t.Error("NewToken returned the same value twice")
	}

	if len(first) < 32 {
		t.Errorf("token %q is shorter than expected", first)
	}
}

// Talos splits the SMBIOS serial on ';' and reads each part as one option, so
// the URL must not introduce one, and it only treats the value as a datasource
// at all when the scheme is http or https.
func TestValidateBaseURL(t *testing.T) {
	for name, test := range map[string]struct {
		url     string
		wantErr bool
	}{
		"http":          {url: "http://10.0.0.5:9080", wantErr: false},
		"https":         {url: "https://provider.example.com", wantErr: false},
		"no scheme":     {url: "10.0.0.5:9080", wantErr: true},
		"wrong scheme":  {url: "ftp://10.0.0.5", wantErr: true},
		"no host":       {url: "http://", wantErr: true},
		"has semicolon": {url: "http://10.0.0.5:9080/a;b", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateBaseURL(test.url)

			if test.wantErr && err == nil {
				t.Errorf("ValidateBaseURL(%q) = nil, want an error", test.url)
			}

			if !test.wantErr && err != nil {
				t.Errorf("ValidateBaseURL(%q) = %v", test.url, err)
			}
		})
	}
}

// Talos concatenates the file name onto the base URL, so it has to end in a
// slash however the operator spelled the server URL.
func TestMachineURLEndsInSlash(t *testing.T) {
	for _, base := range []string{"http://10.0.0.5:9080", "http://10.0.0.5:9080/"} {
		got := MachineURL(base, testToken)

		if got != "http://10.0.0.5:9080/nocloud/"+testToken+"/" {
			t.Errorf("MachineURL(%q) = %q", base, got)
		}
	}
}

func TestSMBIOSSerialSelectsNetworkDatasource(t *testing.T) {
	serial := SMBIOSSerial("http://10.0.0.5:9080", testToken)

	// Talos only switches away from the config drive when it sees exactly this.
	if !strings.HasPrefix(serial, "ds=nocloud-net;s=http://") {
		t.Errorf("serial = %q", serial)
	}
}
