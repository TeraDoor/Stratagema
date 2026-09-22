package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"time"
)

const (
	InterestActive = "active"
	InterestPaused = "paused"
)

// Interest is one identity's subscription to one resource, by exact name —
// no component/family/procedure taxonomy to match against, because there's
// no gate/procedure system underneath it here. If wildcard or prefix
// matching turns out to be needed, that's a real future decision, not
// assumed up front.
type Interest struct {
	ID         string
	IdentityID string
	Resource   string
	Status     string
	Label      string
	CreatedAt  time.Time
}

func (s *Store) CreateInterest(identityID, resource, label string) (*Interest, error) {
	if err := validResourceName(resource); err != nil {
		return nil, err
	}

	// Idempotent: a durability pass found a client that times out and
	// retries an identical `interest create` call (not knowing whether
	// the first attempt actually landed — exactly the "connectivity
	// loss" case) silently doubled, tripled, etc. its own future
	// notifications. Same identity + same resource + still active is
	// the same logical interest; return it rather than creating another.
	existing, err := s.activeInterestFor(identityID, resource)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	id, err := newID("interest")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := s.exec(
		`INSERT INTO interests (id, identity_id, resource, status, label, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, identityID, resource, InterestActive, label, now,
	); err != nil {
		return nil, err
	}
	return &Interest{ID: id, IdentityID: identityID, Resource: resource, Status: InterestActive, Label: label, CreatedAt: time.UnixMilli(now)}, nil
}

// activeInterestFor returns identityID's active interest on resource, if
// one already exists, or (nil, nil).
func (s *Store) activeInterestFor(identityID, resource string) (*Interest, error) {
	row := s.db.QueryRow(
		`SELECT id, identity_id, resource, status, label, created_at FROM interests
		  WHERE identity_id = ? AND resource = ? AND status = ? LIMIT 1`,
		identityID, resource, InterestActive,
	)
	in, err := scanInterest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return in, err
}

func (s *Store) SetInterestStatus(id, status string) error {
	res, err := s.exec(`UPDATE interests SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("interest %s not found", id)
	}
	return nil
}

