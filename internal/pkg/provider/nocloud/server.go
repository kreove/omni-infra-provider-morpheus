// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package nocloud serves the Talos NoCloud datasource over HTTP.
//
// Morpheus builds the guest's cloud-init drive itself and does not pass
// `config.userData` through to it: it renders its own `#cloud-config` document
// and folds whatever the API was given into that document's `runcmd` list.
// Talos reads a NoCloud user-data whose first line is `#cloud-config` and
// discards it -- silently, as a missing config source rather than an error --
// so a machine provisioned that way boots into maintenance mode and never
// joins Omni.
//
// Talos's NoCloud platform also accepts its datasource over HTTP, selected by
// options in the guest's SMBIOS system serial number:
//
//	ds=nocloud-net;s=http://host:port/nocloud/<token>/
//
// Pointing a machine here therefore bypasses the config drive Morpheus writes,
// without touching the image: the serial is set per VM, so every machine still
// boots the same cached image.
package nocloud

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// NewToken mints an address for one machine's datasource.
//
// It is the only thing guarding the join config, which carries a join token
// good for registering a machine with Omni, so it is random rather than
// derived from the machine request ID: request IDs are predictable, and the
// URL holding this value is readable in the Morpheus UI and the guest's
// SMBIOS.
func NewToken() (string, error) {
	buf := make([]byte, 32)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate a NoCloud config token: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// Machine is one machine's datasource.
type Machine struct {
	// JoinConfig is served as user-data, verbatim. It is the Omni join config,
	// which Talos parses as a machine configuration.
	JoinConfig string

	// Hostname is served as meta-data's local-hostname. Morpheus's own
	// meta-data is not read once a machine uses the network datasource, so
	// without this the guest would come up with no hostname from the platform.
	Hostname string
}

type entry struct {
	machine      Machine
	registeredAt time.Time
	servedAt     time.Time
}

// Status reports a machine's progress through its first boot.
type Status struct {
	// RegisteredAt is when this provider process first published the machine.
	// It resets when the provider restarts, which only extends the window the
	// provisioner is willing to wait, and so is harmless.
	RegisteredAt time.Time

	// ServedAt is when the machine collected its config, or the zero time.
	ServedAt time.Time
}

// Server serves per-machine NoCloud datasources.
type Server struct {
	logger *zap.Logger

	mu       sync.Mutex
	machines map[string]*entry
}

// NewServer creates a NoCloud datasource server.
func NewServer(logger *zap.Logger) *Server {
	return &Server{
		logger:   logger,
		machines: map[string]*entry{},
	}
}

// Register publishes a machine's datasource, replacing any previous one.
//
// Registering is idempotent and is repeated on every reconcile, so that a
// provider that restarted while a machine was still booting republishes the
// entry that machine is asking for.
func (s *Server) Register(token string, machine Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.machines[token]; ok {
		existing.machine = machine

		return
	}

	s.machines[token] = &entry{machine: machine, registeredAt: time.Now()}
}

// Forget drops a machine's datasource, on deprovision.
func (s *Server) Forget(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.machines, token)
}

// Status reports a machine's progress, and whether it is registered at all.
func (s *Server) Status(token string) (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.machines[token]
	if !ok {
		return Status{}, false
	}

	return Status{RegisteredAt: entry.registeredAt, ServedAt: entry.servedAt}, true
}

// MachineURL returns the datasource base URL for a machine, which is what the
// guest's SMBIOS serial has to point at. It ends in a slash because Talos
// concatenates the file name onto it.
func MachineURL(baseURL, token string) string {
	return strings.TrimSuffix(baseURL, "/") + "/nocloud/" + token + "/"
}

// SMBIOSSerial renders the SMBIOS system serial number that sends a guest to
// its datasource here.
func SMBIOSSerial(baseURL, token string) string {
	return "ds=nocloud-net;s=" + MachineURL(baseURL, token)
}

// ValidateBaseURL rejects a datasource URL a guest could not use, at startup
// rather than on the first machine that fails to boot because of it.
func ValidateBaseURL(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("nocloud-server-url %q is not a valid URL: %w", baseURL, err)
	}

	// Talos requires an http(s) scheme before it will treat the SMBIOS option
	// as a datasource at all, and ignores it silently otherwise.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("nocloud-server-url %q must be an http:// or https:// URL", baseURL)
	}

	if parsed.Host == "" {
		return fmt.Errorf("nocloud-server-url %q has no host", baseURL)
	}

	// The value is embedded in the SMBIOS serial as one `;`-separated option.
	// A `;` in the URL itself would split it into two and silently truncate
	// the address.
	if strings.Contains(baseURL, ";") {
		return fmt.Errorf("nocloud-server-url %q must not contain ';'", baseURL)
	}

	return nil
}

// Handler routes the three NoCloud datasource files.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /nocloud/{token}/user-data", s.serveUserData)
	mux.HandleFunc("GET /nocloud/{token}/meta-data", s.serveMetaData)
	mux.HandleFunc("GET /nocloud/{token}/network-config", s.serveNetworkConfig)

	return mux
}

// serveUserData serves the join config, which is the machine configuration
// Talos boots with.
func (s *Server) serveUserData(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	machine, ok := s.lookup(token, true)
	if !ok {
		// Deliberately not a 404. Talos treats 404 on user-data as a definitive
		// "this machine has no config", gives up immediately and drops into
		// maintenance mode, while it retries any other error for three minutes.
		// A machine can legitimately ask before the provider has registered it,
		// so an unknown token has to be retryable.
		s.logger.Warn("NoCloud config requested for an unknown machine", zap.String("remote_addr", r.RemoteAddr))

		http.Error(w, "machine is not registered yet", http.StatusServiceUnavailable)

		return
	}

	s.logger.Info(
		"served Talos machine config",
		zap.String("hostname", machine.Hostname),
		zap.String("remote_addr", r.RemoteAddr),
	)

	writeBody(w, machine.JoinConfig)
}

// serveMetaData serves the identity Talos reports for the machine.
func (s *Server) serveMetaData(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.lookup(r.PathValue("token"), false)
	if !ok {
		http.Error(w, "machine is not registered yet", http.StatusServiceUnavailable)

		return
	}

	writeBody(w, fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", machine.Hostname, machine.Hostname))
}

// serveNetworkConfig serves an empty version 1 network config, which leaves
// every interface on DHCP.
//
// Morpheus attaches the NIC and runs DHCP on the network the Machine Class
// names, so there is nothing for the platform to configure. The file is served
// rather than omitted because a machine that gets no network config at all
// from its platform is harder to tell apart from one whose datasource is
// broken.
func (s *Server) serveNetworkConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.lookup(r.PathValue("token"), false); !ok {
		http.Error(w, "machine is not registered yet", http.StatusServiceUnavailable)

		return
	}

	writeBody(w, "version: 1\n")
}

// lookup resolves a token, optionally recording that the machine collected its
// config.
func (s *Server) lookup(token string, markServed bool) (Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.machines[token]
	if !ok {
		return Machine{}, false
	}

	if markServed && entry.servedAt.IsZero() {
		entry.servedAt = time.Now()
	}

	return entry.machine, true
}

func writeBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Talos reads these once per boot and a stale copy would strand a machine
	// on a config it has already outgrown.
	w.Header().Set("Cache-Control", "no-store")

	fmt.Fprint(w, body)
}
