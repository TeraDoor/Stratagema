package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Profile is an agent-role definition: a small, flat frontmatter block plus
// a free-form prose body describing the role's actual behavior. Profiles
// are deliberately not stored in the SQLite database — they're files on
// disk, meant to be read, copied, and hand-edited like any other config a
// human or agent authors directly, not rows behind a CLI's exclusive
// control.
type Profile struct {
	Name       string
	Capability string
	Harness    string
	Tools      []string
	Body       string // everything after the closing ---, trimmed
}

// defaultProfilesDir mirrors defaultDBPath's own resolution order in
// store.go: an explicit env var, else a project-local default.
func defaultProfilesDir() string {
	if v := os.Getenv("STRATAGEMA_PROFILES"); v != "" {
		return v
	}
	return filepath.Join(".stratagema", "profiles")
}

// profilesDirFlag is the profile commands' equivalent of dbPathFlag in
// cli.go — same override pattern, different default.
func profilesDirFlag(fs *flag.FlagSet) *string {
	return fs.String("profiles-dir", "", "profiles directory (default: $STRATAGEMA_PROFILES, else ./.stratagema/profiles)")
}

func resolveProfilesDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return defaultProfilesDir()
}

// Frontmatter parse errors are real and specific, not a generic "invalid
// profile file" — matching this codebase's existing style (see
// validResourceName, ErrLockHeld) of naming exactly what's wrong.
var (
	ErrMissingOpeningDelimiter = errors.New("missing opening --- delimiter (file must start with a line containing only ---)")
	ErrMissingClosingDelimiter = errors.New("missing closing --- delimiter (frontmatter block was never closed)")
	ErrToolsNotBracketList     = errors.New(`tools value must be a bracketed list, e.g. tools: [read_file, write_file]`)
	ErrToolsEmpty              = errors.New("tools list must not be empty")
)

// missingFieldError reports a required frontmatter key that was absent or
// blank — real and specific about which key, not a generic failure.
func missingFieldError(key string) error {
	return fmt.Errorf("missing required frontmatter field %q", key)
}

// validProfileName guards the one place a user-supplied name is turned
// directly into a file path (profile create's -name, profile show's
// positional name): filepath.Join does not stop a path-separator-bearing
// name from walking outside the profiles directory. Verified directly —
// before this check existed, `profile create -name=../evil` wrote
// evil.md one directory above the intended profiles dir, not inside it.
// Same "reject the dangerous characters at the one point a name is first
// written" pattern as validResourceName in lock.go, adapted for a
// filesystem name rather than a database column: a bare ".." or "."
// is harmless here (the ".md" suffix this name is always joined with
// turns it into "...md" / "..md", an ordinary filename, not a parent-dir
// token), so only an embedded separator — the thing that can introduce a
// new, standalone ".." path segment — needs rejecting.
func validProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("profile name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("profile name must not contain a path separator: %q", name)
	}
	return nil
}

// ParseProfile hand-parses exactly the frontmatter subset this project
// actually uses: a --- delimited block of flat "key: value" lines, where
// one key (tools) has a bracketed list value like [a, b, c], followed by a
// prose body. Deliberately not a general YAML parser — this format has no
// nesting, no multi-line values, no quoting rules beyond what's needed
// here.
func ParseProfile(data []byte) (*Profile, error) {
	// Normalize CRLF up front so a Windows-authored file parses the same
	// way as a Unix one; everything below only ever sees \n.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, ErrMissingOpeningDelimiter
	}

	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx == -1 {
		return nil, ErrMissingClosingDelimiter
	}

	f := &Profile{}
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
		case "capability":
			f.Capability = value
		case "harness":
			f.Harness = value
		case "tools":
			tools, err := parseToolsList(value)
			if err != nil {
				return nil, err
			}
			f.Tools = tools
		default:
			// Unknown keys are ignored rather than rejected: this format
			// is deliberately small and flat, and a future field showing
			// up in a hand-authored file shouldn't hard-fail every
			// existing tool that hasn't learned it yet. This is also what
			// makes a hand-authored file that still has a leftover "core:"
			// line (written before that field was cut, S085) continue to
			// parse cleanly — it's just silently unrecognized now, not an
			// error.
		}
	}

	if f.Name == "" {
		return nil, missingFieldError("name")
	}
	if f.Capability == "" {
		return nil, missingFieldError("capability")
	}
	if f.Harness == "" {
		return nil, missingFieldError("harness")
	}
	if f.Tools == nil {
		return nil, missingFieldError("tools")
	}

	f.Body = strings.TrimSpace(strings.Join(lines[closeIdx+1:], "\n"))
	return f, nil
}

// parseToolsList parses the one non-scalar value this format has:
// "[a, b, c]". Anything not wrapped in brackets, or an empty list, is a
// real parse error rather than being silently treated as "no tools" —
// a Profile with no tool access is worth stating explicitly, and this
// format doesn't have a way to state it (an explicit empty list reads as
// author error, not intent, given every hand-written example declares at
// least one tool).
func parseToolsList(value string) ([]string, error) {
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, ErrToolsNotBracketList
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return nil, ErrToolsEmpty
	}
	parts := strings.Split(inner, ",")
	tools := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			return nil, fmt.Errorf("tools list contains an empty entry: %q", value)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// ParseProfileFile reads path and parses it, wrapping any parse error with
