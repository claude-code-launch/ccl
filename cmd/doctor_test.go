package cmd

import "testing"

func TestModelVerificationSummary(t *testing.T) {
	tests := []struct {
		name        string
		available   []string
		unavailable []string
		want        string
	}{
		{name: "mixed", available: []string{"a", "b"}, unavailable: []string{"c"}, want: "2 available · 1 unavailable"},
		{name: "all available", available: []string{"a"}, want: "1 available · 0 unavailable"},
		{name: "all unavailable", unavailable: []string{"a", "b"}, want: "0 available · 2 unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelVerificationSummary(tt.available, tt.unavailable); got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}
