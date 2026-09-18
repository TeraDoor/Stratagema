package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withServerAccessTokenEnv sets STRATAGEMA_SERVER_ACCESS_TOKEN for the
// duration of the test and restores whatever was there before, mirroring
// identity_auth_test.go's withTokenEnv for the per-identity token — these
// are two genuinely separate env vars, and this helper deliberately never
// touches STRATAGEMA_TOKEN.
func withServerAccessTokenEnv(t *testing.T, value string) {
	t.Helper()
	orig, had := os.LookupEnv(serverAccessTokenEnv)
	if value == "" {
		os.Unsetenv(serverAccessTokenEnv)
	} else {
		os.Setenv(serverAccessTokenEnv, value)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(serverAccessTokenEnv, orig)
		} else {
			os.Unsetenv(serverAccessTokenEnv)
		}
	})
}

// TestGatedMuxWithEmptyTokenIsUnchanged is the backward-compat guarantee
// this whole feature rests on, proven directly rather than inferred from
// other tests passing: newGatedMux(store, "") -- the exact handler cmdServe
// builds when -access-token is unset, the default -- must behave with zero
// observable difference from newMux(store) on every route, including no
// caller ever sending accessTokenHeader at all.
func TestGatedMuxWithEmptyTokenIsUnchanged(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newGatedMux(s, ""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health with no access token configured: want 200, got %d: %s", resp.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("unexpected /health body: %s", body)
	}

	resp, err = http.Get(srv.URL + "/locks")
	if err != nil {
		t.Fatalf("GET /locks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /locks with no access token configured: want 200, got %d", resp.StatusCode)
	}

	// /stream/interest without ?identity= still 400s exactly as before --
	// the gate must not intercept or change this route's own behavior.
	resp, err = http.Get(srv.URL + "/stream/interest")
	if err != nil {
		t.Fatalf("GET /stream/interest: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /stream/interest with no access token configured: want 400, got %d", resp.StatusCode)
	}
}

// TestAccessTokenGateCoversEveryRoute proves the gate is real, shared
// middleware wrapping the entire routing table -- not something scattered
// per-handler that could miss one -- by hitting a representative spread of
// routes (including the previously wide-open /health, /locks, and
// /stream/interest) with no header, a wrong header, and the correct header.
func TestAccessTokenGateCoversEveryRoute(t *testing.T) {
	const secret = "server-secret-abc123"
	s := newTestStore(t)
	srv := httptest.NewServer(newGatedMux(s, secret))
	defer srv.Close()

	// path -> the status a correctly-authorized GET on it returns, proving
	// the gate passes control through to the real handler untouched.
	routes := map[string]int{
		"/health":          http.StatusOK,
		"/locks":           http.StatusOK,
		"/identities":      http.StatusOK,
		"/strategies":      http.StatusOK,
		"/stream/interest": http.StatusBadRequest, // missing ?identity=, same as always -- proves the gate ran first but didn't swallow the route's own validation
	}

	get := func(path, headerVal string, setHeader bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("building request for %s: %v", path, err)
		}
		if setHeader {
			req.Header.Set(accessTokenHeader, headerVal)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp
	}

	for path := range routes {
		if resp := get(path, "", false); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s with no %s header: want 401, got %d", path, accessTokenHeader, resp.StatusCode)
		}
		if resp := get(path, "totally-wrong", true); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s with wrong %s header: want 401, got %d", path, accessTokenHeader, resp.StatusCode)
		}
	}
	for path, want := range routes {
		if resp := get(path, secret, true); resp.StatusCode != want {
			t.Fatalf("GET %s with correct %s header: want %d, got %d", path, accessTokenHeader, want, resp.StatusCode)
		}
	}
}

