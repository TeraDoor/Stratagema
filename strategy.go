package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
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
// resource_usage is schema-only for now: a place to log real spend
// (tokens, dollars, wall time) against a strategy once a harness reports
// it, logged by hand until real per-invocation reporting exists. It
// carries no enforcement — a strategy that overspends isn't blocked,
// just recorded, matching the soft-reporting-first lean this project
// settled on before building any budget enforcement.
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
// happened; this row is just what a human or a Faculty reads first to
// know what a strategy_events log even belongs to.
type Strategy struct {
	ID        string
	Name      string
	Thesis    string
	Status    string
	CreatedAt time.Time
	ClosedAt  *time.Time
}

// StrategyEvent is one append-only entry in a strategy's log — a finding,
// a decision, a reflection, or a step boundary, written by whichever
// identity (often a Faculty) was doing the work at the time. Never
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
	row := s.db.QueryRow(`SELECT id, name, thesis, status, created_at, closed_at FROM strategies WHERE id = ?`, id)
	st, err := scanStrategy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

func (s *Store) ListStrategies() ([]*Strategy, error) {
	rows, err := s.db.Query(`SELECT id, name, thesis, status, created_at, closed_at FROM strategies ORDER BY created_at`)
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
	if err := sc.Scan(&st.ID, &st.Name, &st.Thesis, &st.Status, &createdAt, &closedAt); err != nil {
		return nil, err
	}
	st.CreatedAt = time.UnixMilli(createdAt)
	if closedAt.Valid {
		t := time.UnixMilli(closedAt.Int64)
		st.ClosedAt = &t
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

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdStrategy(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema strategy <create|list|show|log|activate|observe|close|next> [flags]")
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
	case "activate":
		return cmdStrategySetStatus(rest, StrategyActive)
	case "observe":
		return cmdStrategySetStatus(rest, StrategyObserving)
	case "close":
		return cmdStrategyClose(rest)
	case "next":
		return cmdStrategyNext(rest)
	default:
		return die(2, "strategy: unknown subcommand %q (create|list|show|log|activate|observe|close|next)", sub)
	}
}

func cmdStrategyCreate(args []string) int {
	fs := flag.NewFlagSet("strategy create", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	name := fs.String("name", "", "short name for the strategy (required)")
	thesis := fs.String("thesis", "", "what this strategy is trying to prove or achieve (required)")
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
	fmt.Printf("created %s\n  name:   %s\n  thesis: %s\n", st.ID, st.Name, st.Thesis)
	return 0
}

func cmdStrategyList(args []string) int {
	fs := flag.NewFlagSet("strategy list", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
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
	if len(strategies) == 0 {
		fmt.Println("no strategies")
		return 0
	}
	for _, st := range strategies {
		fmt.Printf("%-22s  %-10s  %-9s  created=%s\n", st.ID, st.Name, st.Status, st.CreatedAt.UTC().Format(time.RFC3339))
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
	fmt.Printf("%s  %s\n", st.ID, st.Name)
	fmt.Printf("  status:  %s\n", st.Status)
	fmt.Printf("  thesis:  %s\n", st.Thesis)
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
		fmt.Printf("    %s  %-15s  %-9s  by=%s  %s\n", ev.Ts.UTC().Format(time.RFC3339), ev.ID, ev.Kind, ev.IdentityID, ev.Note)
	}
	return 0
}

func cmdStrategyLog(args []string) int {
	fs := flag.NewFlagSet("strategy log", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID logging this event (required)")
	kind := fs.String("kind", "", "step_started|step_completed|finding|decision|reflection|resource_usage (required)")
	note := fs.String("note", "", "what happened (required)")
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

	ev, err := store.LogStrategyEvent(rest[0], *identity, *kind, *note)
	if err != nil {
		return die(1, "strategy log: %v", err)
	}
	fmt.Printf("logged %s\n  strategy: %s\n  kind:     %s\n", ev.ID, ev.StrategyID, ev.Kind)
	return 0
}

// cmdStrategySetStatus reuses one function for both activate and
// observe, parameterized by target status — the same pattern
// cmdInterestSetStatus already uses for pause/resume.
func cmdStrategySetStatus(args []string, status string) int {
	fs := flag.NewFlagSet("strategy "+status, flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "strategy %s: strategy id required", status)
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "strategy %s: %v", status, err)
	}
	defer store.Close()

	if err := store.SetStrategyStatus(rest[0], status); err != nil {
		return die(1, "strategy %s: %v", status, err)
	}
	fmt.Printf("%s: %s\n", rest[0], status)
	return 0
}

func cmdStrategyClose(args []string) int {
	fs := flag.NewFlagSet("strategy close", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID closing this strategy (required)")
	outcome := fs.String("outcome", "", "the final outcome note, recorded as a reflection event (required)")
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

	if err := store.CloseStrategy(rest[0], *identity, *outcome); err != nil {
		return die(1, "strategy close: %v", err)
	}
	fmt.Printf("%s: closed\n", rest[0])
	return 0
}

// cmdStrategyNext prints a concise, structured recap for an agent (or
// human) resuming work on a strategy: its current status and thesis, the
// recent slice of its event log (see RecentStrategyEvents for the
// cutoff), and every lock currently held system-wide as a "here's what's
// contested right now" courtesy — strategies and locks aren't formally
// linked in the schema, so this is every held lock, not just ones this
// strategy is presumed to care about.
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

	fmt.Printf("%s  %s\n", st.ID, st.Name)
	fmt.Printf("  status:  %s\n", st.Status)
	fmt.Printf("  thesis:  %s\n", st.Thesis)
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
			fmt.Printf("    %s  %-15s  %-9s  by=%s  %s\n", ev.Ts.UTC().Format(time.RFC3339), ev.ID, ev.Kind, ev.IdentityID, ev.Note)
		}
	}

	fmt.Println()
	locks, err := store.ListLocks()
	if err != nil {
		return die(1, "strategy next: %v", err)
	}
	if len(locks) == 0 {
		fmt.Println("  locks held system-wide: none")
	} else {
		fmt.Println("  locks held system-wide (informational only — not linked to this strategy):")
		for _, l := range locks {
			fmt.Printf("    %-24s  holder=%-20s  since=%s\n", l.Resource, l.HolderID, l.AcquiredAt.UTC().Format(time.RFC3339))
		}
	}
	return 0
}
