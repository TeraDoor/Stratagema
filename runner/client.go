package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// strategyEvent mirrors the root package's StrategyEvent wire shape
// (../strategy.go) field-for-field: that type carries no json struct
// tags, so its default encoding/json field names (ID, StrategyID, Kind,
// IdentityID, Note, Ts) are exactly what a running `stratagema serve`
// actually sends -- this struct exists to decode that response, not as an
// import (see coordinatorClient's doc comment for why an import isn't
// possible here at all).
type strategyEvent struct {
	ID         string
	StrategyID string
	Kind       string
	IdentityID string
	Note       string
	Ts         time.Time
}

// errorResponse is the one JSON shape every non-2xx response from `serve`
// carries -- see ../serve.go's writeError. Copied, not imported, for the
// same reason as strategyEvent above.
type errorResponse struct {
	Error string `json:"error"`
}

// coordinatorClient is a deliberately tiny, self-contained HTTP client
// speaking only the two Coordinator wire calls this runner actually needs
// (POST /identities/verify, POST /strategies/{id}/events). It is a
// hand-written subset of remote.go's real RemoteStore, not an import of
// it -- Go does not allow importing another package's `package main`, and
// stratagema's root package (per go.mod: a single flat `package main` at
// the repo root) is exactly that. That rules out reuse of RemoteStore, the
// Coordinator interface, and every domain type (Strategy, StrategyEvent,
// ...) regardless of preference: there is no importable non-main package
// in this module today for this runner to depend on without either
// extracting one (real surgery across the whole existing, already-tested
// root package, for two HTTP calls' worth of benefit) or reimplementing
// the tiny slice actually needed here. This file is that second, smaller
// option, chosen to keep the existing, working root package completely
// untouched by this addition.
type coordinatorClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// requireRemoteDB is this runner's one deliberate narrowing of the "-db
// accepts a local file path or a URL" convention the main `stratagema`
// CLI documents (store.go's openStore): this tool only ever supports the
// URL form -- a running `stratagema serve` instance. Supporting a local
// SQLite file directly would mean opening that file and re-deriving
// Store's write path (schema validation, kind checks, existence checks)
// in-process -- exactly the "importing and calling Store/Coordinator
// methods directly in-process pretending to be part of the core" pattern
// this tool's architecture brief rules out. HTTP is the one Coordinator
// surface that exists independently of the root package's internals, so
// it's the only one this genuinely separate binary can honestly speak.
// For local/personal use: run `stratagema serve -db=<file>` yourself and
// point this runner at `http://localhost:<port>` -- "local" here means
// where the coordinator process happens to run, not a direct file open.
func requireRemoteDB(v string) (string, error) {
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return "", fmt.Errorf("-db must be an http:// or https:// URL to a running `stratagema serve` instance (got %q); run `stratagema serve -db=<file>` and point -db at it", v)
	}
	return strings.TrimRight(v, "/"), nil
}

func newCoordinatorClient(dbURL string) (*coordinatorClient, error) {
	base, err := requireRemoteDB(dbURL)
	if err != nil {
		return nil, err
	}
	return &coordinatorClient{baseURL: base, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// do mirrors remote.go's RemoteStore.do: a non-2xx response decodes into
// errorResponse and becomes a plain Go error carrying the server's own
// message, matching this project's existing "clear string, not a typed
// wire error" posture (see remote.go's do doc comment for why).
func (c *coordinatorClient) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("contacting coordinator at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response from %s: %w", path, err)
		}
		return nil
	}

	b, _ := io.ReadAll(resp.Body)
	var er errorResponse
	if jsonErr := json.Unmarshal(b, &er); jsonErr == nil && er.Error != "" {
		return errors.New(er.Error)
	}
	return fmt.Errorf("coordinator request to %s failed (%d): %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
}

// verifyIdentity is its own round trip before any mutating call, mirroring
// the existing local/remote CLI shape (VerifyIdentityToken called as an
// explicit pre-check -- see identity.go's doc comments and remote.go's
// VerifyIdentityToken). token is remembered on the client and replayed as
// Authorization: Bearer on every subsequent request, same as RemoteStore.
func (c *coordinatorClient) verifyIdentity(identityID, token string) error {
	c.token = token
	return c.do(http.MethodPost, "/identities/verify", map[string]string{"identity_id": identityID}, nil)
}

// logEvent is the runner's one write path into a strategy's log -- see
// main.go's step_started/step_completed/finding call sites, which are
// this task's actual load-bearing logic (turning the harness's real exit
// code into an honest event kind, not a self-attested one).
func (c *coordinatorClient) logEvent(strategyID, identityID, kind, note string) (*strategyEvent, error) {
	var ev strategyEvent
	body := map[string]string{"identity_id": identityID, "kind": kind, "note": note}
	if err := c.do(http.MethodPost, "/strategies/"+url.PathEscape(strategyID)+"/events", body, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}
