package schedule

import (
	"time"

	"github.com/sacca97/ghg/internal/session"
)

type DueTask struct {
	ID       int
	Schedule string
	Prompt   string
	Slot     time.Time
}

// NextDue returns the first task whose next grid slot has passed.
func NextDue(tasks []session.Schedule, now time.Time) (*DueTask, bool) {
	for _, task := range tasks {
		parsed, err := Parse(task.Schedule)
		if err != nil {
			continue
		}
		var slot time.Time
		var due bool
		if task.LastFire.IsZero() {
			slot = task.Anchor
			due = !slot.Truncate(time.Second).After(now)
			if parsed.Every > 0 {
				if next, ok := parsed.NextAfter(task.Anchor, task.Anchor.Add(-time.Nanosecond)); ok {
					slot = next
				}
			}
		} else {
			slot, due = parsed.NextAfter(task.Anchor, task.LastFire)
			due = due && !slot.Truncate(time.Second).After(now)
		}
		if due {
			return &DueTask{ID: task.ID, Schedule: task.Schedule, Prompt: task.Prompt, Slot: slot}, true
		}
	}
	return nil, false
}
