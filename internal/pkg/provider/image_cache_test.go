// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

// cacheServer stands in for Morpheus's virtual image endpoints and the image
// factory at once, and records every call so a test can assert on what the
// provider did rather than only on what it returned.
type cacheServer struct {
	mu       sync.Mutex
	calls    []string
	images   map[int]map[string]any // GET /api/virtual-images/{id} bodies
	byName   []map[string]any       // GET /api/virtual-images?name= result
	nextID   int
	deleteRC int // non-zero forces DELETE to fail with this status
	// afterUpload is the status the record reports once its file is uploaded.
	afterUpload string
	url         string
}

func newCacheServer(t *testing.T) (*cacheServer, *Provisioner) {
	t.Helper()

	s := &cacheServer{images: map[int]map[string]any{}, nextID: 8, afterUpload: "Active"}

	server := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(server.Close)

	s.url = server.URL

	client, err := NewClient(ClientConfig{Endpoint: server.URL, Token: "t"})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	return s, NewProvisioner(client, nil, "")
}

func (s *cacheServer) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
}

func (s *cacheServer) handle(w http.ResponseWriter, r *http.Request) {
	s.record(r)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.URL.Path == "/image":
		io.WriteString(w, "not really a qcow2")
	case r.Method == http.MethodGet && r.URL.Path == "/api/virtual-images":
		json.NewEncoder(w).Encode(map[string]any{"virtualImages": s.byName})
	case r.Method == http.MethodPost && r.URL.Path == "/api/virtual-images":
		id := s.nextID
		s.nextID++
		// Freshly created: no file yet. The upload adds one.
		s.images[id] = map[string]any{"virtualImage": map[string]any{"id": id, "status": ""}}
		json.NewEncoder(w).Encode(s.images[id])
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/upload"):
		io.Copy(io.Discard, r.Body)

		for _, image := range s.images {
			if strings.Contains(r.URL.Path, "/"+itoa(image["virtualImage"].(map[string]any)["id"].(int))+"/") {
				image["virtualImage"].(map[string]any)["status"] = s.afterUpload
				image["cloudFiles"] = []map[string]any{{"name": "omni-talos.qcow2", "contentLength": 18}}
			}
		}

		json.NewEncoder(w).Encode(map[string]any{"success": true})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/virtual-images/"):
		if s.deleteRC != 0 {
			w.WriteHeader(s.deleteRC)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "msg": "image is in use"})

			return
		}

		json.NewEncoder(w).Encode(map[string]any{"success": true})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/virtual-images/"):
		id, _ := parseImageID(strings.TrimPrefix(r.URL.Path, "/api/virtual-images/"))
		if body, ok := s.images[id]; ok {
			json.NewEncoder(w).Encode(body)

			return
		}

		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *cacheServer) called(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0

	for _, call := range s.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}

	return n
}

// leftover registers a cached image record in the state an interrupted upload
// leaves it: named, present in name lookups, and without a file.
func (s *cacheServer) leftover(id int, name string, files []map[string]any, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body := map[string]any{"virtualImage": map[string]any{"id": id, "name": name, "status": status}}
	if files != nil {
		body["cloudFiles"] = files
	}

	s.images[id] = body
	s.byName = []map[string]any{{"id": id, "name": name, "status": status}}
}

func TestGetVirtualImageReadsFilesBesideTheRecord(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "omni-talos-x", []map[string]any{{"name": "omni-talos-x.qcow2", "contentLength": 232841216}}, "Active")

	image, err := p.client.GetVirtualImage(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	if !image.HasFile() {
		t.Fatalf("expected the file list to be read from cloudFiles, got %+v", image.Files)
	}
}

func TestGetVirtualImageFallsBackToFilesKey(t *testing.T) {
	s, p := newCacheServer(t)
	s.images[7] = map[string]any{
		"virtualImage": map[string]any{"id": 7, "name": "omni-talos-x"},
		"files":        []map[string]any{{"name": "omni-talos-x.qcow2", "size": 1}},
	}

	image, err := p.client.GetVirtualImage(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	if !image.HasFile() {
		t.Fatal("expected the older files key to be honoured")
	}
}

func TestHasFileIgnoresNamelessEntries(t *testing.T) {
	if (&VirtualImage{Files: []VirtualImageFile{{Name: " "}}}).HasFile() {
		t.Fatal("a blank entry is not a file")
	}

	if (&VirtualImage{}).HasFile() || (*VirtualImage)(nil).HasFile() {
		t.Fatal("no entries is no file")
	}
}

// The case observed live: a record named for the cache with no file behind
// it, every machine on that Talos version failing with "Cloud files could not
// be found". The provider must notice, remove it, and import again.
func TestEnsureCachedImageReplacesARecordWithoutAFile(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "omni-talos-x", nil, "Active")

	source := imageSource{url: s.url + "/image", schematicID: "abc", talosVersion: "v1.14.0"}
	providerData := data.Data{ImageFormat: imageFormatQcow2}

	id, ready, err := p.ensureCachedImage(context.Background(), zap.NewNop(), providerData, source, "omni-talos-x")
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	if ready || id != 0 {
		t.Fatalf("the leftover must not be handed out: got id %d ready %v", id, ready)
	}

	if n := s.called("DELETE /api/virtual-images/7"); n != 1 {
		t.Fatalf("expected the empty record to be deleted once, got %d", n)
	}

	deadline := time.Now().Add(10 * time.Second)

	for !ready {
		if time.Now().After(deadline) {
			t.Fatal("re-import did not finish")
		}

		time.Sleep(50 * time.Millisecond)

		id, ready, err = p.ensureCachedImage(context.Background(), zap.NewNop(), providerData, source, "omni-talos-x")
		if err != nil {
			t.Fatalf("waiting for re-import: %v", err)
		}
	}

	if id != 8 {
		t.Fatalf("expected the re-imported image (id 8), got %d", id)
	}

	if s.called("POST /api/virtual-images/8/upload") != 1 {
		t.Fatal("expected a fresh upload")
	}
}

