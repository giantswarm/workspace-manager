// Package kagent reads the kagent controller's Sessions for the session
// directories' cleanup: SessionService/ListSessions over every creator,
// walked to the end, as the manager's own ServiceAccount. Each Session
// carries its last turn and the workspace volume it works in; the cleanup
// needs nothing else from kagent.
package kagent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	apiv1alpha1 "github.com/giantswarm/workspace-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// Target schemes: plaintext h2c (the in-cluster controller Service) and TLS.
const (
	SchemePlaintext = "grpc"
	SchemeTLS       = "grpcs"
)

const (
	// pageSize is the controller's cap for ListSessions.
	pageSize = 100
	// maxPages bounds how far a listing is followed, so a misbehaving
	// next_page_token cannot loop forever: 100 000 Sessions.
	maxPages = 1000
	// maxMessageBytes bounds one response.
	maxMessageBytes = 16 << 20
)

// ErrIncomplete is returned when a listing did not reach its end. A Session
// missing from a listing loses its directory at once, so the cleanup never
// acts on an incomplete one.
var ErrIncomplete = errors.New("the Session listing did not reach its end")

// Session is what the cleanup reads of a kagent Session.
type Session struct {
	ID      string
	Name    string
	Creator string
	// CreatedAt is when the Session was created; LastTurn when its last turn
	// was dispatched, zero before the first.
	CreatedAt time.Time
	LastTurn  time.Time
	// Driver and Handle name the workspace volume the Session works in, by
	// its CSI driver and the driver's handle; empty for a Session without
	// one.
	Driver, Handle string
}

// Client reads one kagent controller's Sessions. It is safe for concurrent
// use.
type Client struct {
	sessions  apiv1alpha1.SessionServiceClient
	tokenFile string
	closeConn func() error
}

// ParseTarget splits a grpc:// or grpcs:// target into the host:port the gRPC
// client dials and whether the connection uses TLS. A grpcs target without a
// port is 443; a grpc target must name its port.
func ParseTarget(target string) (hostPort string, useTLS bool, err error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", false, fmt.Errorf("kagent target %q: %w", target, err)
	}
	switch u.Scheme {
	case SchemePlaintext:
	case SchemeTLS:
		useTLS = true
	default:
		return "", false, fmt.Errorf("kagent target %q must use the %s:// or %s:// scheme", target, SchemePlaintext, SchemeTLS)
	}
	if u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", false, fmt.Errorf("kagent target %q must be %s://host[:port] with no path", target, u.Scheme)
	}
	if u.Port() != "" {
		return u.Host, useTLS, nil
	}
	if !useTLS {
		return "", false, fmt.Errorf("kagent target %q must name a port", target)
	}
	return net.JoinHostPort(u.Hostname(), "443"), true, nil
}

// Dial builds a Client for target that authenticates every call with the
// token in tokenFile, read anew per call since a projected token rotates;
// an empty tokenFile sends none. The connection is established lazily on the
// first call.
func Dial(target, tokenFile string) (*Client, error) {
	hostPort, useTLS, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(hostPort,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes)),
	)
	if err != nil {
		return nil, fmt.Errorf("kagent: dial %s: %w", target, err)
	}
	c := NewClient(conn, tokenFile)
	c.closeConn = conn.Close
	return c, nil
}

// NewClient builds a Client on an existing connection (tests serve a fake
// controller).
func NewClient(conn grpc.ClientConnInterface, tokenFile string) *Client {
	return &Client{sessions: apiv1alpha1.NewSessionServiceClient(conn), tokenFile: tokenFile, closeConn: func() error { return nil }}
}

// Close releases the connection Dial opened.
func (c *Client) Close() error { return c.closeConn() }

// Sessions lists every creator's Sessions, walked to the end; a listing cut
// short is ErrIncomplete.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	ctx, err := c.authenticated(ctx)
	if err != nil {
		return nil, err
	}
	var out []Session
	pageToken := ""
	for range maxPages {
		resp, err := c.sessions.ListSessions(ctx, &apiv1alpha1.ListSessionsRequest{
			AllCreators: true,
			Page:        &apiv1alpha1.PageRequest{Limit: pageSize, PageToken: pageToken},
		})
		if err != nil {
			return nil, fmt.Errorf("kagent: list sessions: %w", err)
		}
		for _, s := range resp.GetSessions() {
			out = append(out, sessionOf(s))
		}
		if pageToken = resp.GetPage().GetNextPageToken(); pageToken == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("kagent: %w after %d pages", ErrIncomplete, maxPages)
}

// authenticated is ctx carrying the token as the call's bearer.
func (c *Client) authenticated(ctx context.Context) (context.Context, error) {
	if c.tokenFile == "" {
		return ctx, nil
	}
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("kagent: read the token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("kagent: the token file %s is empty", c.tokenFile)
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), nil
}

func sessionOf(s *apiv1alpha1.Session) Session {
	out := Session{ID: s.GetId(), Name: s.GetName(), Creator: s.GetCreator()}
	if t := s.GetCreatedAt(); t != nil {
		out.CreatedAt = t.AsTime()
	}
	if t := s.GetLastTurnTime(); t != nil {
		out.LastTurn = t.AsTime()
	}
	if v := s.GetVolumeSource().GetVolume(); v != nil {
		out.Driver, out.Handle = v.GetCsiDriver(), v.GetVolumeHandle()
	}
	return out
}
