package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is an adversarial pass over sseStreamInterestHandler
// (serve.go) specifically -- /stream/interest's real-time SSE push path,
// which until now only had TestServeStreamInterestDeliversLive (one client,
// one event, happy path) and TestServeStreamInterestRequiresIdentity
// (missing ?identity=). The mutating routes and both auth layers already
// got their own adversarial pass elsewhere (server_robustness_test.go /
// server_hardening_test.go) -- not duplicated here. Every test below talks
// to a real httptest.Server over real HTTP and reads the real SSE wire
// format off the response body, the same pattern
// TestServeStreamInterestDeliversLive already established; none of this
// mocks the handler or the store.

// ── goroutine-leak helpers ──────────────────────────────────────────────

// sampleGoroutines takes a few NumGoroutine readings after letting the
// runtime settle (GC, then a short pause so a just-returned goroutine's
// stack is actually reclaimed before it's counted) and returns the
// minimum. A genuine per-connection leak shows up as this floor rising
// across measurements; an occasional high single reading (GC, scheduler
// noise) does not, and averaging or taking the last reading would let that
// noise mask -- or fake -- a real signal.
func sampleGoroutines(t *testing.T) int {
	t.Helper()
	min := -1
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if min == -1 || n < min {
			min = n
		}
	}
	return min
}

// waitForGoroutineFloor polls sampleGoroutines until it drops to at most
// want or timeout elapses, returning the last reading either way --
// disconnecting a client and the server actually noticing (ctx.Done())
// aren't the same instant, so a single immediate sample would be racy
// against the very thing being tested.
func waitForGoroutineFloor(t *testing.T, want int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := sampleGoroutines(t)
	for last > want && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		last = sampleGoroutines(t)
	}
	return last
}

// noKeepAliveClient avoids Go's HTTP keep-alive connection pool adding its
// own goroutine/connection noise on top of what the SSE handler itself
// does -- each request here gets a real, fresh TCP connection, closed for
// real when the response body is closed, which is exactly the per-
// connection lifecycle these tests are trying to observe cleanly.
func noKeepAliveClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// ── 1. client disconnect ─────────────────────────────────────────────────

// TestSSEClientDisconnectHandlerGoroutineExits connects one real client,
// confirms the handler's per-connection loop has actually started (reads
// the opening comment line), disconnects mid-stream before any real event,
// and confirms the goroutine count returns to its pre-connection baseline
// -- i.e. the handler's own `case <-ctx.Done(): return` really does fire
// and really does end that goroutine, not just that the client-side
// request returns.
func TestSSEClientDisconnectHandlerGoroutineExits(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	client := noKeepAliveClient()
	baseline := sampleGoroutines(t)

	req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+alpha.ID, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("connecting to /stream/interest: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "stratagema propagation stream") {
		t.Fatalf("did not see the opening comment line before disconnecting (line=%q err=%v) -- can't confirm the per-connection loop had started", line, err)
	}

	resp.Body.Close() // disconnect mid-stream, no event ever arrived

	got := waitForGoroutineFloor(t, baseline, 2*time.Second)
	if got > baseline {
		t.Fatalf("goroutine count did not return to baseline after client disconnect: baseline=%d, after=%d -- the per-connection SSE loop may have leaked", baseline, got)
	}
}

// TestSSEManyClientsConnectDisconnectNoGoroutineLeak runs several rounds of
// many clients (well over the task's "10+") connecting and disconnecting
// at varying points, and checks the goroutine floor across rounds -- a
// real leak shows up as that floor climbing round over round, not as a
// one-off blip in a single round, which is why this compares the first and
// last round rather than asserting every round individually.
func TestSSEManyClientsConnectDisconnectNoGoroutineLeak(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	client := noKeepAliveClient()
	const rounds = 6
	const perRound = 12
	counts := make([]int, 0, rounds)

	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for i := 0; i < perRound; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+alpha.ID, nil)
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				buf := make([]byte, 256)
				resp.Body.Read(buf) // consume the opening comment
				// Disconnect at varying points within the round: some
				// immediately, some after a short window -- both shapes of
				// "mid-stream" the task calls for.
				time.Sleep(time.Duration(i%4) * 15 * time.Millisecond)
			}(i)
		}
		wg.Wait()
		time.Sleep(150 * time.Millisecond) // let ctx.Done()-triggered returns actually unwind
		counts = append(counts, sampleGoroutines(t))
	}

	t.Logf("goroutine floor per round: %v", counts)
	first, last := counts[0], counts[rounds-1]
	const slack = 6 // real scheduler/runtime noise headroom, not a leak allowance
	if last > first+slack {
		t.Fatalf("goroutine count grew across connect/disconnect rounds (first=%d last=%d, all=%v) -- looks like a real per-connection leak, not noise", first, last, counts)
	}
}

// ── 2. identity query param edge cases ───────────────────────────────────
//
// The plain missing-?identity=->400 case is already covered by
// TestServeStreamInterestRequiresIdentity; not duplicated here.

