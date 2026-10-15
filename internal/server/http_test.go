package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/api"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := New(Config{Addr: "127.0.0.1:0"}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postMCP(t *testing.T, url, session, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A session begun on one replica continues on another: the chart may run two
// replicas behind one Service, and muster's requests land on either.
func TestSessionMovesBetweenReplicas(t *testing.T) {
	a, b := newTestServer(t), newTestServer(t)

	resp := postMCP(t, a.URL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	session := resp.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, session)

	resp = postMCP(t, b.URL, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp = postMCP(t, b.URL, session, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// Without provider sign-ins the browser pages answer that none is configured
// instead of a bare 404.
func TestPagesWithoutProvider(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/signin", "/connect/github", "/callback/github"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+path, nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
			assert.Contains(t, string(body), "No provider configured")
		})
	}
}

// A body beyond the bound is refused, and the endpoint keeps answering.
func TestMCPBodyIsBounded(t *testing.T) {
	ts := newTestServer(t)

	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxMCPBodyBytes) + `"}}`
	resp := postMCP(t, ts.URL, "", big)
	assert.GreaterOrEqual(t, resp.StatusCode, 400)

	resp = postMCP(t, ts.URL, "", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// /readyz holds a starting replica back until its providers answered
// (Config.Ready); /healthz does not wait for them.
func TestReadyzWaitsForTheProviders(t *testing.T) {
	var ready atomic.Bool
	srv, err := New(Config{Addr: "127.0.0.1:0", Ready: ready.Load}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	status := func(path string) int {
		resp, err := http.Get(ts.URL + path) //nolint:noctx // test
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusServiceUnavailable, status("/readyz"))
	assert.Equal(t, http.StatusOK, status("/healthz"))
	ready.Store(true)
	assert.Equal(t, http.StatusOK, status("/readyz"))
}

// A stopping replica keeps serving for the drain, unready, so a connection
// the Service still sends it is answered, never refused; then it closes.
func TestRunDrainsBeforeShutdown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	srv, err := New(Config{Addr: addr, Drain: 500 * time.Millisecond}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	get := func() (int, error) {
		resp, err := http.Get("http://" + addr + "/readyz") //nolint:noctx // test
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	require.Eventually(t, func() bool { c, err := get(); return err == nil && c == http.StatusOK }, 5*time.Second, 10*time.Millisecond)

	stop()
	require.Eventually(t, func() bool { c, err := get(); return err == nil && c == http.StatusServiceUnavailable }, time.Second, 10*time.Millisecond,
		"the stop signal closed the listener at once instead of draining")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped after the drain")
	}
	_, err = get()
	assert.Error(t, err, "still serving after the drain")
}
