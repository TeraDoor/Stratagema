package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	StrategyPlanning  = "planning"
	StrategyActive    = "active"
	StrategyObserving = "observing"
	StrategyClosed    = "closed"
)

// validStrategyStatuses is checked at every write of a strategy's status —
// SetStrategyStatus and the close path both go through it, so an unknown
// status can never land in the database in the first place.
var validStrategyStatuses = map[string]bool{
	StrategyPlanning:  true,
	StrategyActive:    true,
	StrategyObserving: true,
	StrategyClosed:    true,
}

// validStrategyEventKinds is checked the same way, at strategy log's one
// write path — an unknown kind is rejected before it's ever appended.
//
// resource_usage logs real spend (tokens, dollars, wall time) against a
// strategy — see log-usage/usage for the structured writer/reader pair
// built around it. Manual until real per-invocation reporting exists per
// harness; carries no enforcement, matching the soft-reporting-first
// lean this project settled on before building any budget enforcement.
//
// external_signal was tried and cut (S079/S081): schema-only from the
// start, meant to record a real external user's reaction the day one
// occurred, but it never had a real writer and — unlike resource_usage,
// which gained one (log-usage/usage) — nothing has changed that. A
// classical-computing-analysis pass independently flagged it as
// speculative, unprompted, at the same time the operator flagged this
// project's own pattern of adding schema ahead of real use. Re-adding it
// is a one-line change if a real external reaction ever needs recording;
// until then, carrying an unused kind was the wrong default.
var validStrategyEventKinds = map[string]bool{
	"step_started":   true,
	"step_completed": true,
	"finding":        true,
	"decision":       true,
	"reflection":     true,
	"resource_usage": true,
}

// Strategy is the durable header for one line of work multiple agents
// might touch over time: what it is, what it's for, and whether it's
// still open. It doesn't decide anything and doesn't drive anything —
// see strategy_events for the actual append-only record of what
// happened; this row is just what a human or a Tactic reads first to
// know what a strategy_events log even belongs to.
type Strategy struct {
	ID        string
	Name      string
	Thesis    string
	Status    string
	CreatedAt time.Time
	ClosedAt  *time.Time
	Group     string // optional: several strategies belonging to one larger effort, "" if unset
}

// StrategyEvent is one append-only entry in a strategy's log — a finding,
// a decision, a reflection, or a step boundary, written by whichever
// identity (often a Tactic) was doing the work at the time. Never
// updated or deleted once written; LogStrategyEvent is the only write
// path and it only ever inserts.
type StrategyEvent struct {
	ID         string
	StrategyID string
	Kind       string // step_started | step_completed | finding | decision | reflection | resource_usage
	IdentityID string
	Note       string
	Ts         time.Time
}

func (s *Store) CreateStrategy(name, thesis string) (*Strategy, error) {
	id, err := newID("strategy")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := s.exec(
		`INSERT INTO strategies (id, name, thesis, status, created_at, closed_at) VALUES (?, ?, ?, ?, ?, NULL)`,
		id, name, thesis, StrategyPlanning, now,
	); err != nil {
		return nil, err
	}
	return &Strategy{ID: id, Name: name, Thesis: thesis, Status: StrategyPlanning, CreatedAt: time.UnixMilli(now)}, nil
}

