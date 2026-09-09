// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Morpheus publishes an official Go SDK (github.com/gomorpheus/morpheus-go-sdk),
// which this provider deliberately does not use. None of its methods accept a
// context.Context, so a provisioning step could neither be cancelled nor
// bounded by a deadline -- an Omni provisioner reconciles on a timer and must
// abandon in-flight work when a step is retried. It also returns results as
// interface{} requiring type assertions at every call site, and takes request
// bodies as untyped maps, which gives up the compile-time checking that is the
// main reason to depend on a client library at all. The API surface this
// provider needs is small enough to state precisely instead.

const (
	// morpheusClientID is the OAuth client Morpheus reserves for API access.
	morpheusClientID = "morph-api"

	defaultRequestTimeout = 2 * time.Minute

	// tokenExpiryGrace renews the token early, so a request is not sent with a
	// token that expires while it is in flight.
	tokenExpiryGrace = 5 * time.Minute
)

// Client is a Morpheus API client.
//
// It authenticates either with a long-lived API token or with a username and
// password exchanged for an OAuth access token. In the second case the token is
// cached, renewed before it expires, and re-fetched once if Morpheus rejects it
// with a 401 -- an appliance restart or a revoked session otherwise breaks the
// provider until the process is restarted by hand.
type Client struct {
	baseURL *url.URL
	http    *http.Client

	// token is set for API-token authentication and never changes.
	token string

	username string
	password string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// ClientConfig configures a Morpheus client.
type ClientConfig struct {
	Endpoint string
	Token    string
	Username string
	Password string
	Insecure bool
}

// NewClient validates the configuration and returns a Morpheus client. It does
// not contact the appliance; the first API call authenticates.
func NewClient(cfg ClientConfig) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("morpheus endpoint is required")
	}

	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}

	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid Morpheus endpoint %q: %w", cfg.Endpoint, err)
	}

	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("Morpheus endpoint must use HTTP or HTTPS, got %q", base.Scheme)
	}

	if base.Host == "" {
		return nil, fmt.Errorf("Morpheus endpoint %q has no host", cfg.Endpoint)
	}

	// A trailing slash on the base would double up when joined with an API
	// path, and Morpheus answers "/api//instances" with a redirect to login.
	base.Path = strings.TrimSuffix(base.Path, "/")

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected default HTTP transport type %T", http.DefaultTransport)
	}

	transport = transport.Clone()
	if cfg.Insecure {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{} //nolint:gosec // opt-in via --morpheus-insecure-skip-verify
		}

		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	client := &Client{
		baseURL:  base,
		http:     &http.Client{Transport: transport},
		token:    strings.TrimSpace(cfg.Token),
		username: strings.TrimSpace(cfg.Username),
		password: cfg.Password,
	}

	if client.token == "" && (client.username == "" || client.password == "") {
		return nil, fmt.Errorf("set MORPHEUS_TOKEN or both MORPHEUS_USERNAME and MORPHEUS_PASSWORD")
	}

	return client, nil
}

// APIError is a structured failure reported by Morpheus.
type APIError struct {
	StatusCode int
	Message    string
	Errors     map[string]string
	Method     string
	Path       string
}

// Error implements error.
func (e *APIError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "morpheus %s %s failed", e.Method, e.Path)

	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " with HTTP %d", e.StatusCode)
	}

	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}

	// Morpheus reports validation failures per field, and the top-level msg is
	// often just "Validation failed". Without these the caller is told nothing
	// about which field Morpheus objected to.
	if len(e.Errors) > 0 {
		fields := make([]string, 0, len(e.Errors))
		for field, msg := range e.Errors {
			fields = append(fields, fmt.Sprintf("%s: %s", field, msg))
		}

		sortStrings(fields)

		fmt.Fprintf(&b, " (%s)", strings.Join(fields, "; "))
	}

	return b.String()
}

// IsNotFound reports whether err is a Morpheus 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}

	return false
}

// isUnauthorized reports whether err means the access token was rejected.
func isUnauthorized(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnauthorized
	}

	return false
}

// standardResponse is the envelope Morpheus wraps most write responses in.
//
// It is decoded on every response because Morpheus reports some failures with
// HTTP 200 and success:false rather than an error status. Treating a 200 as
// successful without reading this field silently accepts failed operations.
type standardResponse struct {
	Success *bool             `json:"success"`
	Message string            `json:"msg"`
	Errors  map[string]string `json:"errors"`
}

// authHeader returns the value for the Authorization header, logging in if
// required.
func (c *Client) authHeader(ctx context.Context) (string, error) {
	if c.token != "" {
		return "Bearer " + c.token, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt.Add(-tokenExpiryGrace)) {
		return "Bearer " + c.accessToken, nil
	}

	if err := c.loginLocked(ctx); err != nil {
		return "", err
	}

	return "Bearer " + c.accessToken, nil
}

// invalidateToken drops a cached access token Morpheus has rejected.
func (c *Client) invalidateToken() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.accessToken = ""
	c.expiresAt = time.Time{}
}