// TestSSENonexistentIdentityStreamsEmptyNotError confirms an ?identity=
// that matches no real identity is not an error: RecentPropagationsForIdentity
// (interest.go) is a plain `WHERE p.identity_id = ?` filter with no
// existence check anywhere in the path, so a nonexistent id is, by the
// code's own logic, just an id that will never match any row -- 200,
// connection stays open, no propagation events, ever. Confirmed live over
// the wire across several real poll ticks, not just read out of the code.
func TestSSENonexistentIdentityStreamsEmptyNotError(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity=does-not-exist", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting to /stream/interest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for a nonexistent identity (a filter with no match, not an error), got %d", resp.StatusCode)
	}

	lineCh := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	sawOpeningComment := false
	deadline := time.After(1200 * time.Millisecond) // several 200ms poll ticks
	for {
		select {
		case line := <-lineCh:
			if strings.HasPrefix(line, "event: propagation") || strings.HasPrefix(line, "data: ") {
				t.Fatalf("nonexistent identity produced a real propagation event: %q -- want an empty stream forever", line)
			}
			if strings.Contains(line, "stratagema propagation stream") {
				sawOpeningComment = true
			}
		case <-deadline:
			if !sawOpeningComment {
				t.Fatalf("never saw the opening comment line at all")
			}
			return // success: stayed open, produced nothing, across multiple poll ticks
		}
	}
}

// TestSSEMultipleIdentityQueryParamsFirstWins confirms Go's standard
// r.URL.Query().Get behavior -- first value wins -- rather than assuming
// it. Only the first identity in the query string has a real interest
// registered on "res"; a real release is triggered after connecting, and
// the test passes only if that first identity's delivery actually arrives,
// which it can only do if the handler used the first value.
func TestSSEMultipleIdentityQueryParamsFirstWins(t *testing.T) {
	s, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer s.Close()

	owner, err := s.CreateIdentity("owner")
	if err != nil {
		t.Fatalf("CreateIdentity(owner): %v", err)
	}
	first, err := s.CreateIdentity("agent-first")
	if err != nil {
		t.Fatalf("CreateIdentity(first): %v", err)
	}
	second, err := s.CreateIdentity("agent-second")
	if err != nil {
		t.Fatalf("CreateIdentity(second): %v", err)
	}
	// Only "first" has a real interest -- if the handler used "second"
	// instead, this identity filter would never match anything and the
	// release below would never show up on the stream.
	if _, err := s.CreateInterest(first.ID, "res", ""); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}
	if _, err := s.AcquireLock("res", owner.ID, "editing", "", 0); err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+first.ID+"&identity="+second.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting to /stream/interest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	if err := s.ReleaseLock("res", owner.ID, "safe to read now", false); err != nil {
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
				if evt.Kind != "released" || evt.Resource != "res" {
					t.Fatalf("unexpected event content: %+v", evt)
				}
				return // success: first query value's identity really was used
			}
		case <-deadline:
			t.Fatal("timed out -- either the release event never arrived (unexpected on its own) or the handler used the wrong identity value from ?identity=first&identity=second")
		}
	}
}

// ── 3. delivery ordering and completeness under real concurrent activity ──