func TestEnsureCachedImageReplacesAFailedRecord(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "omni-talos-x", []map[string]any{{"name": "omni-talos-x.qcow2", "contentLength": 1}}, "Failed")

	_, ready, err := p.ensureCachedImage(context.Background(), zap.NewNop(), data.Data{ImageFormat: imageFormatQcow2},
		imageSource{url: s.url + "/image"}, "omni-talos-x")
	if err != nil || ready {
		t.Fatalf("expected a re-import to start, got ready %v err %v", ready, err)
	}

	if s.called("DELETE /api/virtual-images/7") != 1 {
		t.Fatal("expected the failed record to be deleted")
	}
}

// A healthy cached image is reused untouched: no delete, no upload.
func TestEnsureCachedImageReusesAHealthyRecord(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "omni-talos-x", []map[string]any{{"name": "omni-talos-x.qcow2", "contentLength": 1}}, "Active")

	id, ready, err := p.ensureCachedImage(context.Background(), zap.NewNop(), data.Data{ImageFormat: imageFormatQcow2},
		imageSource{url: s.url + "/image"}, "omni-talos-x")
	if err != nil || !ready || id != 7 {
		t.Fatalf("expected the cached image to be reused: id %d ready %v err %v", id, ready, err)
	}

	if s.called("DELETE ") != 0 || s.called("POST ") != 0 {
		t.Fatalf("a healthy image must not be touched, calls: %v", s.calls)
	}
}

// If the unusable record cannot be removed, the error says so and names the
// manual fix, rather than silently trying to import a duplicate name.
func TestEnsureCachedImageReportsAnUnremovableRecord(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "omni-talos-x", nil, "Active")
	s.deleteRC = http.StatusConflict

	_, _, err := p.ensureCachedImage(context.Background(), zap.NewNop(), data.Data{ImageFormat: imageFormatQcow2},
		imageSource{url: s.url + "/image"}, "omni-talos-x")
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, want := range []string{"no image file", "delete it in Morpheus", "image is in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	if s.called("POST /api/virtual-images") != 0 {
		t.Fatal("must not import behind an unremovable record")
	}
}

// An import whose record Morpheus then marks failed must not leave that record
// behind for the cache lookup to find.
func TestImportImageDiscardsARecordThatFailsProcessing(t *testing.T) {
	s, p := newCacheServer(t)
	s.afterUpload = "failed"

	_, err := p.importImage(context.Background(), data.Data{ImageFormat: imageFormatQcow2},
		imageSource{url: s.url + "/image"}, "omni-talos-x")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("expected the processing failure to be reported, got %v", err)
	}

	if s.called("DELETE /api/virtual-images/8") != 1 {
		t.Fatalf("expected the failed record to be removed, calls: %v", s.calls)
	}
}

// A pinned image is the operator's: a missing file is reported, never fixed
// by deleting their image.
func TestResolveExistingImageRejectsAnImageWithoutAFile(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "talos-custom", nil, "Active")

	_, err := p.resolveExistingImage(context.Background(), data.Ref{Name: "talos-custom"})
	if err == nil || !strings.Contains(err.Error(), "has no image file") {
		t.Fatalf("expected the missing file to be reported, got %v", err)
	}

	if s.called("DELETE ") != 0 {
		t.Fatal("a pinned image must never be deleted")
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatal("this is a provider check, not an API failure")
	}
}

func TestResolveExistingImageReadsByIDAfterNameMatch(t *testing.T) {
	s, p := newCacheServer(t)
	s.leftover(7, "talos-custom", []map[string]any{{"name": "talos-custom.qcow2", "contentLength": 1}}, "Active")

	image, err := p.resolveExistingImage(context.Background(), data.Ref{Name: "talos-custom"})
	if err != nil {
		t.Fatal(err)
	}

	if image.ID != 7 || !image.HasFile() {
		t.Fatalf("expected image 7 with its file list, got %+v", image)
	}
}
