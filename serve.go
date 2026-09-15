package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
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

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbFlag := dbPathFlag(fs)
	port := fs.Int("port", 7979, "port to listen on")
	fs.Parse(args)

	store, err := openStore(*dbFlag)
	if err != nil {
		return die(1, "serve: %v", err)
	}
	defer store.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/locks", locksHandler(store))
	mux.HandleFunc("/stream/interest", sseStreamInterestHandler(store))

	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("stratagema serve  http://localhost%s\n\n", addr)
	fmt.Println("  GET /health                          health check")
	fmt.Println("  GET /locks                            JSON snapshot of active locks")
	fmt.Println("  GET /stream/interest?identity=<id>    SSE stream of propagation deliveries for one identity")
	fmt.Println()
	fmt.Println("ctrl-c to stop")

	if err := http.ListenAndServe(addr, mux); err != nil {
		return die(1, "serve: %v", err)
	}
	return 0
}
