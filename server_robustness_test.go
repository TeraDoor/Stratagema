package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file is a dedicated adversarial pass over remote.go/serve.go/
// identity.go's HTTP surface, on top of the happy-path coverage already in
// remote_test.go/serve_test.go/server_hardening_test.go/
// identity_auth_test.go: malformed bodies, both auth layers' edge shapes,
// raw HTTP-level tricks (method mismatch, path encoding), RemoteStore's
// client-side failure handling, and TLS verification's default (non-skip)
// path. Organized in the same five groups the task that produced it was
// scoped around.

// ── 1. malformed request bodies against every mutating route ───────────

// mutatingRoute names one POST route this server exposes, along with a
// validBody that decodes and passes that handler's own required-field
// check -- used as the known-good baseline every malformed-body variant
// below is compared against.
type mutatingRoute struct {
	method    string
	path      string
	validBody string
}

// mutatingRoutesForBodyTests is every POST route that actually calls
// decodeBody (i.e. every mutating route except the two that take no body:
// /identities/protected's sibling /interests/propagations/{id}/ack, which
// is POST with no body by design -- see interestsPropagationAckHandler).
// {id} placeholders don't need to resolve to a real row: decodeBody runs
// before any store lookup on every one of these handlers, so a malformed
// body 400s before the id is ever used.
func mutatingRoutesForBodyTests() []mutatingRoute {
	return []mutatingRoute{
		{"POST", "/locks/acquire", `{"resource":"r","identity_id":"i","lease_seconds":30}`},
		{"POST", "/locks/release", `{"resource":"r","identity_id":"i"}`},
		{"POST", "/locks/renew", `{"resource":"r","identity_id":"i"}`},
		{"POST", "/identities", `{"label":"agent-x"}`},
		{"POST", "/identities/protected", `{"label":"agent-x"}`},
		{"POST", "/identities/verify", `{"identity_id":"i"}`},
		{"POST", "/interests", `{"identity_id":"i","resource":"r","label":""}`},
		{"POST", "/interests/some-id/status", `{"status":"active"}`},
		{"POST", "/strategies", `{"name":"n","thesis":"t"}`},
		{"POST", "/strategies/some-id/events", `{"identity_id":"i","kind":"finding","note":"n"}`},
		{"POST", "/strategies/some-id/close", `{"identity_id":"i","outcome":"o"}`},
		{"POST", "/strategies/some-id/group", `{"group":"g"}`},
		{"POST", "/strategies/some-id/status", `{"status":"planning"}`},
	}
}

// rawPost sends body verbatim (no json.Marshal) as a POST to path, letting
// a test construct deliberately malformed wire bytes -- json.Marshal could
// never itself produce invalid JSON, so the earlier tests in this codebase
// (which all go through RemoteStore or valid structs) never exercised this.
func rawPost(t *testing.T, baseURL, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// TestMalformedBodyEveryMutatingRouteRejectedCleanly is item 1's core
// sweep: truncated JSON, non-JSON, and an empty body against every single
// mutating route must all come back as a clean 4xx with the standard
// {"error": "..."} shape -- never a 500, never a hang, never a body that
// isn't valid JSON itself (which would mean a panic recovered by net/http
// into a plaintext dump instead of writeError running).
func TestMalformedBodyEveryMutatingRouteRejectedCleanly(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	badBodies := map[string]string{
		"truncated":       `{"resource":"r","identity`,
		"not_json_at_all": `this is not json at all, just prose`,
		"empty_body":      ``,
		"json_null":       `null`,
		"json_array":      `[1,2,3]`,
	}

	for _, route := range mutatingRoutesForBodyTests() {
		for name, body := range badBodies {
			t.Run(route.path+"/"+name, func(t *testing.T) {
				resp := rawPost(t, srv.URL, route.path, body)
				defer resp.Body.Close()
				respBody, _ := io.ReadAll(resp.Body)
				// json_null and json_array both decode without a JSON
				// syntax error into a zero-value struct (null) or an error
				// (array into struct) -- either way every route's own
				// required-field check (or the decoder itself) must still
				// produce a 4xx, never a 2xx and never a 500.
				if resp.StatusCode < 400 || resp.StatusCode >= 500 {
					t.Fatalf("POST %s with %s body: want a 4xx, got %d: %s", route.path, name, resp.StatusCode, respBody)
				}
				var er errorResponse
				if err := json.Unmarshal(respBody, &er); err != nil || er.Error == "" {
					t.Fatalf("POST %s with %s body: response isn't the standard {\"error\":...} shape: %s", route.path, name, respBody)
				}
			})
		}
	}
}

// TestMalformedBodyWrongJSONTypePerRoute is the "wrong type where a number
// is expected" case, which needs a field name that actually exists per
// route rather than one shared bad body -- lease_seconds (int) on acquire,
// force (bool) on release.
func TestMalformedBodyWrongJSONTypePerRoute(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	cases := []struct {
		path string
		body string
	}{
		{"/locks/acquire", `{"resource":"r","identity_id":"i","lease_seconds":"five"}`},
		{"/locks/release", `{"resource":"r","identity_id":"i","force":"yes"}`},
	}
	for _, c := range cases {
		resp := rawPost(t, srv.URL, c.path, c.body)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST %s with wrong-typed field: want 400, got %d: %s", c.path, resp.StatusCode, body)
		}
	}
}

