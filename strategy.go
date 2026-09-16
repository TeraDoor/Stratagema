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
var validStrategyEventKinds = map[string]bool{
	"step_started":   true,
	"step_completed": true,
	"finding":        true,
	"decision":       true,
	"reflection":     true,
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
	Kind       string // step_started | step_completed | finding | decision | reflection
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
		return nil, fmt.Errorf("strategy log: unknown kind %q (want step_started|step_completed|finding|decision|reflection)", kind)
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

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdStrategy(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema strategy <create|list|show|log|activate|observe|close> [flags]")
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
	default:
		return die(2, "strategy: unknown subcommand %q (create|list|show|log|activate|observe|close)", sub)
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
	kind := fs.String("kind", "", "step_started|step_completed|finding|decision|reflection (required)")
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
