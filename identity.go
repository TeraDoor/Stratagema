package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"time"
)

// Identity is deliberately just a label — no owner, no status, no
// suspend/retire lifecycle. Those are Substrate's accountability/audit
// concerns; Stratagema only needs enough of an identity to name a lock
// holder and a propagation recipient.
type Identity struct {
	ID        string
	Label     string
	CreatedAt time.Time
}

func (s *Store) CreateIdentity(label string) (*Identity, error) {
	id, err := newID("ident")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO identities (id, label, created_at) VALUES (?, ?, ?)`, id, label, now); err != nil {
		return nil, err
	}
	return &Identity{ID: id, Label: label, CreatedAt: time.UnixMilli(now)}, nil
}

func (s *Store) GetIdentity(id string) (*Identity, error) {
	row := s.db.QueryRow(`SELECT id, label, created_at FROM identities WHERE id = ?`, id)
	var it Identity
	var createdAt int64
	err := row.Scan(&it.ID, &it.Label, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it.CreatedAt = time.UnixMilli(createdAt)
	return &it, nil
}

func (s *Store) ListIdentities() ([]*Identity, error) {
	rows, err := s.db.Query(`SELECT id, label, created_at FROM identities ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Identity
	for rows.Next() {
		var it Identity
		var createdAt int64
		if err := rows.Scan(&it.ID, &it.Label, &createdAt); err != nil {
			return nil, err
		}
		it.CreatedAt = time.UnixMilli(createdAt)
		out = append(out, &it)
	}
	return out, rows.Err()
}

// ── CLI ──────────────────────────────────────────────────────────────────

func cmdIdentity(args []string) int {
	if len(args) == 0 {
		fmt.Println("usage: stratagema identity <create|list> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return cmdIdentityCreate(rest)
	case "list":
		return cmdIdentityList(rest)
	default:
		return die(2, "identity: unknown subcommand %q (create|list)", sub)
	}
}

func cmdIdentityCreate(args []string) int {
	fs := flag.NewFlagSet("identity create", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	label := fs.String("label", "", "a name for this identity, e.g. \"claude-code-1\" (required)")
	fs.Parse(args)

	if *label == "" {
		return die(1, "identity create: -label is required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "identity create: %v", err)
	}
	defer store.Close()

	it, err := store.CreateIdentity(*label)
	if err != nil {
		return die(1, "identity create: %v", err)
	}
	fmt.Printf("created %s\n  label: %s\n", it.ID, it.Label)
	return 0
}

func cmdIdentityList(args []string) int {
	fs := flag.NewFlagSet("identity list", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "identity list: %v", err)
	}
	defer store.Close()

	ids, err := store.ListIdentities()
	if err != nil {
		return die(1, "identity list: %v", err)
	}
	if len(ids) == 0 {
		fmt.Println("no identities")
		return 0
	}
	for _, it := range ids {
		fmt.Printf("%-20s  %s\n", it.ID, it.Label)
	}
	return 0
}