// loginLocked exchanges the username and password for an access token. The
// caller must hold c.mu.
func (c *Client) loginLocked(ctx context.Context) error {
	// The parameter placement mirrors what Morpheus's own clients send:
	// client_id, grant_type, scope and username as query parameters, with only
	// the password form-encoded. Morpheus accepts other arrangements
	// inconsistently across versions, so this follows the combination its
	// official SDK and CLI both use.
	query := url.Values{
		"client_id":  {morpheusClientID},
		"grant_type": {"password"},
		"scope":      {"write"},
		"username":   {c.username},
	}

	form := url.Values{"password": {c.password}}

	endpoint := *c.baseURL
	endpoint.Path = endpoint.Path + "/oauth/token"
	endpoint.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(ctx, defaultRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to authenticate against Morpheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read Morpheus authentication response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// A wrong password comes back as 400 with an OAuth error body, which
		// says nothing about which credential was rejected.
		return &APIError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodPost,
			Path:       "/oauth/token",
			Message: fmt.Sprintf(
				"authentication failed for user %q; check MORPHEUS_USERNAME and MORPHEUS_PASSWORD (%s)",
				c.username, strings.TrimSpace(string(body)),
			),
		}
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to decode Morpheus authentication response: %w", err)
	}

	if result.AccessToken == "" {
		return fmt.Errorf("Morpheus authentication succeeded but returned no access token")
	}

	c.accessToken = result.AccessToken

	// Morpheus tokens are long-lived (a year by default). Cap the assumed
	// lifetime so a token that is revoked server-side is still re-fetched
	// within the day rather than being trusted until the process restarts.
	lifetime := time.Duration(result.ExpiresIn) * time.Second
	if lifetime <= 0 || lifetime > 24*time.Hour {
		lifetime = 24 * time.Hour
	}

	c.expiresAt = time.Now().Add(lifetime)

	return nil
}

// request describes one Morpheus API call.
type request struct {
	method string
	path   string
	query  url.Values
	body   any

	// out, when set, receives the decoded JSON response.
	out any
}

// do executes req, retrying once if the access token was rejected.
func (c *Client) do(ctx context.Context, req request) error {
	err := c.doOnce(ctx, req)
	if !isUnauthorized(err) || c.token != "" {
		return err
	}

	// The cached token was rejected. Drop it and let the retry log in again.
	c.invalidateToken()

	return c.doOnce(ctx, req)
}

func (c *Client) doOnce(ctx context.Context, req request) error {
	auth, err := c.authHeader(ctx)
	if err != nil {
		return err
	}

	var bodyReader io.Reader

	if req.body != nil {
		encoded, merr := json.Marshal(req.body)
		if merr != nil {
			return fmt.Errorf("failed to encode Morpheus request body: %w", merr)
		}

		bodyReader = bytes.NewReader(encoded)
	}

	endpoint := *c.baseURL
	endpoint.Path = endpoint.Path + req.path

	if req.query != nil {
		endpoint.RawQuery = req.query.Encode()
	}

	ctx, cancel := context.WithTimeout(ctx, defaultRequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, req.method, endpoint.String(), bodyReader)
	if err != nil {
		return err
	}

	httpReq.Header.Set("Authorization", auth)
	httpReq.Header.Set("Accept", "application/json")

	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("morpheus %s %s failed: %w", req.method, req.path, err)
	}
	defer resp.Body.Close()

	return c.handleResponse(resp, req)
}

func (c *Client) handleResponse(resp *http.Response, req request) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read Morpheus response for %s %s: %w", req.method, req.path, err)
	}

	var envelope standardResponse

	// A non-JSON body is expected when Morpheus answers with an HTML error
	// page, so a decode failure here is not itself reported.
	_ = json.Unmarshal(body, &envelope) //nolint:errcheck // best-effort envelope decode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    firstNonEmptyString(envelope.Message, summarizeBody(body)),
			Errors:     envelope.Errors,
			Method:     req.method,
			Path:       req.path,
		}
	}

	if envelope.Success != nil && !*envelope.Success {
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    firstNonEmptyString(envelope.Message, "Morpheus reported the request as unsuccessful"),
			Errors:     envelope.Errors,
			Method:     req.method,
			Path:       req.path,
		}
	}

	if req.out == nil {
		return nil
	}

	if err = json.Unmarshal(body, req.out); err != nil {
		return fmt.Errorf("failed to decode Morpheus response for %s %s: %w", req.method, req.path, err)
	}

	return nil
}

// uploadFile streams path to a Morpheus endpoint as an octet-stream.
//
// The image is sent from an open file rather than a buffer so a multi-hundred
// megabyte upload does not have to be held in memory, and so Content-Length can
// be set from the file size -- Morpheus rejects a chunked upload of an image.
func (c *Client) uploadFile(ctx context.Context, path string, query url.Values, filePath string, timeout time.Duration) error {
	auth, err := c.authHeader(ctx)
	if err != nil {
		return err
	}

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open image for upload: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat image for upload: %w", err)
	}

	endpoint := *c.baseURL
	endpoint.Path = endpoint.Path + path

	if query != nil {
		endpoint.RawQuery = query.Encode()
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), file)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = info.Size()

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload image to Morpheus: %w", err)
	}
	defer resp.Body.Close()

	return c.handleResponse(resp, request{method: http.MethodPost, path: path})
}

func summarizeBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}

	const limit = 300
	if len(text) > limit {
		text = text[:limit] + "..."
	}

	return strings.Join(strings.Fields(text), " ")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
