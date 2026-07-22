package gate

const (
	StatusPassed  = "passed"
	StatusBlocked = "blocked"
	StatusFailed  = "failed"
)

type Check struct {
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Detail map[string]any `json:"detail,omitempty"`
}

type Report struct {
	SchemaVersion int            `json:"schemaVersion"`
	Test          string         `json:"test"`
	Status        string         `json:"status"`
	Checks        []Check        `json:"checks"`
	Containment   map[string]any `json:"containment"`
	Environment   map[string]any `json:"environment"`
}

// ClassifyContainment distinguishes a compatibility blocker from a core gate
// failure. A mechanism counts only when the gate observed that it contained the
// new-session descendant; merely exposing cgroup controls is not sufficient.
func ClassifyContainment(processGroupContained, alternativeContained bool) string {
	if processGroupContained || alternativeContained {
		return StatusPassed
	}
	return StatusBlocked
}

func Summarize(checks []Check) string {
	if len(checks) == 0 {
		return StatusFailed
	}
	status := StatusPassed
	for _, check := range checks {
		switch check.Status {
		case StatusPassed:
		case StatusBlocked:
			status = StatusBlocked
		case StatusFailed:
			return StatusFailed
		default:
			return StatusFailed
		}
	}
	return status
}
