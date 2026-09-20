package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Until now, serve.go's handlers had zero automated coverage — every
// check of /health, /locks, and /stream/interest was a manual curl during
// development. These exercise the actual handlers (via newMux, the same
// routing table cmdServe uses) over real HTTP.

func TestServeHealth(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestServeLocksEndpoint(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	// Empty case first — a real, previously-unverified assumption that
	// "no locks" serializes as `[]`/`null`, not an error.
	resp, err := http.Get(srv.URL + "/locks")
	if err != nil {
		t.Fatalf("GET /locks (empty): %v", err)
	}
	var empty []Lock
	if err := json.NewDecoder(resp.Body).Decode(&empty); err != nil {
		t.Fatalf("decoding empty /locks response: %v", err)
	}
	resp.Body.Close()
	if len(empty) != 0 {
		t.Fatalf("want 0 locks, got %d", len(empty))
	}

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.AcquireLock("res", alpha.ID, "working on it", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	resp, err = http.Get(srv.URL + "/locks")
	if err != nil {
		t.Fatalf("GET /locks: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want Content-Type: application/json, got %q", ct)
	}
	var locks []Lock
	if err := json.NewDecoder(resp.Body).Decode(&locks); err != nil {
		t.Fatalf("decoding /locks response: %v", err)
	}
	if len(locks) != 1 || locks[0].Resource != "res" || locks[0].HolderID != alpha.ID {
		t.Fatalf("unexpected /locks content: %+v", locks)
	}
}

func TestServeStreamInterestRequiresIdentity(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/stream/interest")
	if err != nil {
		t.Fatalf("GET /stream/interest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 with no ?identity=, got %d", resp.StatusCode)
	}
}

// TestServeStreamInterestDeliversLive is the automated version of the
// manual curl-based dry run this project's design was originally proven
// with: a real HTTP client connects to /stream/interest before anything
// happens, and must see a real propagation arrive live, over the wire,
// without polling — not just that the underlying store method returns
// the right rows.
func TestServeStreamInterestDeliversLive(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.Close()

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	beta, err := s.CreateIdentity("agent-beta")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	if _, err := s.CreateInterest(beta.ID, "res", ""); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}
	if _, err := s.AcquireLock("res", alpha.ID, "editing", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+beta.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting to /stream/interest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// Now trigger the real event, after the client is already connected —
	// the whole point of "live," as opposed to a snapshot fetched after
	// the fact.
	if err := s.ReleaseLock("res", alpha.ID, "safe to read now", false); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	lineCh := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lineCh:
			if strings.HasPrefix(line, "data: ") {
				var evt ssePropagationEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
					t.Fatalf("unmarshaling SSE event %q: %v", line, err)
				}
				if evt.Kind != "released" || evt.Resource != "res" || evt.Note != "safe to read now" {
					t.Fatalf("unexpected event content: %+v", evt)
				}
				return // success
			}
		case <-deadline:
			t.Fatal("timed out waiting for the live propagation event over SSE")
		}
	}
}