// GetStrategy returns nil, nil if id doesn't exist — same not-found
// pattern as GetLock, not an error, since "does this strategy exist" is
// a normal question to ask, not a failure.
func (s *Store) GetStrategy(id string) (*Strategy, error) {
	row := s.db.QueryRow(`SELECT id, name, thesis, status, created_at, closed_at, group_name FROM strategies WHERE id = ?`, id)
	st, err := scanStrategy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

func (s *Store) ListStrategies() ([]*Strategy, error) {
	rows, err := s.db.Query(`SELECT id, name, thesis, status, created_at, closed_at, group_name FROM strategies ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Strategy
	for rows.Next() {
		st, err := scanStrategy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func scanStrategy(sc rowScanner) (*Strategy, error) {
	var st Strategy
	var createdAt int64
	var closedAt sql.NullInt64
	var group sql.NullString
	if err := sc.Scan(&st.ID, &st.Name, &st.Thesis, &st.Status, &createdAt, &closedAt, &group); err != nil {
		return nil, err
	}
	st.CreatedAt = time.UnixMilli(createdAt)
	if closedAt.Valid {
		t := time.UnixMilli(closedAt.Int64)
		st.ClosedAt = &t
	}
	if group.Valid {
		st.Group = group.String
	}
	return &st, nil
}

// SetStrategyStatus transitions id to status, erroring if id doesn't
// exist (same pattern as SetInterestStatus) or if status isn't one of
// the four valid values. Closing a strategy through here alone does not
// set closed_at or log anything — that's CloseStrategy's job, since a
// close always needs to leave a real event behind, not just a status
// flip; this function is for planning/active/observing transitions.
func (s *Store) SetStrategyStatus(id, status string) error {
	if !validStrategyStatuses[status] {
		return fmt.Errorf("strategy: unknown status %q (want planning|active|observing|closed)", status)
	}
	res, err := s.exec(`UPDATE strategies SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("strategy %s not found", id)
	}
	return nil
}

// SetStrategyGroup sets id's optional group label, erroring if id doesn't
// exist (same not-found pattern as SetStrategyStatus). Deliberately a
// separate call rather than a CreateStrategy parameter -- see
// cmdStrategyCreate, which calls this right after CreateStrategy when
// -group is given, so CreateStrategy's existing signature (and its many
// call sites) never has to change.
func (s *Store) SetStrategyGroup(id, group string) error {
	res, err := s.exec(`UPDATE strategies SET group_name = ? WHERE id = ?`, group, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("strategy %s not found", id)
	}
	return nil
}

// CloseStrategy sets status to closed, stamps closed_at, and logs a
// final reflection-kind event carrying the outcome note — a strategy
// never goes quiet with just a status flip and no trace of why.
func (s *Store) CloseStrategy(id, identityID, outcome string) error {
	now := time.Now().UnixMilli()
	res, err := s.exec(`UPDATE strategies SET status = ?, closed_at = ? WHERE id = ?`, StrategyClosed, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("strategy %s not found", id)
	}
	_, err = s.LogStrategyEvent(id, identityID, "reflection", outcome)
	return err
}

// LogStrategyEvent is the one append-only write path for a strategy's
// log: it only ever inserts a new row, never updates or deletes an
// existing one, and errors cleanly if strategyID doesn't exist or kind
// isn't recognized, rather than silently appending to a strategy that
// was never created.
func (s *Store) LogStrategyEvent(strategyID, identityID, kind, note string) (*StrategyEvent, error) {
	if !validStrategyEventKinds[kind] {
		return nil, fmt.Errorf("strategy log: unknown kind %q (want step_started|step_completed|finding|decision|reflection|resource_usage)", kind)
	}
	existing, err := s.GetStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("strategy %s not found", strategyID)
	}

	id, err := newID("stratevt")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := s.exec(
		`INSERT INTO strategy_events (id, strategy_id, kind, identity_id, note, ts) VALUES (?, ?, ?, ?, ?, ?)`,
		id, strategyID, kind, identityID, note, now,
	); err != nil {
		return nil, err
	}
	return &StrategyEvent{ID: id, StrategyID: strategyID, Kind: kind, IdentityID: identityID, Note: note, Ts: time.UnixMilli(now)}, nil
}

// ListStrategyEvents returns every event for strategyID in the order
// they actually happened. Ordered by rowid, not ts — the same fix this
// codebase already paid to learn once in matchAndPropagate
// (interest.go): wall-clock ts is vulnerable to clock skew between
// writers (a process with a fast or wrong clock can otherwise jump
// ahead of every real, subsequent event), while rowid is SQLite's own
// monotonic insert order and is immune to that by construction. Errors
// cleanly if strategyID doesn't exist, rather than silently returning
// an empty log for a typo'd id.
func (s *Store) ListStrategyEvents(strategyID string) ([]*StrategyEvent, error) {
	existing, err := s.GetStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("strategy %s not found", strategyID)
	}

	rows, err := s.db.Query(
		`SELECT id, strategy_id, kind, identity_id, note, ts FROM strategy_events WHERE strategy_id = ? ORDER BY rowid`,
		strategyID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*StrategyEvent
	for rows.Next() {
		var ev StrategyEvent
		var ts int64
		if err := rows.Scan(&ev.ID, &ev.StrategyID, &ev.Kind, &ev.IdentityID, &ev.Note, &ts); err != nil {
			return nil, err
		}
		ev.Ts = time.UnixMilli(ts)
		out = append(out, &ev)
	}
	return out, rows.Err()
}

