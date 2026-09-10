// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package data

import (
	"encoding/json"
	"strings"
	"testing"
)

type schemaProperty struct {
	Type        string                    `json:"type"`
	Description string                    `json:"description"`
	Default     *json.RawMessage          `json:"default"`
	Minimum     *float64                  `json:"minimum"`
	Properties  map[string]schemaProperty `json:"properties"`
}

type schemaDocument struct {
	Properties map[string]schemaProperty `json:"properties"`
	Required   []string                  `json:"required"`
}

func parseSchema(t *testing.T) schemaDocument {
	t.Helper()

	var doc schemaDocument
	if err := json.Unmarshal(Schema, &doc); err != nil {
		t.Fatalf("schema.json does not parse: %v", err)
	}

	return doc
}

func TestSchemaParses(t *testing.T) {
	if len(parseSchema(t).Properties) == 0 {
		t.Fatal("schema declares no properties")
	}
}

// Omni treats an integer property with no default as required, whatever the
// top-level "required" list says. That rejected every Machine Class with
// "config value \".cloud.id\" is required but was not set", for eight separate
// id fields, including ones on objects that are entirely optional.
//
// Strings and booleans are not affected, so this rule is specifically about
// integers. Zero means unset here, matching Ref.IsZero().
func TestEveryIntegerDeclaresADefault(t *testing.T) {
	var walk func(props map[string]schemaProperty, prefix string)

	walk = func(props map[string]schemaProperty, prefix string) {
		for name, prop := range props {
			path := prefix + name

			if len(prop.Properties) > 0 {
				walk(prop.Properties, path+".")

				continue
			}

			if prop.Type == "integer" && prop.Default == nil {
				t.Errorf("%s is an integer with no default; Omni will treat it as required", path)
			}
		}
	}

	walk(parseSchema(t).Properties, "")
}

// A default has to satisfy the field's own constraints, or a strict validator
// rejects the schema before Omni ever renders it.
func TestIntegerDefaultsSatisfyTheirMinimum(t *testing.T) {
	var walk func(props map[string]schemaProperty, prefix string)

	walk = func(props map[string]schemaProperty, prefix string) {
		for name, prop := range props {
			path := prefix + name

			if len(prop.Properties) > 0 {
				walk(prop.Properties, path+".")

				continue
			}

			if prop.Type != "integer" || prop.Default == nil || prop.Minimum == nil {
				continue
			}

			var value float64
			if err := json.Unmarshal(*prop.Default, &value); err != nil {
				t.Errorf("%s has a non-numeric default: %v", path, err)

				continue
			}

			if value < *prop.Minimum {
				t.Errorf("%s defaults to %v, below its minimum of %v", path, value, *prop.Minimum)
			}
		}
	}

	walk(parseSchema(t).Properties, "")
}

// Every object an operator references by id or name must accept both, or the
// id-or-name contract the provider documents does not hold.
func TestRefObjectsAcceptIDAndName(t *testing.T) {
	for _, name := range []string{"cloud", "group", "instance_type", "layout", "plan", "network", "resource_pool", "image"} {
		prop, ok := parseSchema(t).Properties[name]
		if !ok {
			t.Errorf("%s is missing from the schema", name)

			continue
		}

		if _, ok = prop.Properties["id"]; !ok {
			t.Errorf("%s has no id property", name)
		}

		if _, ok = prop.Properties["name"]; !ok {
			t.Errorf("%s has no name property", name)
		}
	}
}

// Omni renders a field's description as a tooltip only for leaf properties.
// A description on an object -- cloud, layout, instance_type -- is not shown
// at all, so guidance written there is invisible to someone filling in the
// form, which is exactly where it is needed. Every leaf therefore carries its
// own.
func TestEveryLeafFieldHasADescription(t *testing.T) {
	var walk func(props map[string]schemaProperty, prefix string)

	walk = func(props map[string]schemaProperty, prefix string) {
		for name, prop := range props {
			path := prefix + name

			if len(prop.Properties) > 0 {
				walk(prop.Properties, path+".")

				continue
			}

			if prop.Description == "" {
				t.Errorf("%s has no description; Omni will render it with no tooltip", path)
			}
		}
	}

	walk(parseSchema(t).Properties, "")
}

// The optional fields are the ones an operator is most likely to fill in
// needlessly, so each has to say it can be left alone.
func TestOptionalFieldsSayTheyAreOptional(t *testing.T) {
	doc := parseSchema(t)

	for _, path := range []string{
		"instance_type.id", "instance_type.name", "instance_type_code",
		"resource_pool.id", "resource_pool.name",
		"image.id", "image.name",
	} {
		parts := strings.SplitN(path, ".", 2)

		prop, ok := doc.Properties[parts[0]]
		if !ok {
			t.Errorf("%s is missing from the schema", path)

			continue
		}

		if len(parts) == 2 {
			if prop, ok = prop.Properties[parts[1]]; !ok {
				t.Errorf("%s is missing from the schema", path)

				continue
			}
		}

		if !strings.HasPrefix(prop.Description, "Optional.") {
			t.Errorf("%s does not start its description with \"Optional.\": %q", path, prop.Description)
		}
	}
}
