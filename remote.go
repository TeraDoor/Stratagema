package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteStore is Coordinator's other implementation: every method below
// makes a real HTTP/JSON call to a running `stratagema serve` instance
// instead of touching SQLite directly. It never runs server-side — the
// server (cmdServe, serve.go) always talks to the real local *Store; a
// RemoteStore only ever exists on the calling end, constructed by
// openStore when a "-db" value starts with http:// or https://.
//
// Standard library only (net/http, encoding/json) — no new dependency, same
// zero-framework posture as the rest of this project.
type RemoteStore struct {
	baseURL string
	client  *http.Client

	mu    sync.Mutex
	token string // last token passed to VerifyIdentityToken, replayed on Authorization: Bearer for subsequent requests
}

func newRemoteStore(baseURL string) *RemoteStore {
	return &RemoteStore{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Close is a safe no-op beyond releasing idle connections — RemoteStore
// holds no open resource that must be released, but every cmd* function
// calls Close via defer unconditionally, so this must never panic or error.
func (r *RemoteStore) Close() error {
	r.client.CloseIdleConnections()
	return nil
}

func (r *RemoteStore) currentToken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.token
}

func (r *RemoteStore) setToken(token string) {
	r.mu.Lock()
	r.token = token
	r.mu.Unlock()
}

// errorResponse is the one JSON shape every non-2xx response from the
// server carries — see serve.go's writeError.
type errorResponse struct {
	Error string `json:"error"`
}

// request builds an HTTP request against path, JSON-encoding body (if any)
// and attaching whatever token VerifyIdentityToken last supplied via the
// standard Authorization: Bearer <token> header convention — see
// VerifyIdentityToken's doc comment for why a RemoteStore has to remember
// it at all, given AcquireLock/ReleaseLock/etc. take no token parameter of
// their own.
func (r *RemoteStore) request(method, path string, query url.Values, body any) (*http.Request, error) {
	u := r.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := r.currentToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req, nil
}

// do sends req and decodes a 2xx JSON body into out (nil out = discard the
// body, used for the error-only mutations that respond 204). A non-2xx
// response is turned into a plain Go error carrying the server's message —
// deliberately not a typed/wrapped error: no cmd* function does
// errors.Is(err, ErrLockHeld) or similar today (confirmed by grep before
// building this), so a clear string is enough to match existing CLI
// behavior; over-engineering a typed-error-over-the-wire scheme here would
// be solving a problem nothing actually has.
func (r *RemoteStore) do(req *http.Request, out any) error {
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("stratagema: contacting %s: %w", r.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response from %s: %w", req.URL.Path, err)
		}
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	var er errorResponse
	if jsonErr := json.Unmarshal(body, &er); jsonErr == nil && er.Error != "" {
		return errors.New(er.Error)
	}
	return fmt.Errorf("remote request to %s failed (%d): %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
}

func (r *RemoteStore) get(path string, query url.Values, out any) error {
	req, err := r.request(http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	return r.do(req, out)
}

func (r *RemoteStore) post(path string, body any, out any) error {
	req, err := r.request(http.MethodPost, path, nil, body)
	if err != nil {
		return err
	}
	return r.do(req, out)
}

// ── wire request/response DTOs ──────────────────────────────────────────
//
// Shared between RemoteStore (encodes/decodes these) and serve.go's
// handlers (decodes/encodes the same shapes) — every domain type that
// already exists (Lock, Identity, Strategy, StrategyEvent, Interest,
// PropagationDelivery) is sent as-is via encoding/json (time.Time marshals
// natively; a nil *Lock/*Strategy marshals to JSON null, matching
// GetLock/GetStrategy's existing not-found-is-nil-not-error semantics
// exactly). These small structs exist only for the method parameters that
// aren't already one struct.

type acquireLockRequest struct {
	Resource     string `json:"resource"`
	IdentityID   string `json:"identity_id"`
	Note         string `json:"note"`
	StrategyID   string `json:"strategy_id"`
	LeaseSeconds int    `json:"lease_seconds"`
}

type releaseLockRequest struct {
	Resource   string `json:"resource"`
	IdentityID string `json:"identity_id"`
	Note       string `json:"note"`
	Force      bool   `json:"force"`
}

type renewLockRequest struct {
	Resource   string `json:"resource"`
	IdentityID string `json:"identity_id"`
}

type labelRequest struct {
	Label string `json:"label"`
}

type createProtectedIdentityResponse struct {
	Identity *Identity `json:"identity"`
	Token    string    `json:"token"`
}

type verifyIdentityRequest struct {
	IdentityID string `json:"identity_id"`
}

type createInterestRequest struct {
	IdentityID string `json:"identity_id"`
	Resource   string `json:"resource"`
	Label      string `json:"label"`
}

type statusRequest struct {
	Status string `json:"status"`
}

type recentPropagationsResponse struct {
	Deliveries []PropagationDelivery `json:"deliveries"`
	LastRowID  int64                  `json:"last_row_id"`
}

type createStrategyRequest struct {
	Name   string `json:"name"`
	Thesis string `json:"thesis"`
}

type logStrategyEventRequest struct {
	IdentityID string `json:"identity_id"`
	Kind       string `json:"kind"`
	Note       string `json:"note"`
}

type closeStrategyRequest struct {
	IdentityID string `json:"identity_id"`
	Outcome    string `json:"outcome"`
}

type groupRequest struct {
	Group string `json:"group"`
}

// ── Coordinator implementation ──────────────────────────────────────────

func (r *RemoteStore) GetLock(resource string) (*Lock, error) {
	var l *Lock
	err := r.get("/locks/get", url.Values{"resource": {resource}}, &l)
	return l, err
}

func (r *RemoteStore) ListLocks() ([]*Lock, error) {
	var locks []*Lock
	err := r.get("/locks", nil, &locks)
	return locks, err
}

func (r *RemoteStore) AcquireLock(resource, identityID, note, strategyID string, leaseSeconds int) (*Lock, error) {
	var l *Lock
	err := r.post("/locks/acquire", acquireLockRequest{resource, identityID, note, strategyID, leaseSeconds}, &l)
	return l, err
}

func (r *RemoteStore) ReleaseLock(resource, identityID, note string, force bool) error {
	return r.post("/locks/release", releaseLockRequest{resource, identityID, note, force}, nil)
}

func (r *RemoteStore) RenewLock(resource, identityID string) (*Lock, error) {
	var l *Lock
	err := r.post("/locks/renew", renewLockRequest{resource, identityID}, &l)
	return l, err
}

func (r *RemoteStore) ListIdentities() ([]*Identity, error) {
	var ids []*Identity
	err := r.get("/identities", nil, &ids)
	return ids, err
}

func (r *RemoteStore) CreateIdentity(label string) (*Identity, error) {
	var it *Identity
	err := r.post("/identities", labelRequest{label}, &it)
	return it, err
}

func (r *RemoteStore) CreateProtectedIdentity(label string) (*Identity, string, error) {
	var resp createProtectedIdentityResponse
	err := r.post("/identities/protected", labelRequest{label}, &resp)
	return resp.Identity, resp.Token, err
}

// VerifyIdentityToken is its own real HTTP round trip, separate from
// whatever mutating call follows it (AcquireLock, LogStrategyEvent, ...) —
// see lock.go's cmdLockAcquire for the existing local pattern this mirrors:
// a cmd* function verifies as its own step before acting, and that
// structure isn't changing here. It also remembers token (via setToken) so
// the request that follows can carry it on Authorization: Bearer, since
// none of the mutating Coordinator methods themselves take a token
// parameter — this costs one extra round trip per mutating command in
// remote mode versus local mode's single in-process call; an honest,
// accepted tradeoff, not something to optimize away by restructuring
// commands (see cmd*'s "verify then act" shape, unchanged by design).
func (r *RemoteStore) VerifyIdentityToken(identityID, token string) error {
	r.setToken(token)
	return r.post("/identities/verify", verifyIdentityRequest{identityID}, nil)
}

func (r *RemoteStore) ListInterests() ([]*Interest, error) {
	var ins []*Interest
	err := r.get("/interests", nil, &ins)
	return ins, err
}

func (r *RemoteStore) CreateInterest(identityID, resource, label string) (*Interest, error) {
	var in *Interest
	err := r.post("/interests", createInterestRequest{identityID, resource, label}, &in)
	return in, err
}

func (r *RemoteStore) SetInterestStatus(id, status string) error {
	return r.post("/interests/"+url.PathEscape(id)+"/status", statusRequest{status}, nil)
}

func (r *RemoteStore) ListPropagationsForIdentity(identityID string, pendingOnly bool) ([]PropagationDelivery, error) {
	var out []PropagationDelivery
	err := r.get("/interests/propagations", url.Values{
		"identity": {identityID},
		"pending":  {strconv.FormatBool(pendingOnly)},
	}, &out)
	return out, err
}

func (r *RemoteStore) RecentPropagationsForIdentity(identityID string, lastRowID int64, limit int) ([]PropagationDelivery, int64, error) {
	var resp recentPropagationsResponse
	err := r.get("/interests/propagations/recent", url.Values{
		"identity": {identityID},
		"after":    {strconv.FormatInt(lastRowID, 10)},
		"limit":    {strconv.Itoa(limit)},
	}, &resp)
	return resp.Deliveries, resp.LastRowID, err
}

func (r *RemoteStore) AcknowledgePropagation(id string) error {
	return r.post("/interests/propagations/"+url.PathEscape(id)+"/ack", nil, nil)
}

func (r *RemoteStore) ListStrategies() ([]*Strategy, error) {
	var out []*Strategy
	err := r.get("/strategies", nil, &out)
	return out, err
}

func (r *RemoteStore) CreateStrategy(name, thesis string) (*Strategy, error) {
	var st *Strategy
	err := r.post("/strategies", createStrategyRequest{name, thesis}, &st)
	return st, err
}

func (r *RemoteStore) GetStrategy(id string) (*Strategy, error) {
	var st *Strategy
	err := r.get("/strategies/"+url.PathEscape(id), nil, &st)
	return st, err
}

func (r *RemoteStore) ListStrategyEvents(strategyID string) ([]*StrategyEvent, error) {
	var out []*StrategyEvent
	err := r.get("/strategies/"+url.PathEscape(strategyID)+"/events", nil, &out)
	return out, err
}

func (r *RemoteStore) RecentStrategyEvents(strategyID string) ([]*StrategyEvent, error) {
	var out []*StrategyEvent
	err := r.get("/strategies/"+url.PathEscape(strategyID)+"/events/recent", nil, &out)
	return out, err
}

func (r *RemoteStore) LogStrategyEvent(strategyID, identityID, kind, note string) (*StrategyEvent, error) {
	var ev *StrategyEvent
	err := r.post("/strategies/"+url.PathEscape(strategyID)+"/events", logStrategyEventRequest{identityID, kind, note}, &ev)
	return ev, err
}

func (r *RemoteStore) CloseStrategy(id, identityID, outcome string) error {
	return r.post("/strategies/"+url.PathEscape(id)+"/close", closeStrategyRequest{identityID, outcome}, nil)
}

func (r *RemoteStore) SetStrategyGroup(id, group string) error {
	return r.post("/strategies/"+url.PathEscape(id)+"/group", groupRequest{group}, nil)
}

func (r *RemoteStore) SetStrategyStatus(id, status string) error {
	return r.post("/strategies/"+url.PathEscape(id)+"/status", statusRequest{status}, nil)
}
