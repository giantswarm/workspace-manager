package signin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// tokenServer is a fake OAuth 2.0 token endpoint that rotates refresh tokens
// like GitHub: redeeming refresh-N returns access-(N+1) and refresh-(N+1),
// and a second redemption of refresh-N is refused with invalid_grant, its
// description echoing the token to prove the store never repeats it.
type tokenServer struct {
	*httptest.Server
	delay time.Duration

	mu          sync.Mutex
	redemptions map[string]int
	issued      []string // every token value the server handed out
}

func newTokenServer(t *testing.T, delay time.Duration) *tokenServer {
	ts := &tokenServer{delay: delay, redemptions: map[string]int{}}
	ts.Server = httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tokenServer) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if id, secret, ok := r.BasicAuth(); (!ok || id != "client" || secret != "client-secret") &&
		(r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "client-secret") {
		http.Error(w, "bad client", http.StatusUnauthorized)
		return
	}
	time.Sleep(ts.delay)
	rt := r.Form.Get("refresh_token")
	ts.mu.Lock()
	ts.redemptions[rt]++
	n := ts.redemptions[rt]
	ts.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if n > 1 || !strings.HasPrefix(rt, "refresh-") {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "refresh token " + rt + " was already used",
		})
		return
	}
	var gen int
	_, _ = fmt.Sscanf(rt, "refresh-%d", &gen)
	access, refresh := fmt.Sprintf("access-%d", gen+1), fmt.Sprintf("refresh-%d", gen+1)
	ts.mu.Lock()
	ts.issued = append(ts.issued, access, refresh)
	ts.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  access,
		"refresh_token": refresh,
		"token_type":    "bearer",
		"expires_in":    8 * 3600,
	})
}

func (ts *tokenServer) count(rt string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.redemptions[rt]
}

func (ts *tokenServer) total() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := 0
	for _, c := range ts.redemptions {
		n += c
	}
	return n
}

func (ts *tokenServer) tokens() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.issued...)
}

func (ts *tokenServer) configs() OAuth2Configs {
	return OAuth2ConfigFunc(func(_ context.Context, instance string) (*oauth2.Config, error) {
		return &oauth2.Config{
			ClientID:     "client",
			ClientSecret: "client-secret",
			Endpoint:     oauth2.Endpoint{TokenURL: ts.URL + "/login/oauth/access_token"},
		}, nil
	})
}

// syncBuffer is a log sink safe for concurrent handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs() (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// assertNoSecret fails when any secret value, in clear, hex or base64,
// appears in the text (captured logs and error messages).
func assertNoSecret(t *testing.T, text string, secrets ...[]byte) {
	t.Helper()
	for _, s := range secrets {
		if len(s) == 0 {
			continue
		}
		for _, form := range []string{
			string(s),
			hex.EncodeToString(s),
			base64.StdEncoding.EncodeToString(s),
			base64.RawURLEncoding.EncodeToString(s),
		} {
			if strings.Contains(text, form) {
				t.Errorf("a secret value appears in the output:\n%s", text)
				return
			}
		}
	}
}

func bytesOf(values ...string) [][]byte {
	out := make([][]byte, 0, len(values))
	for _, v := range values {
		out = append(out, []byte(v))
	}
	return out
}