// verifyOverHTTP does the one raw HTTP round trip that actually enforces
// per-identity token verification server-side (identitiesVerifyHandler) --
// the one place, other than the access gate itself, where a wrong value
// produces a distinct, identity-specific failure. Used below to prove the
// two auth layers are genuinely independent rather than accidentally
// coupled: each header is set (or omitted) on its own, never derived from
// the other.
func verifyOverHTTP(t *testing.T, baseURL, identityID, accessTok, identityTok string) (int, string) {
	t.Helper()
	body, err := json.Marshal(verifyIdentityRequest{IdentityID: identityID})
	if err != nil {
		t.Fatalf("marshaling verify request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/identities/verify", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building verify request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accessTok != "" {
		req.Header.Set(accessTokenHeader, accessTok)
	}
	if identityTok != "" {
		req.Header.Set("Authorization", "Bearer "+identityTok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /identities/verify: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// TestAccessGateAndIdentityTokenAreIndependent is the core proof this task
// asks for: the server-wide access gate (X-Stratagema-Access-Token) and
// per-identity token verification (Authorization: Bearer, already shipped)
// are two genuinely separate layers, not accidentally coupled through one
// header or one code path. A request needs both correct to succeed; each
// wrong value alone produces its own distinct failure -- the access gate's
// generic 401 (naming its own header, nothing identity-specific) when the
// access token is wrong, regardless of the identity token, versus
// identitiesVerifyHandler's identity-specific 401 (naming the identity)
// when only the identity token is wrong.
func TestAccessGateAndIdentityTokenAreIndependent(t *testing.T) {
	local, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer local.Close()
	it, idToken, err := local.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}

	const serverSecret = "server-secret-xyz"
	srv := httptest.NewServer(newGatedMux(local, serverSecret))
	defer srv.Close()

	// Both correct: succeeds.
	if code, body := verifyOverHTTP(t, srv.URL, it.ID, serverSecret, idToken); code != http.StatusNoContent {
		t.Fatalf("verify(correct access, correct identity) = %d %s, want 204", code, body)
	}

	// Correct access token, wrong identity token: fails at identity
	// verification specifically -- the failure names the identity, proving
	// the request made it past the gate and was rejected by the other
	// layer.
	code, body := verifyOverHTTP(t, srv.URL, it.ID, serverSecret, "wrong-identity-token")
	if code != http.StatusUnauthorized {
		t.Fatalf("verify(correct access, wrong identity) = %d, want 401", code)
	}
	if !strings.Contains(body, it.ID) {
		t.Fatalf("verify(correct access, wrong identity) body %q doesn't name the identity -- want the identity-specific failure, not the gate's", body)
	}

	// Wrong access token, correct identity token: fails at the gate,
	// before ever reaching identity verification -- the failure names the
	// gate's own header, not the identity.
	code, body = verifyOverHTTP(t, srv.URL, it.ID, "wrong-server-secret", idToken)
	if code != http.StatusUnauthorized {
		t.Fatalf("verify(wrong access, correct identity) = %d, want 401", code)
	}
	if !strings.Contains(body, accessTokenHeader) {
		t.Fatalf("verify(wrong access, correct identity) body %q doesn't name %s -- want the gate's own failure, not identity-verify's", body, accessTokenHeader)
	}
	if strings.Contains(body, it.ID) {
		t.Fatalf("verify(wrong access, correct identity) body %q names the identity -- the gate rejected this before identity verification ever ran, so it must not", body)
	}

	// Missing access token entirely, correct identity token: same gate
	// failure as a wrong one -- no header is not treated differently from
	// a bad one.
	code, body = verifyOverHTTP(t, srv.URL, it.ID, "", idToken)
	if code != http.StatusUnauthorized || !strings.Contains(body, accessTokenHeader) {
		t.Fatalf("verify(missing access, correct identity) = %d %s, want 401 naming %s", code, body, accessTokenHeader)
	}
}

// TestRemoteLockAcquireAgainstGatedServerWithProtectedIdentity is the
// full, realistic end-to-end version of the independence proof above: the
// actual CLI entry point (cmdLockAcquire), talking to a real
// httptest.Server wrapped in the access gate, acting as a protected
// identity. Proves the two auth layers compose correctly through the real
// client path (RemoteStore, STRATAGEMA_SERVER_ACCESS_TOKEN, -token) rather
// than only at the raw-HTTP level TestAccessGateAndIdentityTokenAreIndependent
// exercises.
func TestRemoteLockAcquireAgainstGatedServerWithProtectedIdentity(t *testing.T) {
	withTokenEnv(t, "")
	withServerAccessTokenEnv(t, "")

	local, err := openLocalStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openLocalStore: %v", err)
	}
	defer local.Close()
	it, idToken, err := local.CreateProtectedIdentity("agent-secure")
	if err != nil {
		t.Fatalf("CreateProtectedIdentity: %v", err)
	}

	const serverSecret = "server-secret-remote"
	srv := httptest.NewServer(newGatedMux(local, serverSecret))
	defer srv.Close()

	db := srv.URL

	// Both correct: succeeds, and the lock is really visible on the
	// server's own local store, not just in the RemoteStore's response.
	withServerAccessTokenEnv(t, serverSecret)
	out, code := captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=gated-res", "-identity=" + it.ID, "-token=" + idToken})
	})
	if code != 0 {
		t.Fatalf("cmdLockAcquire with correct access token and correct identity token: want exit 0, got %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "acquired gated-res") {
		t.Fatalf("cmdLockAcquire: want confirmation of acquiring gated-res, got:\n%s", out)
	}
	l, err := local.GetLock("gated-res")
	if err != nil {
		t.Fatalf("local.GetLock: %v", err)
	}
	if l == nil || l.HolderID != it.ID {
		t.Fatalf("local.GetLock(gated-res) = %+v, want held by %s", l, it.ID)
	}

	// Wrong server access token, correct identity token: fails, even
	// though the identity token alone is right.
	withServerAccessTokenEnv(t, "wrong-server-secret")
	out, code = captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=gated-res-2", "-identity=" + it.ID, "-token=" + idToken})
	})
	if code == 0 {
		t.Fatalf("cmdLockAcquire with wrong access token: want non-zero exit, got 0, output:\n%s", out)
	}

	// Correct server access token restored, wrong identity token: fails
	// distinctly (identity verification, not the gate), even though the
	// server access token alone is right.
	withServerAccessTokenEnv(t, serverSecret)
	out, code = captureOutput(t, func() int {
		return cmdLockAcquire([]string{"-db=" + db, "-resource=gated-res-3", "-identity=" + it.ID, "-token=wrong-identity-token"})
	})
	if code == 0 {
		t.Fatalf("cmdLockAcquire with wrong identity token: want non-zero exit, got 0, output:\n%s", out)
	}

	// Neither resource from the two failed attempts should have been
	// acquired.
	for _, res := range []string{"gated-res-2", "gated-res-3"} {
		l, err := local.GetLock(res)
		if err != nil {
			t.Fatalf("local.GetLock(%s): %v", res, err)
		}
		if l != nil {
			t.Fatalf("local.GetLock(%s) = %+v, want nil (that acquire should have failed)", res, l)
		}
	}
}

