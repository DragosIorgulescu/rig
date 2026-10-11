package cronschedule

import (
	"testing"
	"time"

	"github.com/BaronBonet/rig/internal/core"

	"github.com/stretchr/testify/require"
)

// inZone is a calculator for a machine whose local time zone is zone.
func inZone(zone *time.Location) core.ScheduleCalculator {
	return calculator{location: zone}
}

func TestValidate_AcceptsStandardExpressionsAndDescriptors(t *testing.T) {
	for _, expr := range []string{
		"*/5 * * * *",
		"0 9 * * 1-5",
		"30 8,17 * * *",
		"0 0 1 * *",
		"@hourly",
		"@daily",
		"@weekly",
		"@monthly",
		"@every 2h",
		"@every 90m",
		"  0 9 * * *  ",
	} {
		t.Run(expr, func(t *testing.T) {
			require.NoError(t, New().Validate(expr))
		})
	}
}

func TestValidate_RejectsWhatIsNotAScheduleThatFires(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		want string
	}{
		"empty":               {"", "not a cron schedule"},
		"four fields":         {"* * * *", "not a cron schedule"},
		"six fields":          {"0 0 9 * * *", "not a cron schedule"},
		"minute out of range": {"61 * * * *", "not a cron schedule"},
		"unknown descriptor":  {"@sometimes", "not a cron schedule"},
		"every without time":  {"@every", "not a cron schedule"},
		"words":               {"every day at nine", "not a cron schedule"},
		"never fires":         {"0 0 30 2 *", "never fires"},
		"cron_tz prefix":      {"CRON_TZ=Asia/Tokyo 0 9 * * *", "local time"},
		"tz prefix":           {"TZ=UTC 0 9 * * *", "local time"},
		"lower case tz":       {"tz=UTC 0 9 * * *", "local time"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, New().Validate(tc.expr), tc.want)
			_, err := New().Next(tc.expr, time.Now())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNext_IsTheFirstFireStrictlyAfterTheGivenTime(t *testing.T) {
	calc := inZone(time.UTC)
	at := func(day int, hour int, minute int) time.Time {
		return time.Date(2026, time.October, day, hour, minute, 0, 0, time.UTC)
	}

	for name, tc := range map[string]struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		"later today":               {"0 9 * * *", at(11, 7, 30), at(11, 9, 0)},
		"tomorrow once past":        {"0 9 * * *", at(11, 9, 30), at(12, 9, 0)},
		"not the time itself":       {"0 9 * * *", at(11, 9, 0), at(12, 9, 0)},
		"every five minutes":        {"*/5 * * * *", at(11, 9, 2), at(11, 9, 5)},
		"weekdays skip the weekend": {"0 9 * * 1-5", at(10, 10, 0), at(12, 9, 0)}, // Sat 10th, Mon 12th
		"daily descriptor":          {"@daily", at(11, 9, 30), at(12, 0, 0)},
		"hourly descriptor":         {"@hourly", at(11, 9, 30), at(11, 10, 0)},
		"every two hours":           {"@every 2h", at(11, 9, 30), at(11, 11, 30)},
		"first of the month":        {"0 0 1 * *", at(11, 9, 30), time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(name, func(t *testing.T) {
			next, err := calc.Next(tc.expr, tc.after)

			require.NoError(t, err)
			require.True(t, tc.want.Equal(next), "want %s, got %s", tc.want, next)
		})
	}
}

func TestNext_RunsInLocalTime(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)
	calc := inZone(tokyo)

	// 00:30 UTC is 09:30 in Tokyo, so 9am has passed there for the day.
	after := time.Date(2026, time.October, 11, 0, 30, 0, 0, time.UTC)
	next, err := calc.Next("0 9 * * *", after)

	require.NoError(t, err)
	require.True(t, time.Date(2026, time.October, 12, 9, 0, 0, 0, tokyo).Equal(next), "got %s", next)
	require.Equal(t, tokyo, next.Location())
}

func TestNext_CountsEveryRunOfAShortSchedule(t *testing.T) {
	calc := inZone(time.UTC)

	at := time.Date(2026, time.October, 11, 9, 0, 0, 0, time.UTC)
	var gaps []time.Duration
	for range 4 {
		next, err := calc.Next("*/10 * * * *", at)
		require.NoError(t, err)
		gaps = append(gaps, next.Sub(at))
		at = next
	}

	require.Equal(t, []time.Duration{10 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}, gaps)
}

func TestNew_RunsInTheMachinesLocalTime(t *testing.T) {
	next, err := New().Next("0 9 * * *", time.Now())

	require.NoError(t, err)
	require.Equal(t, 9, next.Hour(), "9am on the machine's own clock")
	require.Zero(t, next.Minute())
	require.Equal(t, time.Now().Location().String(), next.Location().String())
}
