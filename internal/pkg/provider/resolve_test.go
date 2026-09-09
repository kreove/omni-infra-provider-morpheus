// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"strings"
	"testing"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
)

var testObjects = []NamedObject{
	{ID: 1, Name: "Production", Code: "prod"},
	{ID: 2, Name: "Staging", Code: "stage"},
	{ID: 3, Name: "Duplicate"},
	{ID: 4, Name: "Duplicate"},
}

func TestMatchRefByID(t *testing.T) {
	got, err := matchRef(data.Ref{ID: 2}, testObjects, "cloud")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Name != "Staging" {
		t.Errorf("matched %q, want Staging", got.Name)
	}
}

// The ID is documented as authoritative, so a stale or mismatched name
// alongside it must not change the result.
func TestMatchRefPrefersIDOverName(t *testing.T) {
	got, err := matchRef(data.Ref{ID: 1, Name: "Staging"}, testObjects, "cloud")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.ID != 1 || got.Name != "Production" {
		t.Errorf("matched %+v, want the object with id 1", got)
	}
}

func TestMatchRefByName(t *testing.T) {
	got, err := matchRef(data.Ref{Name: "Production"}, testObjects, "cloud")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.ID != 1 {
		t.Errorf("matched id %d, want 1", got.ID)
	}
}

func TestMatchRefByNameIsCaseInsensitive(t *testing.T) {
	got, err := matchRef(data.Ref{Name: "pRoDuCtIoN"}, testObjects, "cloud")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.ID != 1 {
		t.Errorf("matched id %d, want 1", got.ID)
	}
}

func TestMatchRefSurfacesAvailableOptions(t *testing.T) {
	_, err := matchRef(data.Ref{Name: "Nonexistent"}, testObjects, "layout")
	if err == nil {
		t.Fatal("expected an error")
	}

	// The whole point of the listing is that an operator can fix the Machine
	// Class from the error alone.
	for _, want := range []string{"layout", "Nonexistent", "Production", "id 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestMatchRefRejectsMissingID(t *testing.T) {
	_, err := matchRef(data.Ref{ID: 99}, testObjects, "plan")
	if err == nil || !strings.Contains(err.Error(), "id 99 does not exist") {
		t.Fatalf("expected a missing-id error, got %v", err)
	}
}

// Morpheus does not enforce unique names, and picking the first match would
// make provisioning depend on listing order.
func TestMatchRefRejectsAmbiguousName(t *testing.T) {
	_, err := matchRef(data.Ref{Name: "Duplicate"}, testObjects, "network")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected an ambiguity error, got %v", err)
	}

	if !strings.Contains(err.Error(), "set an id instead") {
		t.Errorf("error should suggest using an id, got %q", err.Error())
	}
}

func TestMatchRefRejectsEmptyRef(t *testing.T) {
	_, err := matchRef(data.Ref{}, testObjects, "group")
	if err == nil || !strings.Contains(err.Error(), "must set either id or name") {
		t.Fatalf("expected an empty-ref error, got %v", err)
	}
}

func TestMatchCode(t *testing.T) {
	got, err := matchCode("stage", testObjects)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.ID != 2 {
		t.Errorf("matched id %d, want 2", got.ID)
	}
}

func TestMatchCodeRejectsUnknown(t *testing.T) {
	_, err := matchCode("nope", testObjects)
	if err == nil || !strings.Contains(err.Error(), "instance_type_code") {
		t.Fatalf("expected an instance_type_code error, got %v", err)
	}
}
