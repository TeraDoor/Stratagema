package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Faculty is a deliberately minimal, self-contained re-implementation of
// exactly the subset of the root package's Faculty format (../faculty.go)
// this runner actually needs: Name (for its own status output), Harness
// (ditto), and Body (the prose that becomes the harness prompt -- see
// buildPrompt in main.go). See client.go's doc comment on requireRemoteDB
// for the fuller reasoning; the short version: the root package is
// `package main`, and Go does not allow importing a main package at all,
// from anywhere, under any circumstances -- so "import the root package's
// ParseFacultyFile" was never an option on the table once the repo layout
// was actually checked, regardless of which approach would otherwise have
// been preferred. Duplicating this parser is low-risk, not an ongoing
// maintenance liability: the format is small and already stated as frozen
// in the root parser's own doc comment ("deliberately not a general YAML
// parser"), and this runner only reads two of its five fields.
type Faculty struct {
	Name    string
	Harness string
	Body    string
}

var (
	errMissingOpeningDelimiter = errors.New("missing opening --- delimiter (file must start with a line containing only ---)")
	errMissingClosingDelimiter = errors.New("missing closing --- delimiter (frontmatter block was never closed)")
)

// parseFaculty hand-parses the same flat "key: value" frontmatter block +
// prose body format faculty.go's ParseFaculty does, keeping only the two
// keys (name, harness) this runner has any use for -- every other key
// (capability, tools, core) is real in a faculty file authored for the
// full toolchain and must still parse cleanly here, just unread.
func parseFaculty(data []byte) (*Faculty, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, errMissingOpeningDelimiter
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx == -1 {
		return nil, errMissingClosingDelimiter
	}

	f := &Faculty{}
	for _, raw := range lines[1:closeIdx] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed frontmatter line %q (want \"key: value\")", raw)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "name":
			f.Name = value
		case "harness":
			f.Harness = value
		default:
			// capability, tools, core, and any future key: real fields
			// this runner doesn't need, ignored rather than rejected.
		}
	}
	if f.Name == "" {
		return nil, fmt.Errorf("missing required frontmatter field %q", "name")
	}

	f.Body = strings.TrimSpace(strings.Join(lines[closeIdx+1:], "\n"))
	return f, nil
}

// parseFacultyFile reads path and parses it, wrapping any parse error with
// the path -- mirrors ParseFacultyFile's own error-wrapping in faculty.go.
func parseFacultyFile(path string) (*Faculty, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := parseFaculty(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
