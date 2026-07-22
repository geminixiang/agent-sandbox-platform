package gate

import "testing"

func TestClassifyContainment(t *testing.T) {
	for _, test := range []struct {
		name                string
		processGroupHandled bool
		alternativeHandled  bool
		want                string
	}{
		{name: "process group contains new session", processGroupHandled: true, want: StatusPassed},
		{name: "alternative mechanism contains new session", alternativeHandled: true, want: StatusPassed},
		{name: "no mechanism contains new session", want: StatusBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyContainment(test.processGroupHandled, test.alternativeHandled); got != test.want {
				t.Fatalf("ClassifyContainment() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	for _, test := range []struct {
		name   string
		checks []Check
		want   string
	}{
		{name: "all expected checks passed", checks: []Check{{Status: StatusPassed}, {Status: StatusPassed}}, want: StatusPassed},
		{name: "expected environment blocker is preserved", checks: []Check{{Status: StatusPassed}, {Status: StatusBlocked}}, want: StatusBlocked},
		{name: "unexpected failure wins over blocker", checks: []Check{{Status: StatusBlocked}, {Status: StatusFailed}}, want: StatusFailed},
		{name: "missing checks fail closed", checks: nil, want: StatusFailed},
		{name: "unknown status fails closed", checks: []Check{{Status: "skipped"}}, want: StatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Summarize(test.checks); got != test.want {
				t.Fatalf("Summarize() = %q, want %q", got, test.want)
			}
		})
	}
}
