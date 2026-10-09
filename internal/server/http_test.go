package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
