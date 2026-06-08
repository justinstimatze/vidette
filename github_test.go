package main

import "testing"

func TestFailureStreakStart(t *testing.T) {
	// Helper: a run of a given workflow/conclusion at a given timestamp.
	run := func(name, concl, at string) CIRun {
		return CIRun{Name: name, Conclusion: concl, CreatedAt: at, Status: "completed"}
	}

	tests := []struct {
		name      string
		runs      []CIRun
		wantStart string
		wantCap   bool
	}{
		{
			name:      "empty",
			runs:      nil,
			wantStart: "",
			wantCap:   false,
		},
		{
			name:      "latest green",
			runs:      []CIRun{run("Tests", "success", "2026-06-08T00:00:00Z")},
			wantStart: "2026-06-08T00:00:00Z",
			wantCap:   false,
		},
		{
			name:      "latest in progress",
			runs:      []CIRun{{Name: "Tests", Conclusion: "", Status: "in_progress", CreatedAt: "2026-06-08T00:00:00Z"}},
			wantStart: "2026-06-08T00:00:00Z",
			wantCap:   false,
		},
		{
			// The real gemot bug: a failing link-checker interleaved with a
			// passing Tests on the same pushes. Naively stopping at the first
			// "success" would pick the same-day Tests run and report ~0 days.
			// The streak must follow only the failing workflow back to its last
			// green, ignoring other workflows' successes.
			name: "interleaved workflows — streak follows failing one only",
			runs: []CIRun{
				run("Markdown Links", "failure", "2026-06-08T00:00:00Z"),
				run("Tests", "success", "2026-06-08T00:00:00Z"),
				run("Markdown Links", "failure", "2026-05-01T00:00:00Z"),
				run("Tests", "success", "2026-05-01T00:00:00Z"),
				run("Markdown Links", "success", "2026-04-30T00:00:00Z"),
				run("Tests", "success", "2026-04-30T00:00:00Z"),
			},
			wantStart: "2026-05-01T00:00:00Z", // oldest consecutive Markdown Links failure
			wantCap:   false,
		},
		{
			// No earlier success of the failing workflow anywhere in the window:
			// streak is a lower bound (capped) anchored at the oldest failure.
			name: "no prior success in window — capped",
			runs: []CIRun{
				run("Markdown Links", "failure", "2026-06-08T00:00:00Z"),
				run("Markdown Links", "failure", "2026-05-01T00:00:00Z"),
				run("Markdown Links", "failure", "2026-04-30T00:00:00Z"),
			},
			wantStart: "2026-04-30T00:00:00Z",
			wantCap:   true,
		},
		{
			name: "action_required is treated as failing",
			runs: []CIRun{
				run("Deploy", "action_required", "2026-06-08T00:00:00Z"),
				run("Deploy", "success", "2026-06-01T00:00:00Z"),
			},
			wantStart: "2026-06-08T00:00:00Z",
			wantCap:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStart, gotCap := failureStreakStart(tt.runs)
			if gotStart != tt.wantStart || gotCap != tt.wantCap {
				t.Errorf("failureStreakStart() = (%q, %v), want (%q, %v)",
					gotStart, gotCap, tt.wantStart, tt.wantCap)
			}
		})
	}
}
