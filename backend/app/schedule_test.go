package app

import (
	"errors"
	"testing"
	"time"

	"github.com/automagicops/haliphron/backend/run"
)

func TestACronExpressionFiresAtLocalTimeAcrossADaylightSavingChange(t *testing.T) {
	c, err := ParseCron("0 9 * * *", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}

	// Berlin leaves summer time on 2026-10-25: 09:00 is 07:00Z before and
	// 08:00Z after. A UTC-only schedule would drift by the hour.
	before := c.Next(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
	after := c.Next(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))

	if want := time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC); !before.Equal(want) {
		t.Errorf("before the change: next = %s, want %s", before, want)
	}
	if want := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC); !after.Equal(want) {
		t.Errorf("after the change: next = %s, want %s", after, want)
	}
	if after.Location() != time.UTC {
		t.Errorf("next is in %s; the fence compares UTC values", after.Location())
	}
}

func TestTheNextFireIsStrictlyAfterAndInWholeSeconds(t *testing.T) {
	c, err := ParseCron("*/15 * * * *", "")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)
	if next := c.Next(at); !next.Equal(at.Add(15 * time.Minute)) {
		t.Errorf("a fire at a slot computes the following one: got %s", next)
	}
	if next := c.Next(at.Add(1500 * time.Millisecond)); next.Nanosecond() != 0 {
		t.Errorf("next = %s carries sub-second precision", next)
	}
}

func TestACronExpressionIsRefusedWithTheFieldThatIsWrong(t *testing.T) {
	cases := []struct {
		expr, tz, field string
	}{
		{"", "UTC", "cron"},
		{"0 9 * *", "UTC", "cron"},
		{"61 * * * *", "UTC", "cron"},
		{"CRON_TZ=Europe/Berlin 0 9 * * *", "UTC", "cron"},
		{"TZ=UTC 0 9 * * *", "UTC", "cron"},
		{"@every 1s", "UTC", "cron"},
		{"0 9 * * *", "Mars/Olympus", "timezone"},
		{"0 9 * * *", "Local", "timezone"},
	}
	for _, tc := range cases {
		_, err := ParseCron(tc.expr, tc.tz)
		var invalid *run.InvalidRequestError
		if !errors.As(err, &invalid) {
			t.Errorf("%q in %q: err = %v, want an invalid request", tc.expr, tc.tz, err)
			continue
		}
		if invalid.Field != tc.field {
			t.Errorf("%q in %q: field = %q, want %q", tc.expr, tc.tz, invalid.Field, tc.field)
		}
	}
}

func TestDescriptorsAreAccepted(t *testing.T) {
	for _, expr := range []string{"@hourly", "@daily", "@weekly", "@monthly", "@yearly"} {
		if _, err := ParseCron(expr, "UTC"); err != nil {
			t.Errorf("%s: %v", expr, err)
		}
	}
}