// recentEventsCap is how many trailing events `strategy next` shows as
// "recent" — the concise recap an agent resuming work actually needs,
// not the full log (`strategy show` already prints everything, in
// order, with no cutoff). Fixed rather than a flag: this command exists
// specifically to make the "how much is enough" call so every caller
// doesn't have to, and a knob here would just push that judgment back
// onto them.
const recentEventsCap = 10

// RecentStrategyEvents returns the events `strategy next` treats as
// "what's happened recently, worth recapping before resuming work":
// everything from the strategy's most recent step_completed event
// onward, inclusive of that event itself, or the whole log if no
// step_completed has ever been logged.
//
// Why step_completed as the cutoff: it's the one event kind an agent
// chooses to write specifically to mean "one self-contained unit of
// work here is actually done" — step_started, finding, decision,
// resource_usage, and reflection are all things that happen *during* a
// unit of work,
// step_completed is the marker that a unit of work ended. That makes it
// the natural boundary between "already settled, a resuming agent
// doesn't need to re-derive it" and "happened since, still live
// context" — including the boundary event itself means the recap always
// shows what that last completed step even was, not just what came
// after it. If a strategy has never logged a step_completed (e.g. it's
// still mid-first-step), there's no such boundary yet, so the whole log
// counts as "current."
//
// Either way the window is capped at recentEventsCap events, counting
// from the end — a strategy that runs long stretches without ever
// calling step_completed again would otherwise make "recent" balloon to
// the entire history, which defeats the point of a concise recap.
func (s *Store) RecentStrategyEvents(strategyID string) ([]*StrategyEvent, error) {
	all, err := s.ListStrategyEvents(strategyID)
	if err != nil {
		return nil, err
	}
	start := 0
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Kind == "step_completed" {
			start = i
			break
		}
	}
	recent := all[start:]
	if len(recent) > recentEventsCap {
		recent = recent[len(recent)-recentEventsCap:]
	}
	return recent, nil
}

// resourceUsageNote formats a resource_usage event's note as a
// deterministic, parseable "key=value" line instead of freeform prose:
// "harness=<harness> tokens=<n>", with " cost=<c>" appended only when
// hasCost is true. This is the one format log-usage writes and
// parseResourceUsageNote reads back — the actual point of log-usage over
// plain `strategy log`, since numbers logged today only stay comparable
// once every writer agrees on a shape.
func resourceUsageNote(harness string, tokens int64, cost float64, hasCost bool) string {
	note := fmt.Sprintf("harness=%s tokens=%d", harness, tokens)
	if hasCost {
		note += " cost=" + strconv.FormatFloat(cost, 'f', -1, 64)
	}
	return note
}

// parsedResourceUsage is one resource_usage event's note, decoded back out
// by parseResourceUsageNote.
type parsedResourceUsage struct {
	Harness string
	Tokens  int64
	Cost    float64
	HasCost bool
}

