package dates

import (
	"testing"
	"time"
)

type testCase struct{ name, text, start, end, gran string }

func fixedNow() time.Time { return parseTime("2026-11-06T14:30:00+08:00") }
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
func cases(t *testing.T, now time.Time, rows []testCase) {
	t.Helper()
	loc := zone(t, "Asia/Shanghai")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) { check(t, row, now, loc) })
	}
}
func check(t *testing.T, row testCase, now time.Time, loc *time.Location) {
	t.Helper()
	got, ok := Resolve(row.text, now, loc)
	if !ok || !got.Start.Equal(parseTime(row.start)) || !got.End.Equal(parseTime(row.end)) || got.Granularity != row.gran {
		t.Fatalf("Resolve(%q): got %+v, ok=%v; want [%s,%s) %s", row.text, got, ok, row.start, row.end, row.gran)
	}
	if got.Start.Location() != time.UTC || got.End.Location() != time.UTC || got.Label == "" {
		t.Fatalf("must return UTC and a log label: %+v", got)
	}
}

func TestResolveRelativeDays(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"today_midnight_to_midnight", "今天", "2026-11-05T16:00:00Z", "2026-11-06T16:00:00Z", "day"},
		{"yesterday_in_recall_question", "我昨天跟你说了什么？", "2026-11-04T16:00:00Z", "2026-11-05T16:00:00Z", "day"},
		{"two_days_back", "前天", "2026-11-03T16:00:00Z", "2026-11-04T16:00:00Z", "day"},
		{"three_days_not_two", "大前天", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"tomorrow_resolves_even_for_recall", "明天", "2026-11-06T16:00:00Z", "2026-11-07T16:00:00Z", "day"},
		{"english_yesterday", "what did we say yesterday?", "2026-11-04T16:00:00Z", "2026-11-05T16:00:00Z", "day"},
	})
}

func TestResolveWeekdaysUseMondayWeekStart(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"friday_last_wednesday_is_previous_calendar_week", "我上周三跟你说了什么", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"},
		{"xingqi_alias", "上星期三", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"},
		{"libai_alias", "上礼拜三", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"},
		{"this_wednesday_after_wednesday", "这周三", "2026-11-03T16:00:00Z", "2026-11-04T16:00:00Z", "day"},
		{"ben_alias", "本周三", "2026-11-03T16:00:00Z", "2026-11-04T16:00:00Z", "day"},
		{"bare_wednesday_most_recent_prior_day", "周三", "2026-11-03T16:00:00Z", "2026-11-04T16:00:00Z", "day"},
		{"english_last_wednesday", "last Wednesday", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"},
		{"sunday_is_week_day_seven", "这周日", "2026-11-07T16:00:00Z", "2026-11-08T16:00:00Z", "day"},
	})
	cases(t, parseTime("2026-11-02T14:30:00+08:00"), []testCase{
		{"monday_last_wednesday_previous_week", "上周三", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"},
		{"this_wednesday_before_wednesday_is_future", "这周三", "2026-11-03T16:00:00Z", "2026-11-04T16:00:00Z", "day"},
	})
	cases(t, parseTime("2026-11-08T14:30:00+08:00"), []testCase{{"sunday_still_same_monday_week", "上周三", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"}})
	cases(t, parseTime("2026-11-04T23:30:00+08:00"), []testCase{{"bare_weekday_excludes_today_even_late", "周三", "2026-10-27T16:00:00Z", "2026-10-28T16:00:00Z", "day"}})
}

func TestResolveWeeksAndWeekends(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"last_week_monday_to_monday", "上周", "2026-10-25T16:00:00Z", "2026-11-01T16:00:00Z", "week"},
		{"last_xingqi_alias", "上个星期", "2026-10-25T16:00:00Z", "2026-11-01T16:00:00Z", "week"},
		{"two_weeks_back", "上上周", "2026-10-18T16:00:00Z", "2026-10-25T16:00:00Z", "week"},
		{"last_weekend_saturday_sunday", "上周末", "2026-10-30T16:00:00Z", "2026-11-01T16:00:00Z", "range"},
		{"bare_weekend_completed_only", "周末", "2026-10-30T16:00:00Z", "2026-11-01T16:00:00Z", "range"},
		{"this_week_clips_at_now", "这周", "2026-11-01T16:00:00Z", "2026-11-06T06:30:00Z", "week"},
		{"ben_week_clips_at_now", "本周", "2026-11-01T16:00:00Z", "2026-11-06T06:30:00Z", "week"},
		{"english_week", "last week", "2026-10-25T16:00:00Z", "2026-11-01T16:00:00Z", "week"},
	})
	cases(t, parseTime("2026-11-08T14:30:00+08:00"), []testCase{{"ongoing_weekend_not_completed", "周末", "2026-10-30T16:00:00Z", "2026-11-01T16:00:00Z", "range"}})
}

func TestResolveMonthsAndDayOfMonthWithChineseNumerals(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"last_month", "上个月", "2026-09-30T16:00:00Z", "2026-10-31T16:00:00Z", "month"},
		{"last_month_short", "上月", "2026-09-30T16:00:00Z", "2026-10-31T16:00:00Z", "month"},
		{"this_month", "这个月", "2026-10-31T16:00:00Z", "2026-11-30T16:00:00Z", "month"},
		{"ben_month", "本月", "2026-10-31T16:00:00Z", "2026-11-30T16:00:00Z", "month"},
		{"month_beginning_three_days", "月初", "2026-10-31T16:00:00Z", "2026-11-03T16:00:00Z", "range"},
		{"month_end_three_days", "月底", "2026-11-27T16:00:00Z", "2026-11-30T16:00:00Z", "range"},
		{"bare_arabic_day", "3号", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"bare_chinese_day", "三号", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"bare_chinese_ri", "三日", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"last_month_fifteenth", "上个月15号", "2026-10-14T16:00:00Z", "2026-10-15T16:00:00Z", "day"},
		{"month_arabic_ri", "11月3日", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"month_arabic_hao", "11月3号", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"chinese_eleven_and_three", "十一月三号", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"chinese_ten", "十号", "2026-11-09T16:00:00Z", "2026-11-10T16:00:00Z", "day"},
		{"chinese_eleven", "十一号", "2026-11-10T16:00:00Z", "2026-11-11T16:00:00Z", "day"},
		{"chinese_twenty", "二十号", "2026-11-19T16:00:00Z", "2026-11-20T16:00:00Z", "day"},
		{"chinese_thirty_one", "十月三十一号", "2026-10-30T16:00:00Z", "2026-10-31T16:00:00Z", "day"},
	})
}