// TestMalformedBodyUnknownFieldsIgnoredGracefully confirms encoding/json's
// default (DisallowUnknownFields is never set anywhere in serve.go, grep-
// confirmed) really does mean an unrecognized field is silently ignored,
// not rejected -- a real behavior to prove, not assume, since a future
// serve.go change enabling strict decoding would be a silent breaking
// change for older CLI/RemoteStore clients this server has to keep
// tolerating.
func TestMalformedBodyUnknownFieldsIgnoredGracefully(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	resp := rawPost(t, srv.URL, "/identities", `{"label":"agent-extra","totally_unknown_field":"surprise","nested":{"a":1}}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /identities with an unknown extra field: want 200 (ignored), got %d: %s", resp.StatusCode, body)
	}
	var it Identity
	if err := json.Unmarshal(body, &it); err != nil || it.Label != "agent-extra" {
		t.Fatalf("POST /identities with extra field: unexpected response %s (err=%v)", body, err)
	}
}

// TestMalformedBodyLargeBodyIsNowBounded is item 1's resource-exhaustion
// check. Before the fix in this commit, decodeBody read r.Body straight
// into json.NewDecoder with no cap at all -- a client (malicious or just
// buggy) could stream an unbounded body at any mutating route and the
// server would read every byte of it before ever getting to a 400. This
// sends a body well over maxRequestBodyBytes and confirms it's rejected
// (not silently accepted, and not something that reads forever).
func TestMalformedBodyLargeBodyIsNowBounded(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	// A syntactically valid but oversized body: a legitimate field followed
	// by padding comfortably past maxRequestBodyBytes, so any failure is
	// attributable to size, not to some unrelated JSON error.
	padding := strings.Repeat("a", maxRequestBodyBytes+1024)
	body := fmt.Sprintf(`{"label":"agent-huge","padding":"%s"}`, padding)

	resp := rawPost(t, srv.URL, "/identities", body)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /identities with a body over maxRequestBodyBytes: want 400, got %d: %s", resp.StatusCode, respBody)
	}

	// A body comfortably under the cap with the same shape still works --
	// proving the cap doesn't clip legitimate requests.
	resp2 := rawPost(t, srv.URL, "/identities", `{"label":"agent-normal"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body2, _ := io.ReadAll(resp2.Body)
		t.Fatalf("POST /identities with a normal small body: want 200, got %d: %s", resp2.StatusCode, body2)
	}
}

// ── 2. auth edge cases, both layers ─────────────────────────────────────

// TestBearerTokenMalformedShapesTreatedAsNoToken proves bearerToken's exact
// contract: only the precise "Bearer <token>" shape (capital B, one space)
// is recognized; every other shape -- no space, wrong case, or a header
// that's simply something else -- is treated identically to no
// Authorization header at all, i.e. token="" is what
// identitiesVerifyHandler sees. Checked against a *protected* identity so
// the distinction is observable (an unprotected identity would succeed
// regardless).
func TestBearerTokenMalformedShapesTreatedAsNoToken(t *testing.T) {
	local, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer local.Close()
	it, token, err := local.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	srv := httptest.NewServer(newMux(local))
	defer srv.Close()

	verify := func(headerVal string, setHeader bool) (int, string) {
		t.Helper()
		body, _ := json.Marshal(verifyIdentityRequest{IdentityID: it.ID})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/identities/verify", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if setHeader {
			req.Header.Set("Authorization", headerVal)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /identities/verify: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(respBody)
	}

	malformed := map[string]string{
		"no_space":       "Bearer" + token,  // missing the separating space
		"wrong_case":     "bearer " + token, // lowercase scheme
		"empty_token":    "Bearer ",         // correct shape, empty token
		"totally_other":  "Basic " + token,  // a different scheme entirely
		"just_the_token": token,             // no scheme prefix at all
	}
	for name, headerVal := range malformed {
		if code, body := verify(headerVal, true); code != http.StatusUnauthorized {
			t.Fatalf("verify with Authorization=%q (%s): want 401 (treated as no token), got %d: %s", headerVal, name, code, body)
		}
	}

	// The real control: no header at all must produce the exact same
	// outcome as every malformed shape above.
	if code, body := verify("", false); code != http.StatusUnauthorized {
		t.Fatalf("verify with no Authorization header: want 401, got %d: %s", code, body)
	}

	// And the one shape that IS recognized still works, proving the test
	// harness itself is sound.
	if code, body := verify("Bearer "+token, true); code != http.StatusNoContent {
		t.Fatalf("verify with correct Bearer shape: want 204, got %d: %s", code, body)
	}
}

// TestBearerTokenMultipleAuthorizationHeadersUsesFirst documents (not just
// asserts blindly) which value wins when a request carries more than one
// Authorization header -- Go's http.Header.Get always returns the first
// occurrence, and this confirms the server inherits exactly that stdlib
// behavior rather than something route-specific.
func TestBearerTokenMultipleAuthorizationHeadersUsesFirst(t *testing.T) {
	local, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer local.Close()
	it, token, err := local.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	srv := httptest.NewServer(newMux(local))
	defer srv.Close()

	reqBody, _ := json.Marshal(verifyIdentityRequest{IdentityID: it.ID})

	post := func(first, second string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/identities/verify", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("Authorization", first)
		req.Header.Add("Authorization", second)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /identities/verify: %v", err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(respBody)
	}

	// Correct token first, garbage second: succeeds -- first wins.
	if code, body := post("Bearer "+token, "Bearer garbage"); code != http.StatusNoContent {
		t.Fatalf("correct-then-wrong duplicate headers: want 204 (first header wins), got %d: %s", code, body)
	}
	// Garbage first, correct token second: fails -- still first wins, even
	// though a "try every header" policy would have let this through.
	if code, body := post("Bearer garbage", "Bearer "+token); code != http.StatusUnauthorized {
		t.Fatalf("wrong-then-correct duplicate headers: want 401 (first header wins, second never tried), got %d: %s", code, body)
	}
}

// TestAccessTokenHeaderEmptyValueSameAsAbsent is item 2's explicit ask:
// X-Stratagema-Access-Token set to the empty string must be rejected
// identically to the header being absent entirely -- both must produce the
// exact same 401, since requireAccessToken reads via r.Header.Get, which
// returns "" for both cases.
func TestAccessTokenHeaderEmptyValueSameAsAbsent(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newGatedMux(s, "real-secret"))
	defer srv.Close()

	get := func(setEmpty bool) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
		if setEmpty {
			req.Header.Set(accessTokenHeader, "")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	codeAbsent, bodyAbsent := get(false)
	codeEmpty, bodyEmpty := get(true)
	if codeAbsent != http.StatusUnauthorized || codeEmpty != http.StatusUnauthorized {
		t.Fatalf("want both absent and empty-string header to 401, got absent=%d empty=%d", codeAbsent, codeEmpty)
	}
	if bodyAbsent != bodyEmpty {
		t.Fatalf("absent and empty-string access token headers produced different bodies: absent=%q empty=%q, want identical", bodyAbsent, bodyEmpty)
	}
}

// TestVerifyNonexistentIdentitySucceedsAsUnprotected is the genuinely-
// nonexistent-identity case item 2 calls out specifically, distinct from
// "protected without a token." VerifyIdentityToken's own doc comment
// (identity.go) is explicit that this is deliberate: "no row for this
// label at all: unprotected, same as always." This test exists to confirm
// that documented intent actually holds over the real HTTP path, not to
// flag it as a bug -- an identity is "deliberately just a label," and
// nothing before this feature ever required `identity create` to run
// first.
func TestVerifyNonexistentIdentitySucceedsAsUnprotected(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	body, _ := json.Marshal(verifyIdentityRequest{IdentityID: "ident-never-created-at-all"})
	resp, err := http.Post(srv.URL+"/identities/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /identities/verify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("verify of a genuinely nonexistent identity_id, no token: want 204 (documented as unprotected-by-default), got %d: %s", resp.StatusCode, respBody)
	}
}

// TestConstantTimeCompareHandlesLengthMismatchSafely is item 2's "does
// ConstantTimeCompare handle a length mismatch safely" check, exercised at
// both comparison sites this codebase has (the per-identity token in
// identity.go's VerifyIdentityToken, and the server-wide access token in
// serve.go's requireAccessToken) with a token ten times longer than a real
// one, and separately with a valid-hex-but-wrong-length token. subtle.
// ConstantTimeCompare returns 0 immediately when len(x) != len(y) --
// documented stdlib behavior -- so this proves no panic and a clean 401,
// not a proof of the timing property itself (that would need a statistical
// benchmark, out of scope for a functional test).
func TestConstantTimeCompareHandlesLengthMismatchSafely(t *testing.T) {
	local, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer local.Close()
	it, token, err := local.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}
	const serverSecret = "server-secret-length-test"
	srv := httptest.NewServer(newGatedMux(local, serverSecret))
	defer srv.Close()

	tenXToken := strings.Repeat(token, 10)
	validHexWrongLength := strings.Repeat("a", 32) // valid hex, but half a real token's digest length

	for name, badIdentityToken := range map[string]string{
		"10x_longer_than_real":   tenXToken,
		"valid_hex_wrong_length": validHexWrongLength,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("identity token comparison panicked on %s: %v", name, r)
				}
			}()
			body, _ := json.Marshal(verifyIdentityRequest{IdentityID: it.ID})
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/identities/verify", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(accessTokenHeader, serverSecret)
			req.Header.Set("Authorization", "Bearer "+badIdentityToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST /identities/verify (%s): %v", name, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("identity token comparison with %s: want 401, got %d: %s", name, resp.StatusCode, b)
			}
		}()

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("access token comparison panicked on %s: %v", name, r)
				}
			}()
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/health", nil)
			req.Header.Set(accessTokenHeader, badIdentityToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /health (access token, %s): %v", name, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("access token comparison with %s: want 401, got %d: %s", name, resp.StatusCode, b)
			}
		}()
	}

	// Sanity: the real token/secret still work after all that, proving the
	// server didn't wedge itself.
	if err := local.VerifyIdentityToken(it.ID, token); err != nil {
		t.Fatalf("real identity token still valid after length-mismatch probes: %v", err)
	}
}