// TestSSEDeliveryOrderAndCompletenessUnderConcurrentLoad runs a real
// producer goroutine hammering acquire/deny/release on one resource
// against the same *Store the streaming client is reading from, and
// confirms every propagation meant for the watching identity arrives, in
// the correct order, with none dropped and none duplicated. 80 events
// (2 per cycle x 40 cycles) deliberately exceeds sseStreamInterestHandler's
// hardcoded 50-row poll limit, so completeness here also proves the
// lastRowID cursor correctly continues across multiple poll ticks instead
// of losing anything past the first page.
func TestSSEDeliveryOrderAndCompletenessUnderConcurrentLoad(t *testing.T) {
	s, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer s.Close()

	owner, err := s.CreateIdentity("owner")
	if err != nil {
		t.Fatalf("CreateIdentity(owner): %v", err)
	}
	denier, err := s.CreateIdentity("denier")
	if err != nil {
		t.Fatalf("CreateIdentity(denier): %v", err)
	}
	watcher, err := s.CreateIdentity("watcher")
	if err != nil {
		t.Fatalf("CreateIdentity(watcher): %v", err)
	}
	if _, err := s.CreateInterest(watcher.ID, "res", ""); err != nil {
		t.Fatalf("CreateInterest: %v", err)
	}

	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+watcher.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting to /stream/interest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	lineCh := make(chan string, 4096)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	const cycles = 40
	wantKinds := make([]string, 0, cycles*2)
	for i := 0; i < cycles; i++ {
		wantKinds = append(wantKinds, "denied", "released")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < cycles; i++ {
			if _, err := s.AcquireLock("res", owner.ID, "cycle", "", 0); err != nil {
				t.Errorf("AcquireLock(owner) cycle %d: %v", i, err)
				return
			}
			if _, err := s.AcquireLock("res", denier.ID, "contend", "", 0); err == nil {
				t.Errorf("AcquireLock(denier) cycle %d: want ErrLockHeld, got nil", i)
				return
			} else if !strings.Contains(err.Error(), "resource is locked") {
				t.Errorf("AcquireLock(denier) cycle %d: want ErrLockHeld, got %v", i, err)
				return
			}
			if err := s.ReleaseLock("res", owner.ID, "done", false); err != nil {
				t.Errorf("ReleaseLock cycle %d: %v", i, err)
				return
			}
		}
	}()

	var got []ssePropagationEvent
	deadline := time.After(15 * time.Second)
collect:
	for len(got) < len(wantKinds) {
		select {
		case line := <-lineCh:
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var evt ssePropagationEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
				t.Fatalf("unmarshaling SSE event %q: %v", line, err)
			}
			got = append(got, evt)
		case <-deadline:
			break collect
		}
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("producer goroutine never finished")
	}

	if len(got) != len(wantKinds) {
		t.Fatalf("delivery completeness: want %d propagation events, got %d", len(wantKinds), len(got))
	}

	seen := make(map[int64]bool, len(got))
	var lastSeq int64
	for i, evt := range got {
		if seen[evt.Seq] {
			t.Fatalf("event %d: Seq %d delivered more than once", i, evt.Seq)
		}
		seen[evt.Seq] = true
		if evt.Seq <= lastSeq {
			t.Fatalf("event %d: Seq %d out of order (previous was %d)", i, evt.Seq, lastSeq)
		}
		lastSeq = evt.Seq
		if evt.Resource != "res" {
			t.Fatalf("event %d: want resource \"res\", got %q", i, evt.Resource)
		}
		if evt.Kind != wantKinds[i] {
			t.Fatalf("event %d: want kind %q, got %q -- delivery order doesn't match real chronological order", i, wantKinds[i], evt.Kind)
		}
	}
}

// ── 4. heartbeat ──────────────────────────────────────────────────────────

// TestSSEHeartbeatFiresDuringIdle connects one client to an identity with
// no interest and no lock activity at all -- the only thing that can put
// any byte on the wire is the handler's own 15s heartbeat ticker -- and
// waits, for real, for a ": heartbeat" comment line. This is deliberately
// the one slow test in this file: faking the SSE protocol's actual timer
// wouldn't prove the wiring is real.
func TestSSEHeartbeatFiresDuringIdle(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+alpha.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting to /stream/interest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	lineCh := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
	}()

	deadline := time.After(20 * time.Second)
	for {
		select {
		case line := <-lineCh:
			if strings.Contains(line, "heartbeat") {
				return // success
			}
		case <-deadline:
			t.Fatal("no heartbeat comment line seen within 20s of an idle connection -- want the 15s heartbeat ticker to have fired at least once")
		}
	}
}

// ── 5. resource bounds under many concurrent streams ──────────────────────

// TestSSEManyConcurrentStreamsServerStaysResponsive opens 60 real,
// simultaneous SSE connections (each running the handler's own 200ms DB-
// polling ticker) and then times a completely unrelated GET /locks from a
// separate client, confirming the streaming handlers' per-connection
// polling loops don't starve other real HTTP work. This project's stated
// audience is a solo developer running more than one agent -- the bar here
// is staying responsive at that kind of real, modest concurrency (60
// streams comfortably covers "5-10 real agents" with headroom), not
// surviving unbounded scale.
func TestSSEManyConcurrentStreamsServerStaysResponsive(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	alpha, err := s.CreateIdentity("agent-alpha")
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}

	const n = 60
	client := noKeepAliveClient()
	var mu sync.Mutex
	conns := make([]*http.Response, 0, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", srv.URL+"/stream/interest?identity="+alpha.ID, nil)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			resp.Body.Read(buf) // consume the opening comment: confirms the per-connection loop is really running
			mu.Lock()
			conns = append(conns, resp)
			mu.Unlock()
		}()
	}
	wg.Wait()
	defer func() {
		for _, r := range conns {
			r.Body.Close()
		}
	}()

	if len(conns) < n*3/4 {
		t.Fatalf("only %d/%d SSE connections established -- can't meaningfully test load", len(conns), n)
	}

	start := time.Now()
	resp, err := http.Get(srv.URL + "/locks")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GET /locks while %d SSE streams active: %v", len(conns), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /locks while %d SSE streams active: want 200, got %d", len(conns), resp.StatusCode)
	}

	t.Logf("GET /locks answered in %s while %d concurrent SSE streams were active (each polling every 200ms)", elapsed, len(conns))
	if elapsed > 2*time.Second {
		t.Fatalf("GET /locks took %s while %d SSE streams were active -- server did not stay responsive to unrelated traffic under this load", elapsed, len(conns))
	}
}