func TestResolvePartsOfDay(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"early_morning_zero_six", "凌晨", "2026-11-05T16:00:00Z", "2026-11-05T22:00:00Z", "part_of_day"},
		{"morning_six_twelve", "早上", "2026-11-05T22:00:00Z", "2026-11-06T04:00:00Z", "part_of_day"},
		{"zaochen_alias", "早晨", "2026-11-05T22:00:00Z", "2026-11-06T04:00:00Z", "part_of_day"},
		{"shangwu_alias", "上午", "2026-11-05T22:00:00Z", "2026-11-06T04:00:00Z", "part_of_day"},
		{"noon_overlaps_morning_and_afternoon", "中午", "2026-11-06T03:00:00Z", "2026-11-06T06:00:00Z", "part_of_day"},
		{"afternoon_twelve_eighteen", "下午", "2026-11-06T04:00:00Z", "2026-11-06T10:00:00Z", "part_of_day"},
		{"evening_ends_next_local_midnight", "晚上", "2026-11-06T10:00:00Z", "2026-11-06T16:00:00Z", "part_of_day"},
		{"last_night", "昨晚", "2026-11-05T10:00:00Z", "2026-11-05T16:00:00Z", "part_of_day"},
		{"yesterday_evening", "昨天晚上", "2026-11-05T10:00:00Z", "2026-11-05T16:00:00Z", "part_of_day"},
		{"this_morning_short", "今早", "2026-11-05T22:00:00Z", "2026-11-06T04:00:00Z", "part_of_day"},
		{"today_afternoon_not_clipped", "今天下午", "2026-11-06T04:00:00Z", "2026-11-06T10:00:00Z", "part_of_day"},
		{"english_this_morning", "this morning", "2026-11-05T22:00:00Z", "2026-11-06T04:00:00Z", "part_of_day"},
	})
}

func TestResolveAgoExpressions(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"arabic_days_ago", "3天前", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"chinese_three_days", "三天前", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"chinese_ten_days", "十天前", "2026-10-26T16:00:00Z", "2026-10-27T16:00:00Z", "day"},
		{"english_days_ago", "3 days ago", "2026-11-02T16:00:00Z", "2026-11-03T16:00:00Z", "day"},
		{"english_singular_day", "1 day ago", "2026-11-04T16:00:00Z", "2026-11-05T16:00:00Z", "day"},
		{"hours_trailing_window", "两个小时前", "2026-11-06T04:30:00Z", "2026-11-06T06:30:00Z", "range"},
		{"weeks_whole_monday_week", "两周前", "2026-10-18T16:00:00Z", "2026-10-25T16:00:00Z", "week"},
		{"months_corresponding_day", "三个月前", "2026-08-05T16:00:00Z", "2026-08-06T16:00:00Z", "day"},
	})
	cases(t, parseTime("2026-03-31T14:30:00+08:00"), []testCase{{"months_clamp_not_overflow_to_march", "一个月前", "2026-02-27T16:00:00Z", "2026-02-28T16:00:00Z", "day"}})
}

