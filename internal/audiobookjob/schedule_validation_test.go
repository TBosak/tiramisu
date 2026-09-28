package audiobookjob_test

// Requirement coverage (A5/I3, review-2.md): audiobookjob.ValidateSchedule
// is now a declared boundary (a stub returning nil unconditionally). These
// tests exercise it directly: days outside 0-6, hour outside 0-23, minute
// outside 0-59, duplicate days, an enabled schedule with zero days, and the
// valid edge values (0 and the top of each range), plus disabled inertness
// (a wildly invalid schedule that is not enabled must still validate).
//
// ValidateSchedule always returning nil today makes every invalid-case
// assertion below a genuine RED anchor; the valid-case and disabled-case
// assertions currently pass (a stub that never rejects anything trivially
// accepts valid input too), and are kept as locked regression coverage so a
// future implementation cannot start rejecting well-formed schedules either.

import (
	"testing"

	"tiramisu/internal/audiobookjob"
	"tiramisu/internal/config"
)

func TestValidateSchedule_InvalidBoundaries_Rejected(t *testing.T) {
	cases := []struct {
		name     string
		schedule config.DailyJobConfig
	}{
		{"day below 0", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{-1}, Hour: 0, Minute: 0}},
		{"day above 6", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{7}, Hour: 0, Minute: 0}},
		{"one of several days out of range", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{2, 9}, Hour: 0, Minute: 0}},
		{"duplicate days", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{1, 1}, Hour: 0, Minute: 0}},
		{"hour below 0", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: -1, Minute: 0}},
		{"hour above 23", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 24, Minute: 0}},
		{"minute below 0", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 0, Minute: -1}},
		{"minute above 59", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 0, Minute: 60}},
		{"enabled with zero days", config.DailyJobConfig{Enabled: true, DaysOfWeek: nil, Hour: 0, Minute: 0}},
		{"enabled with empty (non-nil) days", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{}, Hour: 0, Minute: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := audiobookjob.ValidateSchedule(tc.schedule); err == nil {
				t.Fatalf("ValidateSchedule(%+v) = nil error, want a validation failure", tc.schedule)
			}
		})
	}
}

func TestValidateSchedule_ValidEdgeValues_Accepted(t *testing.T) {
	cases := []struct {
		name     string
		schedule config.DailyJobConfig
	}{
		{"day at lower edge (Sunday=0)", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0}, Hour: 12, Minute: 0}},
		{"day at upper edge (Saturday=6)", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{6}, Hour: 12, Minute: 0}},
		{"hour at lower edge (0)", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{1}, Hour: 0, Minute: 30}},
		{"hour at upper edge (23)", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{1}, Hour: 23, Minute: 30}},
		{"minute at lower edge (0)", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{1}, Hour: 6, Minute: 0}},
		{"minute at upper edge (59)", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{1}, Hour: 6, Minute: 59}},
		{"multiple distinct valid days", config.DailyJobConfig{Enabled: true, DaysOfWeek: []int{0, 3, 6}, Hour: 6, Minute: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := audiobookjob.ValidateSchedule(tc.schedule); err != nil {
				t.Fatalf("ValidateSchedule(%+v) = %v, want nil error", tc.schedule, err)
			}
		})
	}
}

// I3: "Disabled configuration is inert" - a schedule with Enabled == false
// must validate regardless of how nonsensical its other fields are, since
// none of them can ever fire.
func TestValidateSchedule_Disabled_IsInertRegardlessOfOtherFields(t *testing.T) {
	cases := []config.DailyJobConfig{
		{Enabled: false, DaysOfWeek: []int{-5, 99}, Hour: -100, Minute: 9999},
		{Enabled: false, DaysOfWeek: nil, Hour: 0, Minute: 0},
		{Enabled: false},
	}
	for _, schedule := range cases {
		if err := audiobookjob.ValidateSchedule(schedule); err != nil {
			t.Fatalf("ValidateSchedule(disabled, %+v) = %v, want nil error (disabled configuration must be inert)", schedule, err)
		}
	}
}
