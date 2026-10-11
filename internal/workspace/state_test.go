package workspace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

func TestNextSync(t *testing.T) {
	// A Wednesday.
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	now := at(14, 3, 0)
	for name, c := range map[string]struct {
		sync v1alpha1.Sync
		want time.Time
	}{
		"nightly, at the hour itself":     {v1alpha1.Sync{Schedule: v1alpha1.ScheduleNightly}, at(15, 3, 0)},
		"empty is nightly":                {v1alpha1.Sync{}, at(15, 3, 0)},
		"weekly on Sunday":                {v1alpha1.Sync{Schedule: v1alpha1.ScheduleWeekly}, at(18, 3, 0)},
		"custom later today":              {v1alpha1.Sync{Schedule: v1alpha1.ScheduleCustom, Weekdays: []v1alpha1.Weekday{"wednesday"}, Hour: ptr.To[int32](22)}, at(14, 22, 0)},
		"custom, today's hour has passed": {v1alpha1.Sync{Schedule: v1alpha1.ScheduleCustom, Weekdays: []v1alpha1.Weekday{"wednesday"}, Hour: ptr.To[int32](1)}, at(21, 1, 0)},
		"custom, the first weekday":       {v1alpha1.Sync{Schedule: v1alpha1.ScheduleCustom, Weekdays: []v1alpha1.Weekday{"monday", "friday"}, Hour: ptr.To[int32](0)}, at(16, 0, 0)},
		"custom without weekdays":         {v1alpha1.Sync{Schedule: v1alpha1.ScheduleCustom, Hour: ptr.To[int32](0)}, time.Time{}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, NextSync(c.sync, now))
		})
	}
	assert.Equal(t, at(15, 3, 0), NextSync(v1alpha1.Sync{}, time.Date(2026, 10, 14, 5, 0, 0, 0, time.FixedZone("CEST", 2*3600))), "any zone is UTC")
}
