package v1alpha1

import (
	"slices"
	"strings"
	"time"
)

// syncRequestIDLayout is a sync request identifier's form: its UTC time to
// the nanosecond, fixed width, so that identifiers sort as they were made.
const syncRequestIDLayout = "20060102T150405.000000000Z"

// SyncRequestIDs are the identifiers of a Workspace's sync requests no cycle
// has served yet, ascending.
func SyncRequestIDs(ws *Workspace) []string {
	raw := ws.Annotations[AnnotationSyncRequestIDs]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// LatestSyncRequestID is the Workspace's latest sync request identifier,
// pending, served by the running cycle or by the last one; empty for none.
func LatestSyncRequestID(ws *Workspace) string {
	var ids []string
	ids = append(ids, SyncRequestIDs(ws)...)
	if a := ws.Status.ActiveSync; a != nil {
		ids = append(ids, a.Requests...)
	}
	if l := ws.Status.LastSync; l != nil {
		ids = append(ids, l.Requests...)
	}
	if len(ids) == 0 {
		return ""
	}
	return slices.Max(ids)
}

// NextSyncRequestID is the identifier of a sync request made at: its time,
// moved past after (the Workspace's latest identifier) when the clock did
// not, so that a Workspace's identifiers ascend in the order of their
// requests.
func NextSyncRequestID(at time.Time, after string) string {
	id := at.UTC().Format(syncRequestIDLayout)
	if after == "" || id > after {
		return id
	}
	prev, err := time.Parse(syncRequestIDLayout, after)
	if err != nil {
		return id
	}
	return prev.Add(time.Nanosecond).Format(syncRequestIDLayout)
}

// WithoutSyncRequests are the pending identifiers a cycle did not serve.
func WithoutSyncRequests(pending, served []string) []string {
	var out []string
	for _, id := range pending {
		if !slices.Contains(served, id) {
			out = append(out, id)
		}
	}
	return out
}
