package daemon

import (
	"testing"
	"time"
	_ "time/tzdata" // as main has it
)

func TestCronNext(t *testing.T) {
	utc := func(s string) time.Time {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	for _, tc := range []struct {
		cron, tz, after string
		want            []string
	}{
		// Weekdays at 07:00 in Jakarta, from a Friday evening.
		{"0 7 * * 1-5", "Asia/Jakarta", "2026-10-23T12:00:00Z", []string{"2026-10-26T00:00:00Z", "2026-10-27T00:00:00Z"}},
		{"@daily", "UTC", "2026-10-22T10:00:00Z", []string{"2026-10-23T00:00:00Z"}},
		// Day of month or day of week, as cron reads both written.
		{"0 0 1 * 1", "UTC", "2026-10-30T00:00:00Z", []string{"2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"}},
		// New York skips 02:00-03:00 on 2026-03-08: a fixed job in the gap
		// runs right after the jump; a wildcard hour does not make it up.
		{"30 2 * * *", "America/New_York", "2026-03-08T06:00:00Z", []string{"2026-03-08T07:00:00Z", "2026-03-09T06:30:00Z"}},
		{"30 * * * *", "America/New_York", "2026-03-08T06:00:00Z", []string{"2026-03-08T06:30:00Z", "2026-03-08T07:30:00Z"}},
		// New York repeats 01:00-02:00 on 2026-11-01: a fixed job runs once,
		// a wildcard one each time the clock shows its minute.
		{"30 1 * * *", "America/New_York", "2026-11-01T04:00:00Z", []string{"2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}},
		{"*/30 * * * *", "America/New_York", "2026-11-01T04:50:00Z", []string{"2026-11-01T05:00:00Z", "2026-11-01T05:30:00Z", "2026-11-01T06:00:00Z", "2026-11-01T06:30:00Z", "2026-11-01T07:00:00Z"}},
	} {
		c, err := parseCron(tc.cron, tc.tz)
		if err != nil {
			t.Fatal(err)
		}
		at := utc(tc.after)
		for _, w := range tc.want {
			at = c.next(at)
			if !at.Equal(utc(w)) {
				t.Fatalf("%s in %s: next %s, want %s", tc.cron, tc.tz, at.UTC().Format(time.RFC3339), w)
			}
		}
	}
	never, err := parseCron("0 0 30 2 *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if at := never.next(utc("2026-01-01T00:00:00Z")); !at.IsZero() {
		t.Fatalf("February 30 came at %s", at)
	}
}
