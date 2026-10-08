package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func loginOnce(t *testing.T, s *Server, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"`+password+`"}`))
	req.RemoteAddr = "192.0.2.10:12345"
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	return w
}

func TestLoginAcceptsBcryptPassword(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.Password = "correct-horse"
	s := newActionServer(t, cfg)
	if s.passwordMatches("wrong") {
		t.Fatal("wrong password must not match")
	}
	if !s.passwordMatches("correct-horse") {
		t.Fatal("configured password must match")
	}
	if w := loginOnce(t, s, "correct-horse"); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestLoginAcceptsPreHashedPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("plaintext-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	cfg := testConfig(t)
	cfg.Admin.Password = string(hash)
	s := newActionServer(t, cfg)
	if !s.passwordPreHashed {
		t.Fatal("a bcrypt hash in config must be used verbatim")
	}
	if w := loginOnce(t, s, "plaintext-secret"); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestLoginLocksOutAfterFiveFailures(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.Password = "secret"
	s := newActionServer(t, cfg)

	for i := 0; i < adminMaxFails; i++ {
		if w := loginOnce(t, s, "nope"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, w.Code)
		}
	}
	// The next attempt — even with the right password — is locked out.
	w := loginOnce(t, s, "secret")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("locked response should carry Retry-After")
	}
}
