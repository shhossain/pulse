package app

import (
	"testing"
	"time"

	"pulse/internal/model"
)

func TestNotificationsCappedPerJob(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s := &SwitchNotifier{now: func() time.Time { return now }}
	for i := 0; i < NotifyPerHour; i++ {
		if show, notice := s.allow(model.Notification{Job: "spammy"}); !show || notice {
			t.Fatalf("notification %d: show=%v notice=%v", i, show, notice)
		}
	}
	if show, notice := s.allow(model.Notification{Job: "spammy"}); !show || !notice {
		t.Fatalf("first over the cap should be the notice: show=%v notice=%v", show, notice)
	}
	if show, _ := s.allow(model.Notification{Job: "spammy"}); show {
		t.Fatal("past the notice, notifications should be held")
	}
	if show, notice := s.allow(model.Notification{Job: "spammy", AskID: "q_1"}); !show || notice {
		t.Fatal("a question was held: the job would wait on it unseen")
	}
	if show, notice := s.allow(model.Notification{Job: "other"}); !show || notice {
		t.Fatal("another job is affected")
	}
	if show, _ := s.allow(model.Notification{}); !show {
		t.Fatal("notifications without a job are capped")
	}
	now = now.Add(61 * time.Minute)
	if show, notice := s.allow(model.Notification{Job: "spammy"}); !show || notice {
		t.Fatalf("an hour later: show=%v notice=%v", show, notice)
	}
}