// Code-inspection note (item 2's last bullet): grep across serve.go and
// identity.go for any `==` comparison of a token/secret that should be
// constant-time turns up none -- the only two call sites that compare a
// caller-supplied secret against a stored one are identity.go's
// VerifyIdentityToken (subtle.ConstantTimeCompare(supplied, tokenHash))
// and serve.go's requireAccessToken (subtle.ConstantTimeCompare(supplied,
// accessToken)), both already using subtle.ConstantTimeCompare. Confirmed
// directly, not assumed:
//
//	grep -n '==' serve.go identity.go | grep -v 'nil\|StatusCode\|== ""\|== 0\|http\.'
//
// returns only the two doc comments describing this, not a live `==` on a
// secret.

// ── 3. HTTP-level edge cases ────────────────────────────────────────────

// TestWrongHTTPMethodEveryRouteIsCleanNotPanic is item 3's method sweep:
// every registered method-specific route (the "METHOD /path" patterns
// newMux uses for everything except /health, /locks, and /stream/interest,
// which are intentionally method-agnostic) must reject the opposite HTTP
// method with a clean 405 and an Allow header, never a panic or a silent
// 200.
func TestWrongHTTPMethodEveryRouteIsCleanNotPanic(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	// path -> the method newMux actually registers for it; the test tries
	// the opposite of each.
	// /identities, /interests, and /strategies/{id}/events each carry BOTH
	// a GET and a POST registration on the same path -- those three are
	// excluded here and tested separately below with DELETE, the real
	// "wrong method" probe for a path that already accepts two methods.
	routes := map[string]string{
		"/locks/get":                        "GET",
		"/locks/acquire":                    "POST",
		"/locks/release":                    "POST",
		"/locks/renew":                      "POST",
		"/identities/protected":             "POST",
		"/identities/verify":                "POST",
		"/interests/some-id/status":         "POST",
		"/interests/propagations":           "GET",
		"/interests/propagations/recent":    "GET",
		"/interests/propagations/some/ack":  "POST",
		"/strategies/some-id":               "GET",
		"/strategies/some-id/events/recent": "GET",
		"/strategies/some-id/close":         "POST",
		"/strategies/some-id/group":         "POST",
		"/strategies/some-id/status":        "POST",
	}
	opposite := func(m string) string {
		if m == "GET" {
			return "POST"
		}
		return "GET"
	}

	for path, registered := range routes {
		wrong := opposite(registered)
		req, err := http.NewRequest(wrong, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("building %s %s: %v", wrong, path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", wrong, path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s (registered as %s-only): want 405, got %d: %s", wrong, path, registered, resp.StatusCode, body)
		}
	}

	// /identities, /interests, and /strategies/{id}/events each have BOTH a
	// GET and a POST registration on the exact same path -- so DELETE (a
	// method neither one registers) is the real "wrong method" probe there.
	for _, path := range []string{"/identities", "/interests", "/strategies/some-id/events"} {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE %s (only GET+POST registered): want 405, got %d", path, resp.StatusCode)
		}
	}

	// The three intentionally method-agnostic routes: confirm both GET and
	// POST reach the same handler (no method restriction registered), i.e.
	// neither one 405s.
	for _, path := range []string{"/health", "/locks"} {
		for _, m := range []string{"GET", "POST"} {
			req, _ := http.NewRequest(m, srv.URL+path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", m, path, err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: want no method restriction (matches newMux's un-prefixed registration), got 405", m, path)
			}
		}
	}
}

