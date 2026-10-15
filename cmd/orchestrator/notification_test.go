package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQuotaNotificationNormalizationKeepsOnlyEnvelope(t *testing.T) {
	for _, kind := range []string{"quota_auto_resume_fired", "quota_auto_resume_stale", "quota_auto_resume_disabled"} {
		raw := `{"session_id":"session","hook_event_name":"Notification","notification_type":"` + kind + `","message":"PRIVATE NOTIFICATION","prompt":"PRIVATE PROMPT","last_assistant_message":"PRIVATE ANSWER"}`
		e, err := normalizeHook([]byte(raw), time.Now())
		if err != nil || e.NotificationType != kind || e.Kind != "code" {
			t.Fatalf("notification: %+v %v", e, err)
		}
		data, _ := json.Marshal(e)
		if strings.Contains(string(data), "PRIVATE") {
			t.Fatal("notification retained private content")
		}
	}
	if _, err := normalizeHook([]byte(`{"session_id":"session","hook_event_name":"Notification","notification_type":"permission_prompt"}`), time.Now()); err == nil {
		t.Fatal("unsupported notification accepted")
	}
}

func TestQuotaNotificationVersionBoundary(t *testing.T) {
	if versionAtLeast("2.1.233", 2, 1, 234) || !versionAtLeast("2.1.234", 2, 1, 234) {
		t.Fatal("notification version boundary changed")
	}
}