func (s *Store) ListInterests() ([]*Interest, error) {
	rows, err := s.db.Query(`SELECT id, identity_id, resource, status, label, created_at FROM interests ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Interest
	for rows.Next() {
		in, err := scanInterest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) activeInterestsForResource(resource string) ([]*Interest, error) {
	rows, err := s.db.Query(
		`SELECT id, identity_id, resource, status, label, created_at FROM interests WHERE resource = ? AND status = ?`,
		resource, InterestActive,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Interest
	for rows.Next() {
		in, err := scanInterest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanInterest(sc rowScanner) (*Interest, error) {
	var in Interest
	var label sql.NullString
	var createdAt int64
	if err := sc.Scan(&in.ID, &in.IdentityID, &in.Resource, &in.Status, &label, &createdAt); err != nil {
		return nil, err
	}
	in.Label = label.String
	in.CreatedAt = time.UnixMilli(createdAt)
	return &in, nil
}

// ── propagation ─────────────────────────────────────────────────────────

// PropagationDelivery is one delivery, enriched with the lock event it
// came from so a subscriber doesn't need a second lookup to know what
// happened — same shape substrate's own PropagationDelivery used, minus
// the gate/procedure fields this project doesn't have.
type PropagationDelivery struct {
	RowID          int64
	ID             string
	InterestID     string
	LockEventID    string
	IdentityID     string
	CreatedAt      time.Time
	AcknowledgedAt *time.Time
	Resource       string
	Kind           string // acquired | denied | released, from the lock event
	Note           string
	ActorID        string // who performed/attempted the lock action
	HolderID       string
	Forced         bool
}

// matchAndPropagate creates one propagation per active interest on
// resource, for the most recent lock event on it. Called after a denied
// acquire or a release — never after a plain acquire, so an uncontested
// claim stays quiet.
func (s *Store) matchAndPropagate(resource string) error {
	interests, err := s.activeInterestsForResource(resource)
	if err != nil {
		return err
	}
	if len(interests) == 0 {
		return nil
	}
	// rowid alone, not ts — a durability pass proved sorting by wall-clock
	// ts first lets one process with a fast/wrong clock silently hijack
	// every future propagation on a resource: insert a row timestamped
	// far in the future once, and this query keeps picking it over every
	// real, subsequent event indefinitely. rowid is SQLite's own
	// monotonic insert order and is immune to clock skew by construction.
	var lockEventID string
	if err := s.db.QueryRow(
		`SELECT id FROM lock_events WHERE resource = ? ORDER BY rowid DESC LIMIT 1`, resource,
	).Scan(&lockEventID); err != nil {
		return err
	}
	for _, in := range interests {
		pid, err := newID("prop")
		if err != nil {
			return err
		}
		if _, err := s.exec(
			`INSERT INTO propagations (id, interest_id, lock_event_id, identity_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			pid, in.ID, lockEventID, in.IdentityID, time.Now().UnixMilli(),
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AcknowledgePropagation(id string) error {
	res, err := s.exec(`UPDATE propagations SET acknowledged_at = ? WHERE id = ? AND acknowledged_at IS NULL`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM propagations WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("propagation %s not found", id)
	} else if err != nil {
		return err
	}
	return nil // already acknowledged — idempotent, not an error
}

const deliveryColumns = `
	p.rowid, p.id, p.interest_id, p.lock_event_id, p.identity_id, p.created_at, p.acknowledged_at,
	le.resource, le.kind, le.note, le.identity_id, le.holder_identity_id, le.forced
`

func scanDelivery(sc rowScanner) (PropagationDelivery, error) {
	var d PropagationDelivery
	var createdAt int64
	var ackedAt sql.NullInt64
	var note sql.NullString
	var forced int
	if err := sc.Scan(
		&d.RowID, &d.ID, &d.InterestID, &d.LockEventID, &d.IdentityID, &createdAt, &ackedAt,
		&d.Resource, &d.Kind, &note, &d.ActorID, &d.HolderID, &forced,
	); err != nil {
		return d, err
	}
	d.CreatedAt = time.UnixMilli(createdAt)
	if ackedAt.Valid {
		t := time.UnixMilli(ackedAt.Int64)
		d.AcknowledgedAt = &t
	}
	d.Note = note.String
	d.Forced = forced != 0
	return d, nil
}

func (s *Store) ListPropagationsForIdentity(identityID string, pendingOnly bool) ([]PropagationDelivery, error) {
	q := `SELECT ` + deliveryColumns + ` FROM propagations p JOIN lock_events le ON le.id = p.lock_event_id WHERE p.identity_id = ?`
	if pendingOnly {
		q += ` AND p.acknowledged_at IS NULL`
	}
	q += ` ORDER BY p.created_at DESC`
	rows, err := s.db.Query(q, identityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PropagationDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecentPropagationsForIdentity is the SSE polling primitive: everything
// delivered to identityID with rowid > lastRowID, oldest first, up to
// limit rows. A negative limit is clamped to 0 rather than passed through
// to SQLite: SQLite's own LIMIT semantics treat a negative value as "no
// upper bound," which would silently turn this bounded polling primitive
// into an unbounded dump for any caller (or malformed HTTP query) that
// passes one -- confirmed directly, not assumed, since that's exactly the
// kind of surprise this codebase's "verify, don't assume" rule exists for.
func (s *Store) RecentPropagationsForIdentity(identityID string, lastRowID int64, limit int) ([]PropagationDelivery, int64, error) {
	if limit < 0 {
		limit = 0
	}
	q := `SELECT ` + deliveryColumns + ` FROM propagations p JOIN lock_events le ON le.id = p.lock_event_id
	      WHERE p.identity_id = ? AND p.rowid > ? ORDER BY p.rowid ASC LIMIT ?`
	rows, err := s.db.Query(q, identityID, lastRowID, limit)
	if err != nil {
		return nil, lastRowID, err
	}
	defer rows.Close()
	var out []PropagationDelivery
	maxRowID := lastRowID
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return out, maxRowID, err
		}
		out = append(out, d)
		if d.RowID > maxRowID {
			maxRowID = d.RowID
		}
	}
	return out, maxRowID, rows.Err()
}

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdInterest(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema interest <create|list|pause|resume|inbox|ack> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return cmdInterestCreate(rest)
	case "list":
		return cmdInterestList(rest)
	case "pause":
		return cmdInterestSetStatus(rest, "pause", InterestPaused)
	case "resume":
		return cmdInterestSetStatus(rest, "resume", InterestActive)
	case "inbox":
		return cmdInterestInbox(rest)
	case "ack":
		return cmdInterestAck(rest)
	default:
		return die(2, "interest: unknown subcommand %q (create|list|pause|resume|inbox|ack)", sub)
	}
}

func cmdInterestCreate(args []string) int {
	fs := flag.NewFlagSet("interest create", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID this interest belongs to (required)")
	resource := fs.String("resource", "", "resource name to watch, exact match (required)")
	label := fs.String("label", "", "human-readable note")
	token := tokenFlag(fs)
	fs.Parse(args)

	if *identity == "" || *resource == "" {
		return die(1, "interest create: -identity and -resource are required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "interest create: %v", err)
	}
	defer store.Close()

	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "interest create: %v", err)
	}

	in, err := store.CreateInterest(*identity, *resource, *label)
	if err != nil {
		return die(1, "interest create: %v", err)
	}
	fmt.Printf("created %s\n  identity: %s\n  resource: %s\n", in.ID, in.IdentityID, in.Resource)
	return 0
}

func cmdInterestList(args []string) int {
	fs := flag.NewFlagSet("interest list", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "interest list: %v", err)
	}
	defer store.Close()

	ins, err := store.ListInterests()
	if err != nil {
		return die(1, "interest list: %v", err)
	}
	if len(ins) == 0 {
		fmt.Println("no interests")
		return 0
	}
	for _, in := range ins {
		fmt.Printf("%-22s  %-8s  identity=%-16s  resource=%s\n", in.ID, in.Status, in.IdentityID, in.Resource)
	}
	return 0
}

// cmdInterestSetStatus takes both the actual subcommand verb (for the
// flag set's own name and user-facing messages) and the target status
// (for the actual write and the result line) — kept separate since they
// diverge for resume -> active.
func cmdInterestSetStatus(args []string, verb, status string) int {
	fs := flag.NewFlagSet("interest "+verb, flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "interest %s: interest id required", verb)
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "interest %s: %v", verb, err)
	}
	defer store.Close()

	if err := store.SetInterestStatus(rest[0], status); err != nil {
		return die(1, "interest %s: %v", verb, err)
	}
	fmt.Printf("%s: %s\n", rest[0], status)
	return 0
}