// TestPathEncodingTricksInIDSegmentAreInert is item 3's path-traversal-
// shaped-input check. Go's net/http ServeMux (1.22+) pattern matching
// splits on the *raw* (still-encoded) path before percent-decoding each
// matched segment individually -- confirmed by direct experiment before
// writing this test -- so a %2F inside an {id} segment decodes into the
// captured value as a literal "/" character rather than being treated as
// an extra path separator that could re-route the match. Since every id in
// this codebase is an opaque string bound as a SQL parameter (never a
// filesystem path), the practical consequence of a traversal-shaped id is
// just "no such row" -- this test confirms that's genuinely what happens:
// no panic, no unexpected 200 with real data, no 500.
func TestPathEncodingTricksInIDSegmentAreInert(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	tricky := []string{
		"%2e%2e%2f",                // decodes to "../"
		"..%2f..%2fetc",            // decodes to "../../etc"
		"%252e%252e",               // double-encoded -- decodes ONCE to "%2e%2e", not to ".."
		"foo%2fbar",                // embeds a literal "/" inside what should be one segment
		"%00",                      // NUL byte
		strings.Repeat("a%2f", 50), // many embedded encoded slashes
	}
	for _, id := range tricky {
		resp, err := http.Get(srv.URL + "/strategies/" + id)
		if err != nil {
			t.Fatalf("GET /strategies/%s: %v", id, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /strategies/%s: want 200 with a null body (not found, not an error), got %d: %s", id, resp.StatusCode, body)
		}
		if strings.TrimSpace(string(body)) != "null" {
			t.Fatalf("GET /strategies/%s: want JSON null (no such strategy), got %s", id, body)
		}
	}

	// The empty-id-segment case: /strategies/ (no segment at all) doesn't
	// match the {id} pattern -- it's a different, unregistered path, so it
	// 404s rather than reaching the handler with id="".
	resp, err := http.Get(srv.URL + "/strategies/")
	if err != nil {
		t.Fatalf("GET /strategies/: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /strategies/ (empty id segment): want 404 (pattern doesn't match), got %d", resp.StatusCode)
	}
}

// TestConcurrentAcquireOverRealHTTPHasExactlyOneWinner is item 3's
// network-layer concurrency check -- e2e_test.go already proves this
// in-process and across real separate CLI processes, but the HTTP server
// adds its own concurrency surface (net/http spins up one goroutine per
// inbound connection/request), which neither of those exercises. Many
// goroutines hit POST /locks/acquire for the same resource through one
// real httptest.Server at once; exactly one must come back 200, everyone
// else a clean 409 (ErrLockHeld), and the server's own store must agree
// afterward.
func TestConcurrentAcquireOverRealHTTPHasExactlyOneWinner(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	const n = 25
	var wg sync.WaitGroup
	var wins int64
	var conflicts int64
	var other int64

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(acquireLockRequest{
				Resource:   "contended",
				IdentityID: fmt.Sprintf("agent-%d", i),
				Note:       "racing",
			})
			resp, err := http.Post(srv.URL+"/locks/acquire", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Errorf("goroutine %d: POST /locks/acquire: %v", i, err)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			switch resp.StatusCode {
			case http.StatusOK:
				atomic.AddInt64(&wins, 1)
			case http.StatusConflict:
				atomic.AddInt64(&conflicts, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}(i)
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("concurrent POST /locks/acquire on one resource: want exactly 1 winner, got %d wins, %d conflicts, %d other", wins, conflicts, other)
	}
	if conflicts != n-1 {
		t.Fatalf("concurrent POST /locks/acquire: want %d conflicts (409), got %d (and %d other/unexpected statuses)", n-1, conflicts, other)
	}

	l, err := s.GetLock("contended")
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if l == nil {
		t.Fatalf("GetLock(contended) after the race: want a held lock, got nil")
	}
}

// ── 4. RemoteStore client-side robustness ───────────────────────────────

// TestRemoteStoreConnectionRefusedIsCleanError points a RemoteStore at a
// port nothing is listening on and confirms AcquireLock returns a plain,
// wrapped Go error -- not a panic, not a hang past the client's own
// timeout.
func TestRemoteStoreConnectionRefusedIsCleanError(t *testing.T) {
	// Reserve a real port, then close the listener immediately so nothing
	// is bound there -- more honest than a hardcoded port number, which
	// risks colliding with something else already listening on this
	// machine.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	r := newRemoteStore("http://" + addr)
	defer r.Close()

	_, err = r.AcquireLock("res", "ident", "note", "", 0)
	if err == nil {
		t.Fatalf("AcquireLock against a closed port: want a connection error, got nil")
	}
	if !strings.Contains(err.Error(), "contacting") {
		t.Fatalf("AcquireLock against a closed port: error %q doesn't look like RemoteStore.do's wrapped connection error", err.Error())
	}
}

// TestRemoteStoreNonJSONResponseDoesNotPanic points RemoteStore at a real
// HTTP server that returns something else entirely -- an HTML error page,
// standing in for a misconfigured proxy or load balancer in front of the
// real server -- both for a 200 (RemoteStore.do tries to JSON-decode the
// body) and a non-2xx status (RemoteStore.do tries to JSON-unmarshal an
// errorResponse and must fall back cleanly when that also fails).
func TestRemoteStoreNonJSONResponseDoesNotPanic(t *testing.T) {
	htmlHandler := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			w.Write([]byte("<html><body><h1>502 Bad Gateway</h1><p>nginx</p></body></html>"))
		}
	}

	t.Run("200_html", func(t *testing.T) {
		srv := httptest.NewServer(htmlHandler(http.StatusOK))
		defer srv.Close()
		r := newRemoteStore(srv.URL)
		defer r.Close()

		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("RemoteStore.AcquireLock against a 200 HTML response panicked: %v", p)
				}
			}()
			if _, err := r.AcquireLock("res", "ident", "note", "", 0); err == nil {
				t.Fatalf("AcquireLock against a 200 HTML (non-JSON) response: want an error, got nil")
			}
		}()
	})

	t.Run("502_html", func(t *testing.T) {
		srv := httptest.NewServer(htmlHandler(http.StatusBadGateway))
		defer srv.Close()
		r := newRemoteStore(srv.URL)
		defer r.Close()

		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("RemoteStore.AcquireLock against a 502 HTML error page panicked: %v", p)
				}
			}()
			_, err := r.AcquireLock("res", "ident", "note", "", 0)
			if err == nil {
				t.Fatalf("AcquireLock against a 502 HTML error page: want an error, got nil")
			}
			// do's fallback path (errorResponse JSON-unmarshal fails) must
			// still surface *something* useful, not an empty message.
			if err.Error() == "" {
				t.Fatalf("AcquireLock against a 502 HTML error page: got an error with an empty message")
			}
		}()
	})
}