// parseResourceUsageNote decodes the format resourceUsageNote writes.
// Errors on anything that doesn't match — a hand-edited note, or one
// written by the older freeform `strategy log -kind=resource_usage`
// before this format existed — rather than guessing at a partial parse;
// callers (strategy usage) are expected to skip-and-report on error, not
// crash, matching tactic list's handling of an unparseable file.
func parseResourceUsageNote(note string) (parsedResourceUsage, error) {
	var p parsedResourceUsage
	seen := map[string]bool{}
	for _, field := range strings.Fields(note) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return p, fmt.Errorf("malformed field %q (want key=value)", field)
		}
		switch key {
		case "harness":
			p.Harness = value
		case "tokens":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return p, fmt.Errorf("invalid tokens value %q (want a non-negative integer)", value)
			}
			p.Tokens = n
		case "cost":
			c, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return p, fmt.Errorf("invalid cost value %q", value)
			}
			p.Cost = c
			p.HasCost = true
		default:
			return p, fmt.Errorf("unknown field %q", key)
		}
		seen[key] = true
	}
	if !seen["harness"] || p.Harness == "" {
		return p, fmt.Errorf("missing harness field")
	}
	if !seen["tokens"] {
		return p, fmt.Errorf("missing tokens field")
	}
	return p, nil
}

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdStrategy(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema strategy <create|list|show|log|log-usage|usage|activate|observe|close|next> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return cmdStrategyCreate(rest)
	case "list":
		return cmdStrategyList(rest)
	case "show":
		return cmdStrategyShow(rest)
	case "log":
		return cmdStrategyLog(rest)
	case "log-usage":
		return cmdStrategyLogUsage(rest)
	case "usage":
		return cmdStrategyUsage(rest)
	case "activate":
		return cmdStrategySetStatus(rest, "activate", StrategyActive)
	case "observe":
		return cmdStrategySetStatus(rest, "observe", StrategyObserving)
	case "close":
		return cmdStrategyClose(rest)
	case "next":
		return cmdStrategyNext(rest)
	default:
		return die(2, "strategy: unknown subcommand %q (create|list|show|log|log-usage|usage|activate|observe|close|next)", sub)
	}
}

