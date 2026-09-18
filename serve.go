package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// serve is the one optional, short-lived process in this project — start
// it when you want live push delivery, stop it when you don't. It does
// not run agents, dispatch work, or do anything on a schedule; it only
// answers HTTP requests for as long as it's running. See README's "what
// this deliberately is not."

type ssePropagationEvent struct {
	Seq           int64  `json:"seq"`
	PropagationID string `json:"propagation_id"`
	InterestID    string `json:"interest_id"`
	Resource      string `json:"resource"`
	Kind          string `json:"kind"`
	Note          string `json:"note,omitempty"`
	ActorID       string `json:"actor_id"`
	HolderID      string `json:"holder_id,omitempty"`
	Forced        bool   `json:"forced"`
	CreatedAtUTC  string `json:"created_at_utc"`
	Acknowledged  bool   `json:"acknowledged"`
}

func sseStreamInterestHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity := r.URL.Query().Get("identity")
		if identity == "" {
			http.Error(w, "missing required ?identity=<identity_id> query param", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		fmt.Fprintf(w, ": stratagema propagation stream identity=%s\n\n", identity)
		flusher.Flush()

		var lastRowID int64
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()
			case <-ticker.C:
				deliveries, newRowID, err := store.RecentPropagationsForIdentity(identity, lastRowID, 50)
				if err != nil {
					continue
				}
				for _, d := range deliveries {
					evt := ssePropagationEvent{
						Seq: d.RowID, PropagationID: d.ID, InterestID: d.InterestID,
						Resource: d.Resource, Kind: d.Kind, Note: d.Note,
						ActorID: d.ActorID, HolderID: d.HolderID, Forced: d.Forced,
						CreatedAtUTC: d.CreatedAt.UTC().Format(time.RFC3339),
						Acknowledged: d.AcknowledgedAt != nil,
					}
					data, err := json.Marshal(evt)
					if err != nil {
						continue
					}
					fmt.Fprintf(w, "event: propagation\ndata: %s\n\n", data)
					flusher.Flush()
				}
				lastRowID = newRowID
			}
		}
	}
}

func locksHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locks, err := store.ListLocks()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(locks)
	}
}

// ── remote-coordinator write/read surface ──────────────────────────────
//
// One handler per Coordinator method not already covered above (GetLock,
// ListLocks, /stream/interest) — each just decodes the request, calls the
// real local *Store's method directly, and encodes the result. No
// validation is reimplemented here: whatever Store already enforces
// (resource-name rules, strategy-must-exist, valid status/kind values,
// ...) is what runs, exactly as it does for the local CLI path — this
// layer only translates HTTP <-> Go calls.
//
// RemoteStore is the only client these are written against, but nothing
// here is RemoteStore-specific: any HTTP client (curl included) can drive
// this same surface, matching this project's "no harness lock-in" stance.

// writeJSON encodes v as the response body with status, used for every
// handler below that returns data (as opposed to the error-only mutations,
// which use w.WriteHeader(http.StatusNoContent) directly).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError is the one shape every non-2xx response takes: {"error":
// "..."} — RemoteStore.do (remote.go) reads this back into a plain Go
// error, matching the message a local caller would have seen.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

// statusForStoreError picks a real, meaningful status for a Store method's
// error, not a blanket 500: ErrLockHeld/ErrLeaseExpired/ErrNoLeaseToRenew
// are genuine conflicts with the resource's current state (409); every
// other "not found" error this codebase raises (interest/strategy/
// propagation by id) is textually consistent ("... not found") by
// convention across identity.go/interest.go/lock.go/strategy.go, so that
// substring is the one honest, low-effort way to route those to 404
// without inventing a typed-error scheme this project doesn't otherwise
// have (see remote.go's do doc comment for the same reasoning). Anything
// else is a genuine unexpected failure: 500.
func statusForStoreError(err error) int {
	switch {
	case errors.Is(err, ErrLockHeld), errors.Is(err, ErrLeaseExpired), errors.Is(err, ErrNoLeaseToRenew):
		return http.StatusConflict
	case strings.Contains(err.Error(), "not found"):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// bearerToken reads the identity token off the standard Authorization:
// Bearer <token> header — the one convention every request in this
// surface uses for a token, matching RemoteStore's request (remote.go).
// Only /identities/verify actually reads it; every other endpoint accepts
// (and ignores) the header the same way today's /locks and /stream/
// interest already ignore headers they don't need.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, prefix) {
		return strings.TrimPrefix(h, prefix)
	}
	return ""
}

// decodeBody parses r's JSON body into v, closing the body when done. A
// GET request with query params instead of a body (AcknowledgePropagation)
// never calls this.
func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func locksGetHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resource := r.URL.Query().Get("resource")
		if resource == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("missing required ?resource= query param"))
			return
		}
		l, err := store.GetLock(resource)
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, l)
	}
}

func locksAcquireHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req acquireLockRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Resource == "" || req.IdentityID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("resource and identity_id are required"))
			return
		}
		l, err := store.AcquireLock(req.Resource, req.IdentityID, req.Note, req.StrategyID, req.LeaseSeconds)
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, l)
	}
}

func locksReleaseHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req releaseLockRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Resource == "" || req.IdentityID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("resource and identity_id are required"))
			return
		}
		if err := store.ReleaseLock(req.Resource, req.IdentityID, req.Note, req.Force); err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func locksRenewHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req renewLockRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Resource == "" || req.IdentityID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("resource and identity_id are required"))
			return
		}
		l, err := store.RenewLock(req.Resource, req.IdentityID)
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, l)
	}
}

func identitiesListHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, err := store.ListIdentities()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, ids)
	}
}

func identitiesCreateHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req labelRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Label == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("label is required"))
			return
		}
		it, err := store.CreateIdentity(req.Label)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

func identitiesCreateProtectedHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req labelRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Label == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("label is required"))
			return
		}
		it, token, err := store.CreateProtectedIdentity(req.Label)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, createProtectedIdentityResponse{Identity: it, Token: token})
	}
}

// identitiesVerifyHandler is the one endpoint an unauthenticated or
// wrongly-authenticated request is actually expected to hit: it's the
// dedicated wire target for RemoteStore.VerifyIdentityToken (remote.go),
// itself the same standalone pre-check step cmdLockAcquire and friends
// already run locally before acting (see lock.go). Any failure here — a
// missing token, a wrong one, a protected identity without one — is a
// real 401, not a generic error, matching point 5's requirement that this
// specific failure mode come back as a genuine auth failure over the wire.
func identitiesVerifyHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req verifyIdentityRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.IdentityID == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("identity_id is required"))
			return
		}
		if err := store.VerifyIdentityToken(req.IdentityID, bearerToken(r)); err != nil {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func interestsListHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ins, err := store.ListInterests()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, ins)
	}
}

func interestsCreateHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createInterestRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.IdentityID == "" || req.Resource == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("identity_id and resource are required"))
			return
		}
		in, err := store.CreateInterest(req.IdentityID, req.Resource, req.Label)
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, in)
	}
}

func interestsSetStatusHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req statusRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Status == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("status is required"))
			return
		}
		if err := store.SetInterestStatus(id, req.Status); err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func interestsPropagationsHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity := r.URL.Query().Get("identity")
		if identity == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("missing required ?identity= query param"))
			return
		}
		pending, _ := strconv.ParseBool(r.URL.Query().Get("pending"))
		out, err := store.ListPropagationsForIdentity(identity, pending)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func interestsPropagationsRecentHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity := r.URL.Query().Get("identity")
		if identity == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("missing required ?identity= query param"))
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		deliveries, lastRowID, err := store.RecentPropagationsForIdentity(identity, after, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, recentPropagationsResponse{Deliveries: deliveries, LastRowID: lastRowID})
	}
}

func interestsPropagationAckHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := store.AcknowledgePropagation(id); err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func strategiesListHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := store.ListStrategies()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func strategiesCreateHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createStrategyRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Name == "" || req.Thesis == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("name and thesis are required"))
			return
		}
		st, err := store.CreateStrategy(req.Name, req.Thesis)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func strategiesGetHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := store.GetStrategy(r.PathValue("id"))
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func strategiesEventsHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := store.ListStrategyEvents(r.PathValue("id"))
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func strategiesEventsRecentHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := store.RecentStrategyEvents(r.PathValue("id"))
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func strategiesLogEventHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req logStrategyEventRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.IdentityID == "" || req.Kind == "" || req.Note == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("identity_id, kind, and note are required"))
			return
		}
		ev, err := store.LogStrategyEvent(r.PathValue("id"), req.IdentityID, req.Kind, req.Note)
		if err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		writeJSON(w, http.StatusOK, ev)
	}
}

func strategiesCloseHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req closeStrategyRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.IdentityID == "" || req.Outcome == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("identity_id and outcome are required"))
			return
		}
		if err := store.CloseStrategy(r.PathValue("id"), req.IdentityID, req.Outcome); err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func strategiesGroupHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req groupRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if err := store.SetStrategyGroup(r.PathValue("id"), req.Group); err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func strategiesStatusHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req statusRequest
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("malformed request body: %w", err))
			return
		}
		if req.Status == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("status is required"))
			return
		}
		if err := store.SetStrategyStatus(r.PathValue("id"), req.Status); err != nil {
			writeError(w, statusForStoreError(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// newMux builds the real routing table — pulled out of cmdServe so a test
// can exercise the actual handlers over real HTTP (httptest.NewServer)
// instead of only via manual curl during development, which is all this
// had until now.
func newMux(store *Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/locks", locksHandler(store))
	mux.HandleFunc("/stream/interest", sseStreamInterestHandler(store))

	mux.HandleFunc("GET /locks/get", locksGetHandler(store))
	mux.HandleFunc("POST /locks/acquire", locksAcquireHandler(store))
	mux.HandleFunc("POST /locks/release", locksReleaseHandler(store))
	mux.HandleFunc("POST /locks/renew", locksRenewHandler(store))

	mux.HandleFunc("GET /identities", identitiesListHandler(store))
	mux.HandleFunc("POST /identities", identitiesCreateHandler(store))
	mux.HandleFunc("POST /identities/protected", identitiesCreateProtectedHandler(store))
	mux.HandleFunc("POST /identities/verify", identitiesVerifyHandler(store))

	mux.HandleFunc("GET /interests", interestsListHandler(store))
	mux.HandleFunc("POST /interests", interestsCreateHandler(store))
	mux.HandleFunc("POST /interests/{id}/status", interestsSetStatusHandler(store))
	mux.HandleFunc("GET /interests/propagations", interestsPropagationsHandler(store))
	mux.HandleFunc("GET /interests/propagations/recent", interestsPropagationsRecentHandler(store))
	mux.HandleFunc("POST /interests/propagations/{id}/ack", interestsPropagationAckHandler(store))

	mux.HandleFunc("GET /strategies", strategiesListHandler(store))
	mux.HandleFunc("POST /strategies", strategiesCreateHandler(store))
	mux.HandleFunc("GET /strategies/{id}", strategiesGetHandler(store))
	mux.HandleFunc("GET /strategies/{id}/events", strategiesEventsHandler(store))
	mux.HandleFunc("GET /strategies/{id}/events/recent", strategiesEventsRecentHandler(store))
	mux.HandleFunc("POST /strategies/{id}/events", strategiesLogEventHandler(store))
	mux.HandleFunc("POST /strategies/{id}/close", strategiesCloseHandler(store))
	mux.HandleFunc("POST /strategies/{id}/group", strategiesGroupHandler(store))
	mux.HandleFunc("POST /strategies/{id}/status", strategiesStatusHandler(store))

	return mux
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	port := fs.Int("port", 7979, "port to listen on")
	fs.Parse(args)

	// Always the real local store, never remote — a server dispatching to
	// another server would just be a confusing proxy, not a real backend,
	// so this deliberately calls openLocalStore, not openStore: -db here
	// always names a local SQLite path, even if it happened to look like a
	// URL.
	store, err := openLocalStore(*dbFlag)
	if err != nil {
		return die(1, "serve: %v", err)
	}
	defer store.Close()

	mux := newMux(store)
	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("stratagema serve  http://localhost%s\n\n", addr)
	fmt.Println("  GET  /health                                    health check")
	fmt.Println("  GET  /locks                                     JSON snapshot of active locks")
	fmt.Println("  GET  /locks/get?resource=<name>                 one lock (null if free)")
	fmt.Println("  POST /locks/acquire                             {resource, identity_id, note, strategy_id, lease_seconds}")
	fmt.Println("  POST /locks/release                             {resource, identity_id, note, force}")
	fmt.Println("  POST /locks/renew                               {resource, identity_id}")
	fmt.Println("  GET  /identities                                list identities")
	fmt.Println("  POST /identities                                {label} -> unprotected identity")
	fmt.Println("  POST /identities/protected                      {label} -> {identity, token} (token shown once)")
	fmt.Println("  POST /identities/verify                         {identity_id} + Authorization: Bearer <token>")
	fmt.Println("  GET  /interests                                 list interests")
	fmt.Println("  POST /interests                                 {identity_id, resource, label}")
	fmt.Println("  POST /interests/{id}/status                     {status: active|paused}")
	fmt.Println("  GET  /interests/propagations?identity=&pending= one identity's deliveries")
	fmt.Println("  GET  /interests/propagations/recent?identity=&after=&limit=  SSE-poll primitive")
	fmt.Println("  POST /interests/propagations/{id}/ack           acknowledge one delivery")
	fmt.Println("  GET  /strategies                                list strategies")
	fmt.Println("  POST /strategies                                {name, thesis}")
	fmt.Println("  GET  /strategies/{id}                           one strategy (null if not found)")
	fmt.Println("  GET  /strategies/{id}/events                    full event log")
	fmt.Println("  GET  /strategies/{id}/events/recent             recap since last step_completed")
	fmt.Println("  POST /strategies/{id}/events                    {identity_id, kind, note}")
	fmt.Println("  POST /strategies/{id}/close                     {identity_id, outcome}")
	fmt.Println("  POST /strategies/{id}/group                     {group}")
	fmt.Println("  POST /strategies/{id}/status                    {status: planning|active|observing|closed}")
	fmt.Println("  GET  /stream/interest?identity=<id>             SSE stream of propagation deliveries for one identity")
	fmt.Println()
	fmt.Println("every command above works against -db=" + fmt.Sprintf("http://localhost%s", addr) + " from another process/machine — see README's \"Remote mode\"")
	fmt.Println()
	fmt.Println("ctrl-c to stop")

	if err := http.ListenAndServe(addr, mux); err != nil {
		return die(1, "serve: %v", err)
	}
	return 0
}