// TestRemoteStoreConnectionClosedMidResponseIsCleanError uses a
// Hijacker to accept the connection, write a truncated/malformed HTTP
// response, and close the raw TCP connection immediately -- simulating a
// server that dies or a proxy that drops the connection mid-stream, which
// httptest.Server's normal Close() (a clean, complete response) never
// exercises.
func TestRemoteStoreConnectionClosedMidResponseIsCleanError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("test server's ResponseWriter doesn't support Hijacker")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		// Claim a large body, then send only a few bytes of it and close --
		// the client should see a truncated-read error, not hang or panic.
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 10000\r\n\r\n{\"partial")
		buf.Flush()
		conn.Close()
	}))
	defer srv.Close()

	r := newRemoteStore(srv.URL)
	defer r.Close()

	done := make(chan struct{})
	var err error
	go func() {
		_, err = r.AcquireLock("res", "ident", "note", "", 0)
		close(done)
	}()
	select {
	case <-done:
		if err == nil {
			t.Fatalf("AcquireLock against a connection closed mid-response: want an error, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("AcquireLock against a connection closed mid-response: never returned (hang)")
	}
}

// TestRemoteStoreCloseIsSafeMultipleTimesAndUnused confirms Close's own
// doc comment: "this must never panic or error," called twice in a row
// (mirroring nothing unusual, since every cmd* function's defer already
// calls it once) and called on a RemoteStore that never made a single
// request.
func TestRemoteStoreCloseIsSafeMultipleTimesAndUnused(t *testing.T) {
	r := newRemoteStore("http://127.0.0.1:0")
	if err := r.Close(); err != nil {
		t.Fatalf("first Close() on an unused RemoteStore: %v, want nil", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close() on an already-closed RemoteStore: %v, want nil", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("third Close() call: %v, want nil (must stay safe no matter how many times it's called)", err)
	}
}

// ── 5. TLS edge cases ────────────────────────────────────────────────────

// TestServeOverTLSWithVerificationEnabledFailsCleanly is TestServeOverTLS's
// (server_hardening_test.go) counterpart with the default, real
// verification path: InsecureSkipVerify left false (the client default)
// against the same self-signed cert must fail with a genuine x509
// verification error, cleanly -- not a hang, not a panic -- proving the
// server's TLS setup isn't accidentally doing something that would make an
// untrusted cert look trusted.
func TestServeOverTLSWithVerificationEnabledFailsCleanly(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCertForTest(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	defer tlsLn.Close()

	s := newTestStore(t)
	handler := newGatedMux(s, "")
	go http.Serve(tlsLn, handler)

	// A real, unmodified client (no InsecureSkipVerify, no custom
	// RootCAs) -- the self-signed cert isn't in any trust store, so this
	// must fail verification.
	client := &http.Client{Timeout: 5 * time.Second}
	url := "https://" + ln.Addr().String() + "/health"

	_, err = client.Get(url)
	if err == nil {
		t.Fatalf("GET %s with default TLS verification against a self-signed cert: want an error, got nil (success would mean verification isn't really happening)", url)
	}
	var uerr *x509.UnknownAuthorityError
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &uerr) && !errors.As(err, &certErr) && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("GET %s: want a certificate-verification-shaped error, got %v", url, err)
	}
}
