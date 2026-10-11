// Package kagenttest serves a fake kagent controller's SessionService for
// tests: ListSessions over a set of Sessions the test puts and deletes, in
// pages, as kagent lists them (a deleted Session is gone from the listing).
package kagenttest

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/giantswarm/workspace-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/workspace-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// Server is the fake controller.
type Server struct {
	apiv1alpha1.UnimplementedSessionServiceServer

	// Target is the grpc:// address it listens on.
	Target string
	// PageSize is how many Sessions one page holds; zero is 2, so a test
	// walks several pages.
	PageSize int

	mu       sync.Mutex
	sessions map[string]kagent.Session
	err      error
	bearers  []string
}

// New starts a fake controller on a loopback port, stopped with the test.
func New(t *testing.T) *Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("kagenttest: listen: %v", err)
	}
	s := &Server{Target: "grpc://" + lis.Addr().String(), sessions: map[string]kagent.Session{}}
	srv := grpc.NewServer()
	apiv1alpha1.RegisterSessionServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return s
}

// Put adds or replaces a Session.
func (s *Server) Put(sess kagent.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

// Turn moves a Session's last turn to at.
func (s *Server) Turn(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	sess.LastTurn = at
	s.sessions[id] = sess
}

// Delete removes a Session, as kagent hides a deleted one.
func (s *Server) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// Fail makes every call fail with err until it is called with nil.
func (s *Server) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Bearers are the authorization headers the calls carried.
func (s *Server) Bearers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.bearers)
}

// ListSessions answers every Session, all creators only, in id order and
// pages of PageSize.
func (s *Server) ListSessions(ctx context.Context, req *apiv1alpha1.ListSessionsRequest) (*apiv1alpha1.ListSessionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		s.bearers = append(s.bearers, strings.Join(md.Get("authorization"), ","))
	}
	if s.err != nil {
		return nil, s.err
	}
	if !req.GetAllCreators() {
		return nil, status.Error(codes.InvalidArgument, "kagenttest serves all_creators listings only")
	}
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		if id > req.GetPage().GetPageToken() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	size := s.PageSize
	if size <= 0 {
		size = 2
	}
	resp := &apiv1alpha1.ListSessionsResponse{Page: &apiv1alpha1.PageResponse{}}
	for i, id := range ids {
		if i == size {
			resp.Page.NextPageToken = ids[i-1]
			break
		}
		resp.Sessions = append(resp.Sessions, toProto(s.sessions[id]))
	}
	return resp, nil
}

func toProto(sess kagent.Session) *apiv1alpha1.Session {
	out := &apiv1alpha1.Session{Id: sess.ID, Name: sess.Name, Creator: sess.Creator}
	if !sess.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(sess.CreatedAt)
	}
	if !sess.LastTurn.IsZero() {
		out.LastTurnTime = timestamppb.New(sess.LastTurn)
	}
	if sess.Handle != "" {
		out.VolumeSource = &apiv1alpha1.SessionVolumeSource{
			Volume: &apiv1alpha1.SessionVolume{CsiDriver: sess.Driver, VolumeHandle: sess.Handle},
			Mounts: []*apiv1alpha1.SessionVolumeMount{{SubPath: "sessions/${SESSION_ID}", MountPath: "/workspace"}},
		}
	}
	return out
}
