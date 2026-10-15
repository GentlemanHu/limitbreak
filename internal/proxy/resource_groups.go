package proxy

import (
	"strconv"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func materializeGroups(a *domain.Account, live evidence, now time.Time) {
	a.QuotaSource = live.source
	if a.QuotaSource == "" && len(live.windows) > 0 {
		a.QuotaSource = "api-call"
	}
	if live.failed {
		a.QuotaError = live.reason
	}
	groups := append([]domain.QuotaGroup(nil), live.groups...)
	if len(groups) == 0 && len(live.windows) > 0 {
		groups = []domain.QuotaGroup{{ID: "account", Name: "共享额度", Scope: "account", Windows: cloneWindows(live.windows), CheckedAt: live.checked}}
	}
	a.Groups = []domain.QuotaGroup{}
	seen := map[string]int{}
	for _, g := range groups {
		key := g.ID
		if key == "" {
			key = g.Name
		}
		seen[key]++
		if seen[key] > 1 {
			key += "/" + strconv.Itoa(seen[key])
		}
		g.ID = domain.ResourceID(a.ID, key)
		g.Windows = cloneWindows(g.Windows)
		seenWindows := map[string]int{}
		for i := range g.Windows {
			w := &g.Windows[i]
			key := w.ID
			if key == "" {
				key = w.Name
			}
			seenWindows[key]++
			if seenWindows[key] > 1 {
				key += "/" + strconv.Itoa(seenWindows[key])
			}
			w.ID = domain.ResourceID(g.ID, key)
			if w.Name == "quota:primary_window" {
				w.Name = "短周期额度"
				if w.LimitSeconds == 18000 {
					w.Name = "5 小时额度"
				}
			}
			if w.Name == "quota:secondary_window" {
				w.Name = "长周期额度"
				if w.LimitSeconds == 604800 {
					w.Name = "7 天额度"
				}
			}
		}
		g.State, g.AvailableAt = domain.WindowState(g.Windows)
		if live.failed {
			g.State = "unknown"
		}
		a.Groups = append(a.Groups, g)
	}
	for i := range a.Windows {
		a.Windows[i].ID = domain.ResourceID(a.ID, a.Windows[i].Name, strconv.Itoa(i))
	}
	a.Metrics = []domain.QuotaMetric{}
	metricIndex := map[string]int{}
	for _, m := range live.metrics {
		if m.Key == "" {
			m.Key = m.Label
		}
		if m.Label == "" {
			m.Label = m.Key
		}
		if m.Key == "" {
			continue
		}
		if index, ok := metricIndex[m.Key]; ok {
			a.Metrics[index] = m
		} else {
			metricIndex[m.Key] = len(a.Metrics)
			a.Metrics = append(a.Metrics, m)
		}
	}
}