func cmdInterestInbox(args []string) int {
	fs := flag.NewFlagSet("interest inbox", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	identity := fs.String("identity", "", "identity ID to show the inbox for (required)")
	all := fs.Bool("all", false, "include already-acknowledged deliveries (default: pending only)")
	token := tokenFlag(fs)
	fs.Parse(args)

	if *identity == "" {
		return die(1, "interest inbox: -identity is required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "interest inbox: %v", err)
	}
	defer store.Close()

	// Reading X's inbox is a read path, not the mutating "act as X" concern
	// the README's known-limitations gap literally named (acquiring/
	// releasing a lock under a claimed identity) -- but once an identity is
	// protected, its inbox is exactly the adjacent information-disclosure
	// case: propagation deliveries can carry another identity's lock notes,
	// resource names, and activity pattern. Extended to require the same
	// token deliberately, for consistency with that concern, not left
	// ambiguous. See README's "Known limitations" for this stated plainly.
	if err := store.VerifyIdentityToken(*identity, resolveToken(*token)); err != nil {
		return die(1, "interest inbox: %v", err)
	}

	deliveries, err := store.ListPropagationsForIdentity(*identity, !*all)
	if err != nil {
		return die(1, "interest inbox: %v", err)
	}
	if len(deliveries) == 0 {
		fmt.Println("inbox empty")
		return 0
	}
	for _, d := range deliveries {
		status := "PENDING"
		if d.AcknowledgedAt != nil {
			status = "acked  "
		}
		forced := ""
		if d.Forced {
			forced = " (forced)"
		}
		fmt.Printf("%s  %-18s  %-9s  resource=%-20s by=%s%s", status, d.ID, d.Kind, d.Resource, d.ActorID, forced)
		if d.Note != "" {
			fmt.Printf("  note=%q", d.Note)
		}
		fmt.Println()
	}
	return 0
}

func cmdInterestAck(args []string) int {
	fs := flag.NewFlagSet("interest ack", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return die(1, "interest ack: propagation id required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "interest ack: %v", err)
	}
	defer store.Close()

	if err := store.AcknowledgePropagation(rest[0]); err != nil {
		return die(1, "interest ack: %v", err)
	}
	fmt.Printf("%s: acknowledged\n", rest[0])
	return 0
}