func cmdStrategyCreate(args []string) int {
	fs := flag.NewFlagSet("strategy create", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	name := fs.String("name", "", "short name for the strategy (required)")
	thesis := fs.String("thesis", "", "what this strategy is trying to prove or achieve (required)")
	group := fs.String("group", "", "optional label grouping this strategy with others in the same larger effort")
	fs.Parse(args)

	if *name == "" || *thesis == "" {
		return die(1, "strategy create: -name and -thesis are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy create: %v", err)
	}
	defer store.Close()

	st, err := store.CreateStrategy(*name, *thesis)
	if err != nil {
		return die(1, "strategy create: %v", err)
	}
	if *group != "" {
		// A second call rather than a CreateStrategy parameter -- see
		// SetStrategyGroup's doc comment for why: it keeps CreateStrategy's
		// signature (and every existing call site) untouched at the cost of
		// this one extra write, which is fine at strategy-creation volume.
		if err := store.SetStrategyGroup(st.ID, *group); err != nil {
			return die(1, "strategy create: %v", err)
		}
		st.Group = *group
	}
	fmt.Printf("created %s\n  name:   %s\n  thesis: %s\n", st.ID, st.Name, st.Thesis)
	return 0
}

// displayGroup renders a strategy's optional group label the same way
// tactic list renders its own optional core field: the value if set, "-"
// if not -- one shared convention for "this optional field wasn't given."
func displayGroup(group string) string {
	if group == "" {
		return "-"
	}
	return group
}

// escapeForSingleLineDisplay guards against the exact class of bug a
// durability pass already found and fixed once for lock resource names
// (validResourceName): a free-text field that's allowed to contain literal
// newlines -- name, thesis, group, and an event note are none of them
// newline-rejected the way a resource name is -- would otherwise break the
// one-row-per-line assumption `strategy list`/`show`/`next` and `usage`
// all print under, splitting a single strategy or event across multiple
// visual lines and orphaning whatever field printed after it. Only touches
// a string that actually contains one; every already-existing single-line
// value round-trips through this unchanged.
func escapeForSingleLineDisplay(s string) string {
	if !strings.ContainsAny(s, "\n\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\n")
	return s
}

func cmdStrategyList(args []string) int {
	fs := flag.NewFlagSet("strategy list", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	group := fs.String("group", "", "only list strategies with this exact group label (optional)")
	fs.Parse(args)

	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy list: %v", err)
	}
	defer store.Close()

	strategies, err := store.ListStrategies()
	if err != nil {
		return die(1, "strategy list: %v", err)
	}
	// Filtered client-side, not a dedicated SQL query -- strategy counts
	// here are small enough that this doesn't need to be a database-level
	// filter to be correct.
	if *group != "" {
		var filtered []*Strategy
		for _, st := range strategies {
			if st.Group == *group {
				filtered = append(filtered, st)
			}
		}
		strategies = filtered
	}
	if len(strategies) == 0 {
		fmt.Println("no strategies")
		return 0
	}
	for _, st := range strategies {
		fmt.Printf("%-22s  %-10s  %-9s  group=%-10s  created=%s\n", st.ID, escapeForSingleLineDisplay(st.Name), st.Status, escapeForSingleLineDisplay(displayGroup(st.Group)), st.CreatedAt.UTC().Format(time.RFC3339))
	}
	return 0
}

func cmdStrategyShow(args []string) int {
	fs := flag.NewFlagSet("strategy show", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy show: strategy id required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy show: %v", err)
	}
	defer store.Close()

	st, err := store.GetStrategy(rest[0])
	if err != nil {
		return die(1, "strategy show: %v", err)
	}
	if st == nil {
		return die(1, "strategy show: strategy %s not found", rest[0])
	}
	fmt.Printf("%s  %s\n", st.ID, escapeForSingleLineDisplay(st.Name))
	fmt.Printf("  status:  %s\n", st.Status)
	fmt.Printf("  thesis:  %s\n", escapeForSingleLineDisplay(st.Thesis))
	fmt.Printf("  group:   %s\n", escapeForSingleLineDisplay(displayGroup(st.Group)))
	fmt.Printf("  created: %s\n", st.CreatedAt.UTC().Format(time.RFC3339))
	if st.ClosedAt != nil {
		fmt.Printf("  closed:  %s\n", st.ClosedAt.UTC().Format(time.RFC3339))
	}

	events, err := store.ListStrategyEvents(st.ID)
	if err != nil {
		return die(1, "strategy show: %v", err)
	}
	if len(events) == 0 {
		fmt.Println("  (no events)")
		return 0
	}
	fmt.Println("  events:")
	for _, ev := range events {
		fmt.Printf("    %s  %-15s  %-9s  by=%s  %s\n", ev.Ts.UTC().Format(time.RFC3339), ev.ID, ev.Kind, ev.IdentityID, escapeForSingleLineDisplay(ev.Note))
	}
	return 0
}

func cmdStrategyLog(args []string) int {
	fs := flag.NewFlagSet("strategy log", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID logging this event (required)")
	kind := fs.String("kind", "", "step_started|step_completed|finding|decision|reflection|resource_usage (required)")
	note := fs.String("note", "", "what happened (required)")
	token := tokenFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy log: strategy id required")
	}
	if *identity == "" || *kind == "" || *note == "" {
		return die(1, "strategy log: -identity, -kind, and -note are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy log: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "strategy log: %v", err)
	}

	ev, err := store.LogStrategyEvent(rest[0], *identity, *kind, *note)
	if err != nil {
		return die(1, "strategy log: %v", err)
	}
	fmt.Printf("logged %s\n  strategy: %s\n  kind:     %s\n", ev.ID, ev.StrategyID, ev.Kind)
	return 0
}

// cmdStrategyLogUsage is the structured alternative to `strategy log
// -kind=resource_usage -note="..."`: same event kind under the hood, but
// the note is always written in resourceUsageNote's fixed shape instead of
// whatever prose a caller typed, so numbers logged by different callers
// stay comparable.
//
// -tokens has no zero value that also means "unset" (0 tokens is a real,
// loggable amount), so it defaults to -1 and any negative value —
// unset or a real mistake — is rejected the same way.
func cmdStrategyLogUsage(args []string) int {
	fs := flag.NewFlagSet("strategy log-usage", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID logging this usage (required)")
	harness := fs.String("harness", "", "agent harness that did the work, e.g. claude-code (required)")
	tokensFlag := fs.Int64("tokens", -1, "tokens used, non-negative integer (required)")
	cost := fs.Float64("cost", 0, "USD cost (optional, omitted from the note if zero/unset)")
	authToken := tokenFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy log-usage: strategy id required")
	}
	if *identity == "" || *harness == "" {
		return die(1, "strategy log-usage: -identity and -harness are required")
	}
	if *tokensFlag < 0 {
		return die(1, "strategy log-usage: -tokens is required and must be non-negative")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy log-usage: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*authToken)); err != nil {
		return die(1, "strategy log-usage: %v", err)
	}

	note := resourceUsageNote(*harness, *tokensFlag, *cost, *cost != 0)
	ev, err := store.LogStrategyEvent(rest[0], *identity, "resource_usage", note)
	if err != nil {
		return die(1, "strategy log-usage: %v", err)
	}
	fmt.Printf("logged %s\n  strategy: %s\n  %s\n", ev.ID, ev.StrategyID, ev.Note)
	return 0
}

// cmdStrategyUsage lists a strategy's resource_usage events and prints a
// per-harness total (tokens, and cost if any were logged), alongside the
// raw per-event lines. A note that doesn't parse — hand-edited, or logged
// through the older freeform `strategy log` before log-usage existed — is
// shown raw and excluded from totals, with a stated reason, rather than
// crashing or silently dropping it (same "report the problem, don't hide
// or crash on it" style as tactic list's handling of a bad file).
func cmdStrategyUsage(args []string) int {
	fs := flag.NewFlagSet("strategy usage", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy usage: strategy id required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy usage: %v", err)
	}
	defer store.Close()

	events, err := store.ListStrategyEvents(rest[0])
	if err != nil {
		return die(1, "strategy usage: %v", err)
	}

	type totals struct {
		tokens  int64
		cost    float64
		hasCost bool
	}
	byHarness := map[string]*totals{}
	var harnessOrder []string
	var rawLines []string
	exit := 0
	for _, ev := range events {
		if ev.Kind != "resource_usage" {
			continue
		}
		rawLines = append(rawLines, fmt.Sprintf("    %s  %-15s  by=%s  %s", ev.Ts.UTC().Format(time.RFC3339), ev.ID, ev.IdentityID, escapeForSingleLineDisplay(ev.Note)))

		p, err := parseResourceUsageNote(ev.Note)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stratagema: strategy usage: skipping %s from totals: %v\n", ev.ID, err)
			exit = 1
			continue
		}
		t, ok := byHarness[p.Harness]
		if !ok {
			t = &totals{}
			byHarness[p.Harness] = t
			harnessOrder = append(harnessOrder, p.Harness)
		}
		t.tokens += p.Tokens
		if p.HasCost {
			t.cost += p.Cost
			t.hasCost = true
		}
	}

	if len(rawLines) == 0 {
		fmt.Println("no resource_usage events logged")
		return exit
	}

	fmt.Println("totals by harness:")
	for _, h := range harnessOrder {
		t := byHarness[h]
		if t.hasCost {
			fmt.Printf("  %-16s  tokens=%d  cost=%s\n", h, t.tokens, strconv.FormatFloat(t.cost, 'f', 2, 64))
		} else {
			fmt.Printf("  %-16s  tokens=%d\n", h, t.tokens)
		}
	}
	fmt.Println("events:")
	for _, l := range rawLines {
		fmt.Println(l)
	}
	return exit
}