// the path so a caller scanning many files (profile list) can report
// exactly which file failed and why.
func ParseProfileFile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := ParseProfile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// render produces the on-disk frontmatter + body form of f, the inverse of
// ParseProfile for the fields profile create writes.
func (f *Profile) render() string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", f.Name)
	fmt.Fprintf(&b, "capability: %s\n", f.Capability)
	fmt.Fprintf(&b, "harness: %s\n", f.Harness)
	fmt.Fprintf(&b, "tools: [%s]\n", strings.Join(f.Tools, ", "))
	b.WriteString("---\n\n")
	b.WriteString(f.Body)
	b.WriteString("\n")
	return b.String()
}

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdProfile(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema profile <create|list|show> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return cmdProfileCreate(rest)
	case "list":
		return cmdProfileList(rest)
	case "show":
		return cmdProfileShow(rest)
	default:
		return die(2, "profile: unknown subcommand %q (create|list|show)", sub)
	}
}

func cmdProfileCreate(args []string) int {
	fs := flag.NewFlagSet("profile create", flag.ExitOnError)
	dirFlag := profilesDirFlag(fs)
	name := fs.String("name", "", "profile name, used as the filename <name>.md (required)")
	capability := fs.String("capability", "", "short label for what this role is for, e.g. go-development (required)")
	harness := fs.String("harness", "", "the agent harness this role runs under, e.g. claude-code (required)")
	tools := fs.String("tools", "", "comma-separated tool list, e.g. read_file,write_file,exec (required)")
	fs.Parse(args)

	if *name == "" || *capability == "" || *harness == "" || *tools == "" {
		return die(1, "profile create: -name, -capability, -harness, and -tools are all required")
	}
	if err := validProfileName(*name); err != nil {
		return die(1, "profile create: %v", err)
	}

	toolList := strings.Split(*tools, ",")
	for i, t := range toolList {
		toolList[i] = strings.TrimSpace(t)
		if toolList[i] == "" {
			return die(1, "profile create: -tools contains an empty entry: %q", *tools)
		}
	}

	dir := resolveProfilesDir(*dirFlag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return die(1, "profile create: %v", err)
	}
	path := filepath.Join(dir, *name+".md")
	if _, err := os.Stat(path); err == nil {
		return die(1, "profile create: %s already exists, refusing to overwrite", path)
	} else if !os.IsNotExist(err) {
		return die(1, "profile create: %v", err)
	}

	f := &Profile{
		Name:       *name,
		Capability: *capability,
		Harness:    *harness,
		Tools:      toolList,
		Body: fmt.Sprintf(
			"Describe %s's actual behavior here: what it does, what it deliberately\n"+
				"does not do, and how it should coordinate with other Profiles sharing\n"+
				"this project. This placeholder is not a real role definition — replace\n"+
				"it before this Profile is used.",
			*name,
		),
	}

	// O_EXCL as the real enforcement, not just the Stat check above, which
	// leaves a TOCTOU gap between the check and the write.
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return die(1, "profile create: %s already exists, refusing to overwrite", path)
		}
		return die(1, "profile create: %v", err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(f.render()); err != nil {
		return die(1, "profile create: %v", err)
	}

	fmt.Printf("created %s\n  capability: %s\n  harness:    %s\n  tools:      %s\n\n  its body is a placeholder -- open %s and replace it with\n  %s's actual behavior before dispatching this Profile.\n", path, f.Capability, f.Harness, strings.Join(f.Tools, ", "), path, f.Name)
	return 0
}

func cmdProfileList(args []string) int {
	fs := flag.NewFlagSet("profile list", flag.ExitOnError)
	dirFlag := profilesDirFlag(fs)
	fs.Parse(args)

	dir := resolveProfilesDir(*dirFlag)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no profiles")
			return 0
		}
		return die(1, "profile list: %v", err)
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	if len(paths) == 0 {
		fmt.Println("no profiles")
		return 0
	}

	// A file that fails to parse is reported inline, clearly, and does not
	// stop the rest of the listing or crash the command — one hand-edited
	// bad file shouldn't hide every other profile that's fine.
	exit := 0
	for _, p := range paths {
		f, err := ParseProfileFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stratagema: profile list: skipping %v\n", err)
			exit = 1
			continue
		}
		fmt.Printf("%-20s  %-24s  %s\n", f.Name, f.Capability, f.Harness)
	}
	return exit
}

func cmdProfileShow(args []string) int {
	fs := flag.NewFlagSet("profile show", flag.ExitOnError)
	dirFlag := profilesDirFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 1 {
		return die(1, "profile show: exactly one profile name is required, e.g. `stratagema profile show builder`")
	}
	name := rest[0]
	if err := validProfileName(name); err != nil {
		return die(1, "profile show: %v", err)
	}

	dir := resolveProfilesDir(*dirFlag)
	path := filepath.Join(dir, name+".md")
	f, err := ParseProfileFile(path)
	if err != nil {
		return die(1, "profile show: %v", err)
	}

	fmt.Printf("name:       %s\n", f.Name)
	fmt.Printf("capability: %s\n", f.Capability)
	fmt.Printf("harness:    %s\n", f.Harness)
	fmt.Printf("tools:      %s\n", strings.Join(f.Tools, ", "))
	fmt.Println()
	fmt.Println(f.Body)
	return 0
}