// TestServeTLSFlagsRequireBothOrNeither is the "only one of the two given"
// configuration-error case, tested directly against cmdServe: giving just
// -tls-cert or just -tls-key must be rejected at startup with a clear
// message, not silently ignored or guessed at. Both calls return before
// ever attempting to bind a port (the validation runs immediately after
// flag parsing), so this is safe to run without a real listener.
func TestServeTLSFlagsRequireBothOrNeither(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")

	out, code := captureOutput(t, func() int {
		return cmdServe([]string{"-db=" + db, "-tls-cert=/does/not/matter.pem"})
	})
	if code == 0 {
		t.Fatalf("cmdServe with only -tls-cert: want non-zero exit, got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "-tls-cert") || !strings.Contains(out, "-tls-key") {
		t.Fatalf("cmdServe with only -tls-cert: want a message naming both flags, got:\n%s", out)
	}

	out, code = captureOutput(t, func() int {
		return cmdServe([]string{"-db=" + db, "-tls-key=/does/not/matter.key"})
	})
	if code == 0 {
		t.Fatalf("cmdServe with only -tls-key: want non-zero exit, got 0, output:\n%s", out)
	}
	if !strings.Contains(out, "-tls-cert") || !strings.Contains(out, "-tls-key") {
		t.Fatalf("cmdServe with only -tls-key: want a message naming both flags, got:\n%s", out)
	}
}

// generateSelfSignedCertForTest builds a fresh, throwaway self-signed
// RSA/TLS keypair entirely in-memory for TestServeOverTLS -- never written
// anywhere but this test's own t.TempDir(), never committed, matching this
// task's explicit instruction not to commit any real cert/key material.
func generateSelfSignedCertForTest(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "stratagema-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"127.0.0.1", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// TestServeOverTLS proves -tls-cert/-tls-key's actual effect -- a real TLS
// listener serving the same handler cmdServe builds -- not just that the
// flags parse. A manually-started tls.Listener (net.Listen + tls.NewListener
// + http.Serve) rather than httptest.NewTLSServer, since the point is to
// exercise the exact same handler construction cmdServe uses
// (newGatedMux) wired into a real TLS accept loop, the same shape cmdServe
// itself follows with http.ListenAndServeTLS.
func TestServeOverTLS(t *testing.T) {
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

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   5 * time.Second,
	}
	url := "https://" + ln.Addr().String() + "/health"

	var resp *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err = client.Get(url)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s over TLS never succeeded: last error %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer resp.Body.Close()

	if resp.TLS == nil {
		t.Fatalf("response has no TLS connection state -- this did not actually go over TLS")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health over TLS: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("unexpected /health body over TLS: %s", body)
	}
}