// cmdStrategySetStatus reuses one function for both activate and
// observe, parameterized by the actual subcommand verb (for the flag
// set's own name and user-facing messages) and the target status (for
// the actual write and the result line) — kept separate since they
// diverge for every verb here (activate -> active, observe ->
// observing), the same pattern cmdInterestSetStatus already uses for
// pause/resume.
func cmdStrategySetStatus(args []string, verb, status string) int {
	fs := flag.NewFlagSet("strategy "+verb, flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy %s: strategy id required", verb)
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy %s: %v", verb, err)
	}
	defer store.Close()

	if err := store.SetStrategyStatus(rest[0], status); err != nil {
		return die(1, "strategy %s: %v", verb, err)
	}
	fmt.Printf("%s: %s\n", rest[0], status)
	return 0
}

func cmdStrategyClose(args []string) int {
	fs := flag.NewFlagSet("strategy close", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID closing this strategy (required)")
	outcome := fs.String("outcome", "", "the final outcome note, recorded as a reflection event (required)")
	token := tokenFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy close: strategy id required")
	}
	if *identity == "" || *outcome == "" {
		return die(1, "strategy close: -identity and -outcome are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy close: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "strategy close: %v", err)
	}

	if err := store.CloseStrategy(rest[0], *identity, *outcome); err != nil {
		return die(1, "strategy close: %v", err)
	}
	fmt.Printf("%s: closed\n", rest[0])
	return 0
}

// cmdStrategyNext prints a concise, structured recap for an agent (or
// human) resuming work on a strategy: its current status and thesis, the
// recent slice of its event log (see RecentStrategyEvents for the
// cutoff), and the locks currently held system-wide, split into what's
// actually linked to this strategy (via AcquireLock's -strategy flag) and
// everything else — a real link now, not just an informational dump of
// every held lock.
//
// Deliberately does not decide anything: no "recommended next step," no
// scoring, no filtering by relevance, no call to any LLM or external
// service. That line is the whole point of this command — Stratagema
// stays infrastructure, the calling agent brings the reasoning. This
// prints state; what to do about it is the caller's call, every time.
func cmdStrategyNext(args []string) int {
	fs := flag.NewFlagSet("strategy next", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy next: strategy id required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy next: %v", err)
	}
	defer store.Close()

	st, err := store.GetStrategy(rest[0])
	if err != nil {
		return die(1, "strategy next: %v", err)
	}
	if st == nil {
		return die(1, "strategy next: strategy %s not found", rest[0])
	}

	all, err := store.ListStrategyEvents(st.ID)
	if err != nil {
		return die(1, "strategy next: %v", err)
	}
	recent, err := store.RecentStrategyEvents(st.ID)
	if err != nil {
		return die(1, "strategy next: %v", err)
	}

	fmt.Printf("%s  %s\n", st.ID, escapeForSingleLineDisplay(st.Name))
	fmt.Printf("  status:  %s\n", st.Status)
	fmt.Printf("  thesis:  %s\n", escapeForSingleLineDisplay(st.Thesis))
	fmt.Printf("  created: %s\n", st.CreatedAt.UTC().Format(time.RFC3339))
	if st.ClosedAt != nil {
		fmt.Printf("  closed:  %s\n", st.ClosedAt.UTC().Format(time.RFC3339))
	}

	fmt.Println()
	if len(all) == 0 {
		fmt.Println("  recent events: (none logged yet)")
	} else {
		cutoff := "since last step_completed"
		if len(recent) == len(all) {
			cutoff = "no step_completed logged yet — this is the full log"
		}
		fmt.Printf("  recent events (%d of %d total, %s):\n", len(recent), len(all), cutoff)
		for _, ev := range recent {
			fmt.Printf("    %s  %-15s  %-9s  by=%s  %s\n", ev.Ts.UTC().Format(time.RFC3339), ev.ID, ev.Kind, ev.IdentityID, escapeForSingleLineDisplay(ev.Note))
		}
	}

	fmt.Println()
	locks, err := store.ListLocks()
	if err != nil {
		return die(1, "strategy next: %v", err)
	}
	var linked, other []*Lock
	for _, l := range locks {
		if l.StrategyID == st.ID {
			linked = append(linked, l)
		} else {
			other = append(other, l)
		}
	}
	now := time.Now()
	if len(linked) == 0 {
		fmt.Println("  locks linked to this strategy: none")
	} else {
		fmt.Printf("  locks linked to this strategy (%d):\n", len(linked))
		for _, l := range linked {
			fmt.Printf("    %-24s  holder=%-20s  since=%s%s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339), leaseSuffix(l, now))
		}
	}

	fmt.Println()
	if len(other) == 0 {
		fmt.Println("  other locks held system-wide: none")
	} else {
		fmt.Printf("  other locks held system-wide (%d, not linked to this strategy):\n", len(other))
		for _, l := range other {
			otherStrategy := "unlinked"
			if l.StrategyID != "" {
				otherStrategy = "strategy=" + l.StrategyID
			}
			fmt.Printf("    %-24s  holder=%-20s  since=%s  %s%s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339), otherStrategy, leaseSuffix(l, now))
		}
	}
	return 0
}
