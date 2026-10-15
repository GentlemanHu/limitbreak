package adapters

import (
	"quota-session-orchestrator/internal/domain"
	"time"
)

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneWindows(windows []domain.Window) []domain.Window {
	copy := append([]domain.Window(nil), windows...)
	for i := range copy {
		copy[i].ResetAt = cloneTime(copy[i].ResetAt)
		if copy[i].UsedPercent != nil {
			value := *copy[i].UsedPercent
			copy[i].UsedPercent = &value
		}
	}
	return copy
}
func cloneAccounts(accounts []domain.Account) []domain.Account {
	copy := append([]domain.Account{}, accounts...)
	for i := range copy {
		a := &copy[i]
		a.Models = append([]string(nil), a.Models...)
		a.Windows = cloneWindows(a.Windows)
		a.AvailableAt = cloneTime(a.AvailableAt)
		a.Metrics = append([]domain.QuotaMetric(nil), a.Metrics...)
		a.Groups = append([]domain.QuotaGroup(nil), a.Groups...)
		for n := range a.Groups {
			a.Groups[n].Models = append([]string(nil), a.Groups[n].Models...)
			a.Groups[n].Windows = cloneWindows(a.Groups[n].Windows)
			a.Groups[n].AvailableAt = cloneTime(a.Groups[n].AvailableAt)
		}
	}
	return copy
}
