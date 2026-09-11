// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// Markdown inline links: [text](target). Reference-style links and bare
	// URLs are not used in this repository.
	linkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

	headingPattern = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)

	fencePattern = regexp.MustCompile("(?s)```.*?```")

	// GitHub drops everything from a heading that is not a letter, digit,
	// underscore, space or hyphen, then lowercases and turns spaces into
	// hyphens.
	anchorDropPattern = regexp.MustCompile(`[^\w\s-]`)
)

// anchorFor renders the fragment GitHub generates for a heading.
func anchorFor(heading string) string {
	cleaned := anchorDropPattern.ReplaceAllString(heading, "")

	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(cleaned)), " ", "-")
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to determine the working directory: %v", err)
	}

	for {
		if _, err = os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("failed to find the module root above the test directory")
		}

		dir = parent
	}
}

// markdownFiles lists the repository's Markdown, excluding anything vendored.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()

	var files []string

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "_out":
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk the repository: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("found no Markdown files, which means this test is not checking anything")
	}

	return files
}

// anchorsIn returns every fragment a file's headings can be linked to.
func anchorsIn(t *testing.T, path string) map[string]bool {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}

	anchors := map[string]bool{}

	// Headings inside a fenced block are illustrative, not real.
	for _, match := range headingPattern.FindAllStringSubmatch(fencePattern.ReplaceAllString(string(body), ""), -1) {
		anchors[anchorFor(match[1])] = true
	}

	return anchors
}

// TestDocumentationLinksResolve checks every relative link between Markdown
// files, and every heading anchor it points at.
//
// External links are not checked: they fail for reasons outside this
// repository and would make the test flaky.
func TestDocumentationLinksResolve(t *testing.T) {
	root := repoRoot(t)
	files := markdownFiles(t, root)

	anchors := map[string]map[string]bool{}
	for _, file := range files {
		anchors[file] = anchorsIn(t, file)
	}

	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("failed to read %s: %v", file, err)
		}

		relative, err := filepath.Rel(root, file)
		if err != nil {
			relative = file
		}

		// Links inside a fenced block are examples, not navigation.
		for _, match := range linkPattern.FindAllStringSubmatch(fencePattern.ReplaceAllString(string(body), ""), -1) {
			checkLink(t, root, file, relative, match[1], anchors)
		}
	}
}

func checkLink(t *testing.T, root, file, relative, target string, anchors map[string]map[string]bool) {
	t.Helper()

	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
		return
	}

	path, fragment, _ := strings.Cut(target, "#")

	// A bare fragment points within the same file.
	resolved := file
	if path != "" {
		resolved = filepath.Join(filepath.Dir(file), path)

		if _, err := os.Stat(resolved); err != nil {
			t.Errorf("%s links to %q, which does not exist", relative, target)

			return
		}
	}

	if fragment == "" {
		return
	}

	// Only Markdown has headings to anchor against.
	if !strings.HasSuffix(resolved, ".md") {
		return
	}

	known, ok := anchors[resolved]
	if !ok {
		known = anchorsIn(t, resolved)
		anchors[resolved] = known
	}

	if !known[fragment] {
		t.Errorf("%s links to %q, but no heading in the target generates that anchor", relative, target)
	}
}
