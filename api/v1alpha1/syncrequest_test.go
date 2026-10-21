package v1alpha1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSyncRequestIDsAscend: an identifier is its request's time to the
// nanosecond, moved past the Workspace's latest when the clock did not move
// or went back, so a later request's identifier always sorts after.
func TestSyncRequestIDsAscend(t *testing.T) {
	at := time.Date(2026, 10, 21, 16, 17, 2, 123456789, time.FixedZone("CEST", 2*3600))
	first := NextSyncRequestID(at, "")
	assert.Equal(t, "20261021T141702.123456789Z", first, "UTC, fixed width")
	same := NextSyncRequestID(at, first)
	assert.Equal(t, "20261021T141702.123456790Z", same)
	assert.Greater(t, NextSyncRequestID(at.Add(-time.Hour), same), same, "a clock gone back still ascends")
	assert.Equal(t, NextSyncRequestID(at.Add(time.Second), same), "20261021T141703.123456789Z")
}

// TestLatestSyncRequestID: the latest identifier is the pending ones', the
// running cycle's or the last cycle's, whichever is greatest.
func TestLatestSyncRequestID(t *testing.T) {
	ws := &Workspace{}
	assert.Empty(t, LatestSyncRequestID(ws))
	ws.Status.LastSync = &LastSync{Time: metav1.Now(), Requests: []string{"20261021T100000.000000000Z"}}
	assert.Equal(t, "20261021T100000.000000000Z", LatestSyncRequestID(ws), "the last cycle's, once nothing is pending")
	ws.Status.ActiveSync = &ActiveSync{Requests: []string{"20261021T110000.000000000Z"}}
	ws.Annotations = map[string]string{AnnotationSyncRequestIDs: "20261021T120000.000000000Z,20261021T130000.000000000Z"}
	assert.Equal(t, "20261021T130000.000000000Z", LatestSyncRequestID(ws))
	assert.Equal(t, []string{"20261021T130000.000000000Z"}, WithoutSyncRequests(SyncRequestIDs(ws), []string{"20261021T120000.000000000Z"}))
}