func TestResolveRecentExpressions(t *testing.T) {
	cases(t, fixedNow(), []testCase{
		{"past_few_days_includes_today", "前几天", "2026-11-03T16:00:00Z", "2026-11-06T16:00:00Z", "range"},
		{"recent_few_days_three_local_days", "最近几天", "2026-11-03T16:00:00Z", "2026-11-06T16:00:00Z", "range"},
		{"recent_seven_local_days", "最近", "2026-10-30T16:00:00Z", "2026-11-06T16:00:00Z", "range"},
		{"just_now_thirty_minutes", "刚才", "2026-11-06T06:00:00Z", "2026-11-06T06:30:00Z", "range"},
		{"just_now_alias", "刚刚", "2026-11-06T06:00:00Z", "2026-11-06T06:30:00Z", "range"},
	})
}

func TestResolveAcrossYearBoundary(t *testing.T) {
	cases(t, parseTime("2027-01-02T14:30:00+08:00"), []testCase{
		{"last_month_day_moves_to_december", "上个月15号", "2026-12-14T16:00:00Z", "2026-12-15T16:00:00Z", "day"},
		{"last_week_crosses_year", "上周", "2026-12-20T16:00:00Z", "2026-12-27T16:00:00Z", "week"},
		{"this_week_crosses_year", "这周", "2026-12-27T16:00:00Z", "2027-01-02T06:30:00Z", "week"},
		{"last_year_whole", "去年", "2025-12-31T16:00:00Z", "2026-12-31T16:00:00Z", "range"},
		{"this_year_whole", "今年", "2026-12-31T16:00:00Z", "2027-12-31T16:00:00Z", "range"},
		{"two_days_ago_prior_year", "前天", "2026-12-30T16:00:00Z", "2026-12-31T16:00:00Z", "day"},
	})
}

func TestResolveLeapDay(t *testing.T) {
	cases(t, parseTime("2024-03-01T14:30:00+08:00"), []testCase{
		{"explicit_leap_day", "2月29日", "2024-02-28T16:00:00Z", "2024-02-29T16:00:00Z", "day"},
		{"yesterday_is_leap_day", "昨天", "2024-02-28T16:00:00Z", "2024-02-29T16:00:00Z", "day"},
		{"last_month_has_29_days", "上个月", "2024-01-31T16:00:00Z", "2024-02-29T16:00:00Z", "month"},
	})
}

func TestResolveDSTZoneUsesLocalMidnight(t *testing.T) {
	loc := zone(t, "America/New_York")
	for _, row := range []struct{ name, now, start, end string }{
		{"spring_forward_23_hour_day", "2024-03-11T12:00:00-04:00", "2024-03-10T05:00:00Z", "2024-03-11T04:00:00Z"},
		{"fall_back_25_hour_day", "2024-11-04T12:00:00-05:00", "2024-11-03T04:00:00Z", "2024-11-04T05:00:00Z"},
	} {
		t.Run(row.name, func(t *testing.T) {
			check(t, testCase{row.name, "昨天", row.start, row.end, "day"}, parseTime(row.now), loc)
		})
	}
}

func TestResolveReturnsFalseForUnresolvableAndAmbiguous(t *testing.T) {
	for _, s := range []string{"上次", "之前", "那个方案", "", "几天前", "昨天还是今天", "上周三和周五", "2月29日", "11月31号", "13月3日", "0天前", "999999999999999999999天前"} {
		t.Run(s, func(t *testing.T) {
			if r, ok := Resolve(s, fixedNow(), zone(t, "Asia/Shanghai")); ok || r != (Range{}) {
				t.Fatalf("unexpected resolution: %+v %v", r, ok)
			}
		})
	}
	if _, ok := Resolve("昨天", fixedNow(), nil); ok {
		t.Fatal("nil location must not select an implicit timezone")
	}
}

func TestResolveNeverGuessesOnMixedGarbage(t *testing.T) {
	for _, s := range []string{"asdf昨天xyz", "上周三之前那个方案", "十一月三十一号garbage", "上周八", "上上周三", "昨天晚上和今天下午", "3天前xyz", "maybe yesterday or tomorrow", "not yesterday", "2025年11月3号", "今天凌晨晚上", "昨天x", "下周三", "-3天前", "三百天前"} {
		t.Run(s, func(t *testing.T) {
			if r, ok := Resolve(s, fixedNow(), zone(t, "Asia/Shanghai")); ok {
				t.Fatalf("must not guess: %+v", r)
			}
		})
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	loc := zone(t, "Asia/Shanghai")
	n := fixedNow()
	first, ok := Resolve("上周三", n, loc)
	if !ok {
		t.Fatal("expected resolution")
	}
	for i := 0; i < 100; i++ {
		got, ok := Resolve("上周三", n, loc)
		if !ok || got != first {
			t.Fatalf("non-deterministic: %+v vs %+v", got, first)
		}
	}
	got, ok := Resolve("上周三", n.In(time.UTC), loc)
	if !ok || got != first {
		t.Fatal("now's display timezone must not affect local calendar", got, first)
	}
}
