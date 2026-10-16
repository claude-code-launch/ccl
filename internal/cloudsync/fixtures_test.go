package cloudsync

import (
	"strings"
)

// LoginICloudLocalKey logs in to the default iCloud remote, as
// `ccl cloud login icloud` does.
func LoginICloudLocalKey() (LoginResult, error) {
	return LoginICloudNamed("icloud")
}

func (m *Manager) Push(force bool) (PushResult, error) {
	prepared, err := m.preparePush()
	if err != nil {
		return PushResult{}, err
	}
	plan, err := m.planPush(prepared, force)
	if err != nil {
		return PushResult{}, err
	}
	result, err := plan.commit(prepared, true)
	if err != nil {
		return PushResult{}, err
	}
	return result, nil
}

func HasDiagnosticErrors(report DiagnosticReport) bool {
	for _, check := range report.Checks {
		if strings.EqualFold(check.Level, "error") {
			return true
		}
	}
	return false
}

func IsNotConfiguredDiagnostic(report DiagnosticReport) bool {
	return !report.Configured && len(report.Checks) == 1 &&
		report.Checks[0].Level == "info" &&
		strings.Contains(report.Checks[0].Message, "not configured")
}
