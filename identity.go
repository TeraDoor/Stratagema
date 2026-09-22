package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"
)

// Identity is deliberately just a label — no owner, no status, no
// suspend/retire lifecycle. Those are Substrate's accountability/audit
// concerns; Stratagema only needs enough of an identity to name a lock
// holder and a propagation recipient.
//
// Protected/token auth is the one addition to that: whether *acting as*
// this identity requires proving you were given its secret. It's still not
// an owner/status/lifecycle system — there's no suspend, no revoke, no
// rotation (see VerifyIdentityToken's doc comment) — just "does using this
// label require a secret, yes or no." Protected is derived from whether
// token_hash is set; the hash itself is never exposed on this struct.
type Identity struct {
	ID        string
	Label     string
	CreatedAt time.Time
	Protected bool
}

// identityTokenPrefix is a plain, recognizable marker on every generated
// token, the same precedent GitHub (ghp_/gho_/...) and Stripe (sk_/pk_)
// follow: it costs nothing, makes a pasted token immediately identifiable
// as a Stratagema identity token (e.g. in a scrubbed log or a secret
// scanner's rules), and is not itself a secret -- the entropy is entirely
// in what follows it.
const identityTokenPrefix = "sgt_"

// generateIdentityToken returns a fresh, high-entropy identity secret: 32
// bytes (256 bits) from crypto/rand -- never math/rand, which is
// predictable and unsuitable for anything security-sensitive -- hex-encoded
// into a CLI-safe string with no characters a shell, URL, or terminal could
// mangle.
func generateIdentityToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return identityTokenPrefix + hex.EncodeToString(b), nil
}

// hashIdentityToken is the one place a token is turned into what actually
// gets stored. sha256, not a slow password KDF (bcrypt/scrypt/argon2): this
// is a 256-bit random secret, not a low-entropy human password, so there's
// no offline-guessing risk a slow hash is defending against -- the same
// reasoning GitHub/AWS/Stripe apply to their own API keys/PATs. A plain
// fast hash is the correct, standard tool for this exact case.
func hashIdentityToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateIdentity(label string) (*Identity, error) {
	return s.createIdentity(label, "")
}

// CreateProtectedIdentity is CreateIdentity's opt-in sibling: it generates
// a real random token (generateIdentityToken), stores only its hash, and
// returns the identity alongside the one and only time the plaintext token
// is ever available. The caller (cmdIdentityCreate) is responsible for
// printing it clearly and exactly once -- this function itself never logs
// or persists it.
func (s *Store) CreateProtectedIdentity(label string) (*Identity, string, error) {
	token, err := generateIdentityToken()
	if err != nil {
		return nil, "", err
	}
	it, err := s.createIdentity(label, hashIdentityToken(token))
	if err != nil {
		return nil, "", err
	}
	return it, token, nil
}

func (s *Store) createIdentity(label, tokenHash string) (*Identity, error) {
	id, err := newID("ident")
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	var hashArg any
	if tokenHash != "" {
		hashArg = tokenHash
	}
	if _, err := s.exec(`INSERT INTO identities (id, label, created_at, token_hash) VALUES (?, ?, ?, ?)`, id, label, now, hashArg); err != nil {
		return nil, err
	}
	return &Identity{ID: id, Label: label, CreatedAt: time.UnixMilli(now), Protected: tokenHash != ""}, nil
}

