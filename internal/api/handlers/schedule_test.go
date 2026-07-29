package handlers

import (
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"
)

func TestCalcNextRunAtInterval(t *testing.T) {
	from := time.Date(2026, 7, 29, 12, 0, 30, 0, time.UTC)

	cases := []struct {
		name    string
		seconds int
		want    time.Time
	}{
		{"15 minutes", 900, from.Add(15 * time.Minute)},
		{"1 hour", 3600, from.Add(time.Hour)},
		{"below floor clamps to 60s", 30, from.Add(60 * time.Second)},
		{"zero clamps to 60s", 0, from.Add(60 * time.Second)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := models.ScheduledTrigger{Frequency: "interval", IntervalSeconds: tc.seconds}
			got := calcNextRunAt(s, from)
			if !got.Equal(tc.want) {
				t.Fatalf("interval=%ds: got %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestCalcNextRunAtHourlyUnchanged(t *testing.T) {
	from := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	s := models.ScheduledTrigger{Frequency: "hourly"}
	want := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	if got := calcNextRunAt(s, from); !got.Equal(want) {
		t.Fatalf("hourly: got %v, want %v", got, want)
	}
}
