package provider

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostKind is a Kind whose git host is host; nothing else is called.
type hostKind struct {
	Kind
	host string
}

func (k hostKind) Hosts() Hosts {
	return Hosts{Git: []Host{{Name: k.host, Scheme: SchemeBasic, User: "x"}}}
}

// testCertClient trusts httptest's certificate, which every TLS server of
// httptest serves.
func testCertClient(t *testing.T) *http.Client {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	return ts.Client()
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A manager rolled while its provider is not reachable yet (connection
// refused, as the lab's replaced GitHub answers right after a roll) is not
// Ready until the provider answers: the Service holds back the Session
// starts that would wait out the access check's bound.
func TestReachReadyOnceTheProviderAnswers(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close()) // refused until the provider starts

	r := &Reach{
		Instances: []Instance{{Name: "fake", Kind: hostKind{host: addr}}},
		HTTP:      testCertClient(t),
		Wait:      time.Minute,
		Interval:  10 * time.Millisecond,
		Logger:    quietLog(),
	}
	done := make(chan struct{})
	go func() { r.Run(t.Context()); close(done) }()

	time.Sleep(100 * time.Millisecond)
	assert.False(t, r.Ready(), "Ready while the provider refuses connections")

	// The provider answers with any status: a 401 on its root is an answer.
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	ts.Listener, err = net.Listen("tcp", addr)
	require.NoError(t, err)
	ts.StartTLS()
	t.Cleanup(ts.Close)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("not Ready after the provider answered")
	}
	assert.True(t, r.Ready())
}

// A provider that never answers keeps the manager unready for the wait only:
// an outage is the tools' answer, never an empty Service.
func TestReachReadyAfterTheWait(t *testing.T) {
	hung := make(chan struct{})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-hung:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(hung) })

	r := &Reach{
		Instances: []Instance{{Name: "slow", Kind: hostKind{host: ts.Listener.Addr().String()}}},
		HTTP:      ts.Client(),
		Wait:      200 * time.Millisecond,
		Interval:  10 * time.Millisecond,
		Logger:    quietLog(),
	}
	start := time.Now()
	r.Run(t.Context())
	assert.True(t, r.Ready())
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)
}

// Without providers there is nothing to wait for.
func TestReachWithoutProviders(t *testing.T) {
	r := &Reach{Logger: quietLog()}
	r.Run(t.Context())
	assert.True(t, r.Ready())
}

func TestReachURL(t *testing.T) {
	assert.Equal(t, "https://api.github.com", ReachURL(Hosts{
		Git: []Host{{Name: "github.com"}}, API: []Host{{Name: "api.github.com"}},
	}))
	assert.Equal(t, "https://gitlab.example.com/api/", ReachURL(Hosts{
		Git: []Host{{Name: "gitlab.example.com"}}, API: []Host{{Name: "gitlab.example.com", PathPrefix: "/api/"}},
	}))
	assert.Equal(t, "https://ghe.example.com", ReachURL(Hosts{Git: []Host{{Name: "ghe.example.com"}}}))
	assert.Empty(t, ReachURL(Hosts{}))
}