func (s *Store) ListIdentities() ([]*Identity, error) {
	rows, err := s.db.Query(`SELECT id, label, created_at, token_hash FROM identities ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Identity
	for rows.Next() {
		var it Identity
		var createdAt int64
		var tokenHash sql.NullString
		if err := rows.Scan(&it.ID, &it.Label, &createdAt, &tokenHash); err != nil {
			return nil, err
		}
		it.CreatedAt = time.UnixMilli(createdAt)
		it.Protected = tokenHash.Valid
		out = append(out, &it)
	}
	return out, rows.Err()
}

// VerifyIdentityToken is the single shared verification path every
// identity-scoped mutating CLI command calls before proceeding (see lock.go,
// interest.go, strategy.go) -- one function, not one copy-pasted check per
// call site, so a fix or a bug applies everywhere at once.
//
// An identity with no token_hash succeeds unconditionally, regardless of
// what token was passed: unprotected means unprotected, not "protected
// unless you happen to pass nothing." That includes an identityID with no
// row in the identities table at all -- identity.go's own doc comment is
// explicit that Identity is "deliberately just a label," and every existing
// call site (AcquireLock included) has never required `identity create` to
// run first; a lock/interest/strategy caller happily uses an ad hoc string.
// Requiring the row to exist here would silently turn that into a required
// registration step for every unprotected identity, which is exactly the
// observable-difference regression this feature must not introduce. Only a
// label that was actually protected via CreateProtectedIdentity -- which by
// construction always has a row -- can ever fail this check.
//
// A protected identity requires the supplied token's hash to match,
// compared with crypto/subtle.ConstantTimeCompare rather than == -- a naive
// equality check on a hash is a real, avoidable timing side-channel (an
// attacker can use response-time differences to recover the hash byte by
// byte), so this does it correctly.
//
// Deliberately does not build: rotation, revocation, or un-protecting an
// identity after creation, or any expiry/session mechanism for the token
// itself. Those are a real, larger feature this one doesn't attempt --
// today, "protected" is permanent and the one token issued at creation is
// the only one that will ever work.
func (s *Store) VerifyIdentityToken(identityID, token string) error {
	var tokenHash sql.NullString
	err := s.db.QueryRow(`SELECT token_hash FROM identities WHERE id = ?`, identityID).Scan(&tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no row for this label at all: unprotected, same as always
	}
	if err != nil {
		return err
	}
	if !tokenHash.Valid {
		return nil // unprotected identity: no token is ever required, whatever was passed
	}
	if token == "" {
		return fmt.Errorf("identity %s is protected: a valid token is required (set STRATAGEMA_TOKEN or pass -token)", identityID)
	}
	supplied := hashIdentityToken(token)
	// Both sides are fixed-length hex-encoded sha256 digests (64 bytes),
	// so this never leaks length -- only ConstantTimeCompare's designed-for
	// equality check does.
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(tokenHash.String)) != 1 {
		return fmt.Errorf("identity %s is protected: invalid token", identityID)
	}
	return nil
}

// tokenFlag registers the -token override every identity-scoped mutating
// command accepts, all with identical wording so the safety tradeoff is
// stated the same way everywhere a caller might read it.
func tokenFlag(fs *flag.FlagSet) *string {
	return fs.String("token", "", "identity token (overrides STRATAGEMA_TOKEN) -- prefer the env var: a flag value is visible in shell history and process listings, the same tradeoff GITHUB_TOKEN/cloud CLIs make")
}

// resolveToken applies the documented precedence: the STRATAGEMA_TOKEN
// env var by default, an explicit non-empty -token flag as the override.
func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("STRATAGEMA_TOKEN")
}

// ── CLI ──────────────────────────────────────────────────────────────────

// cmdIdentity deliberately does not special-case "-h"/"--help"/"help" as a
// pseudo-subcommand -- confirmed intentional-by-precedent, not an oversight:
// every other subcommand-group dispatcher in this codebase (lock, profile,
// strategy, interest) has the exact same shape, so "-h" here falls through
// to the same unknown-subcommand error as any other bad subcommand. Actual
// per-command help is still available two ways: `stratagema identity` alone
// (len(args)==0 above) prints the same usage line, and `stratagema identity
// create -h` / `identity list -h` get real flag-package-generated help,
// since flag.ExitOnError intercepts -h before this switch ever sees it.
// Only the top level (main.go) treats -h/--help/help as first-class.
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
	protect := fs.Bool("protect", false, "generate a random secret token this identity must present (via -token or STRATAGEMA_TOKEN) for identity-scoped mutating actions; printed exactly once, never recoverable afterward")
	fs.Parse(args)

	// identity create takes no positional arguments -- everything is a flag.
	// A leftover arg here almost always means a flag after it went unparsed
	// (flag.Parse stops at the first non-flag token, see cli.go/strategy_e2e_test.go's
	// own note on this), which would otherwise silently drop e.g. -protect
	// instead of erroring. Catch it rather than pretend nothing was typed.
	if fs.NArg() != 0 {
		return die(2, "identity create: unexpected argument(s) %v -- this command takes no positional arguments; if you meant a flag, check it comes before any other argument (flags after a positional value are not parsed)", fs.Args())
	}

	if *label == "" {
		return die(1, "identity create: -label is required")
	}
	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "identity create: %v", err)
	}
	defer store.Close()

	if !*protect {
		it, err := store.CreateIdentity(*label)
		if err != nil {
			return die(1, "identity create: %v", err)
		}
		fmt.Printf("created %s\n  label: %s\n", it.ID, escapeForSingleLineDisplay(it.Label))
		return 0
	}

	it, token, err := store.CreateProtectedIdentity(*label)
	if err != nil {
		return die(1, "identity create: %v", err)
	}
	fmt.Printf("created %s\n  label:     %s\n  protected: yes\n\n", it.ID, escapeForSingleLineDisplay(it.Label))
	fmt.Printf("token (save this now -- it will not be shown again, and cannot be recovered):\n\n  %s\n\n", token)
	fmt.Printf("set STRATAGEMA_TOKEN=%s (preferred), or pass -token=<value> to any identity-scoped\ncommand, to act as %s.\n", token, it.ID)
	return 0
}

// displayProtected renders whether an identity requires a token, following
// the same "value or -" convention profile list/strategy list already use
// for their own optional fields (core, group).
func displayProtected(p bool) string {
	if p {
		return "protected"
	}
	return "-"
}

func cmdIdentityList(args []string) int {
	fs := flag.NewFlagSet("identity list", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	fs.Parse(args)

	// Same reasoning as cmdIdentityCreate: no positional arguments are ever
	// expected, so a leftover one is a sign a flag after it was silently
	// dropped by flag.Parse, not a thing to ignore.
	if fs.NArg() != 0 {
		return die(2, "identity list: unexpected argument(s) %v -- this command takes no positional arguments; if you meant a flag, check it comes before any other argument (flags after a positional value are not parsed)", fs.Args())
	}

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
		fmt.Printf("%-20s  %-9s  %s\n", it.ID, displayProtected(it.Protected), escapeForSingleLineDisplay(it.Label))
	}
	return 0
}
