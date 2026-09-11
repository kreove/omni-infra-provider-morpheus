// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package docs exists to hold a test over the repository's Markdown.
//
// The documentation cross-references itself heavily, and a heading that is
// renamed or renumbered breaks every link to it silently: nothing fails to
// build, and the link only misbehaves for a reader who follows it. That has
// happened twice -- an em-dash in a heading generated an anchor no link used,
// and a section renumbered from 5 to 6 left a link behind pointing at the old
// number.
//
// Living here rather than in a script means it runs wherever `go test ./...`
// does, which is locally, in CI, and in the container build.
package docs
