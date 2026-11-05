// Package dates resolves spoken time expressions into local-calendar ranges.
// Bare weekdays mean the occurrence strictly before today. Bare weekends mean
// the most recent completed Saturday/Sunday pair (including on a weekend).
// 最近 means seven calendar days including today; 前几天 and 最近几天 mean
// three calendar days including today. 月初/月底 mean the first/last three days
// of the current month. Bare month days use this month; explicit month/day uses
// this year. This week ends at now; whole days, months and years are not clipped.
// N months ago means the corresponding day, clamped to that month's last day;
// N weeks ago means the whole Monday-start week; N hours ago is a trailing window.
// Unspecified 几天前 is ambiguous and is not resolved. Nil locations are rejected.
package dates

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Range is a half-open UTC interval; Label is non-authoritative log text.
// Granularity is day, week, month, part_of_day, or range.
type Range struct {
	Start       time.Time
	End         time.Time
	Label       string
	Granularity string
}

// Resolve recognizes one supported temporal anchor in a recall question. Multiple
// anchors, unsupported qualifiers, and unknown surrounding text are rejected;
// callers must handle false rather than silently choosing a date.
func Resolve(text string, now time.Time, loc *time.Location) (Range, bool) {
	if loc == nil {
		return Range{}, false
	}
	text = clean(text)
	if text == "" || strings.Contains(text, "上次") || strings.Contains(text, "之前") {
		return Range{}, false
	}
	num := `[0-9零〇一二两三四五六七八九十百]+`
	anchor := regexp.MustCompile(`(?:上个月|上月)?` + num + `月` + num + `[日号]|(?:上个月|上月)?` + num + `[日号]|` + num + `个?小时前|` + num + `(?:天|周|个月)前|` + num + `days?ago|last(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday)|lastweek|thismorning|yesterday|today|(?:上上|上|这|本)?(?:周|星期|礼拜)[一二三四五六日天]|上上周|上周末|上周|上个星期|这周|本周|周末|(?:大前天|前天|昨天|今天|明天)?(?:凌晨|早上|早晨|上午|中午|下午|晚上)|昨晚|今早|大前天|前天|昨天|今天|明天|上个月|上月|这个月|本月|月初|月底|去年|今年|前几天|最近几天|最近|刚才|刚刚`)
	matches := anchor.FindAllStringIndex(text, -1)
	if len(matches) != 1 {
		return Range{}, false
	}
	m := matches[0]
	rest := text[:m[0]] + text[m[1]:]
	// Deliberately small recall-question vocabulary: unknown text is not evidence
	// for a date. Keep compound words before their shorter components.
	words := []string{"whatdidwesay", "whatdiditell you", "whatdiditell", "whathappened", "whatdidwediscuss", "tellmeabout", "recall", "remember", "跟你说了什么", "跟你聊了什么", "我们聊了什么", "跟你说过什么", "跟你聊过什么", "我说了什么", "我们说了什么", "跟你", "和你", "我们", "我", "你", "说了", "说过", "聊了", "聊过", "讲了", "谈了", "讨论了", "什么", "那个方案", "的通话", "通话记录", "通话", "事情", "内容", "关于", "帮我", "回忆", "查一下", "找一下", "请问", "还记得", "吗", "呢", "在", "on", "about"}
	for _, w := range words {
		rest = strings.ReplaceAll(rest, w, "")
	}
	if rest != "" {
		return Range{}, false
	}
	text = text[m[0]:m[1]]
	if strings.HasPrefix(text, "上上") && text != "上上周" {
		return Range{}, false
	}
	local := now.In(loc)
	day := midnight(local)
	// Explicit day-of-month forms have priority over relative month words.
	if strings.HasSuffix(text, "号") || strings.HasSuffix(text, "日") && strings.Contains(text, "月") {
		return resolveMonthDay(text, local)
	}
	if r, ok := resolvePart(text, day); ok {
		return r, true
	}
	if r, ok := resolveAgo(text, local, day); ok {
		return r, true
	}
	if r, ok := resolveWeekday(text, day); ok {
		return r, true
	}
	if r, ok := resolveRelative(text, local, day); ok {
		return r, true
	}
	return Range{}, false
}

func clean(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || strings.ContainsRune("，。！？、?!：:", r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
func makeRange(a, b time.Time, label, gran string) (Range, bool) {
	if !b.After(a) {
		return Range{}, false
	}
	return Range{a.UTC(), b.UTC(), label, gran}, true
}
func dayRange(d time.Time, label string) (Range, bool) {
	return makeRange(midnight(d), midnight(d).AddDate(0, 0, 1), label, "day")
}
func weekStart(d time.Time) time.Time {
	n := int(d.Weekday())
	if n == 0 {
		n = 7
	}
	return midnight(d).AddDate(0, 0, 1-n)
}

func resolveRelative(s string, now, today time.Time) (Range, bool) {
	if strings.Contains(s, "今天") || strings.Contains(s, "today") {
		return dayRange(today, "today")
	}
	if strings.Contains(s, "昨天") || strings.Contains(s, "yesterday") {
		return dayRange(today.AddDate(0, 0, -1), "yesterday")
	}
	if strings.Contains(s, "大前天") {
		return dayRange(today.AddDate(0, 0, -3), "three days ago")
	}
	if strings.Contains(s, "前天") {
		return dayRange(today.AddDate(0, 0, -2), "the day before yesterday")
	}
	if strings.Contains(s, "明天") {
		return dayRange(today.AddDate(0, 0, 1), "tomorrow")
	}
	if strings.Contains(s, "今年") {
		return makeRange(time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()), time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, now.Location()), "this year", "range")
	}
	if strings.Contains(s, "去年") {
		y := now.Year() - 1
		return makeRange(time.Date(y, 1, 1, 0, 0, 0, 0, now.Location()), time.Date(y+1, 1, 1, 0, 0, 0, 0, now.Location()), "last year", "range")
	}
	if strings.Contains(s, "上上周") {
		a := weekStart(today).AddDate(0, 0, -14)
		return makeRange(a, a.AddDate(0, 0, 7), "two weeks ago", "week")
	}
	if strings.Contains(s, "上周末") {
		a := weekStart(today).AddDate(0, 0, -2)
		return makeRange(a, a.AddDate(0, 0, 2), "last weekend", "range")
	}
	if strings.Contains(s, "上周") || strings.Contains(s, "上个星期") {
		a := weekStart(today).AddDate(0, 0, -7)
		return makeRange(a, a.AddDate(0, 0, 7), "last week", "week")
	}
	if strings.Contains(s, "这周") || strings.Contains(s, "本周") {
		a := weekStart(today)
		return makeRange(a, now, "this week", "week")
	}
	if strings.Contains(s, "周末") {
		a := weekStart(today).AddDate(0, 0, -2)
		return makeRange(a, a.AddDate(0, 0, 2), "most recent weekend", "range")
	}
	if strings.Contains(s, "上个月") || strings.Contains(s, "上月") {
		a := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location())
		return makeRange(a, a.AddDate(0, 1, 0), "last month", "month")
	}
	if strings.Contains(s, "这个月") || strings.Contains(s, "本月") {
		a := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return makeRange(a, a.AddDate(0, 1, 0), "this month", "month")
	}
	if strings.Contains(s, "月初") {
		a := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return makeRange(a, a.AddDate(0, 0, 3), "beginning of month", "range")
	}
	if strings.Contains(s, "月底") {
		a := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -3)
		return makeRange(a, time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location()), "end of month", "range")
	}
	if strings.Contains(s, "最近几天") || strings.Contains(s, "前几天") {
		return makeRange(today.AddDate(0, 0, -2), today.AddDate(0, 0, 1), "last 3 days", "range")
	}
	if strings.Contains(s, "最近") {
		return makeRange(today.AddDate(0, 0, -6), today.AddDate(0, 0, 1), "last 7 days", "range")
	}
	return Range{}, false
}

func resolveWeekday(s string, today time.Time) (Range, bool) {
	weekdays := map[string]time.Weekday{"一": time.Monday, "二": time.Tuesday, "三": time.Wednesday, "四": time.Thursday, "五": time.Friday, "六": time.Saturday, "日": time.Sunday, "天": time.Sunday}
	for k, w := range weekdays {
		if !strings.Contains(s, "周"+k) && !strings.Contains(s, "星期"+k) && !strings.Contains(s, "礼拜"+k) && s != "last"+strings.ToLower(englishWeekday(w)) {
			continue
		}
		var d time.Time
		label := "most recent " + englishWeekday(w)
		if strings.Contains(s, "上周"+k) || strings.Contains(s, "上星期"+k) || strings.Contains(s, "上礼拜"+k) || s == "last"+strings.ToLower(englishWeekday(w)) {
			d = weekStart(today).AddDate(0, 0, -7+int(w-time.Monday))
			if w == time.Sunday {
				d = weekStart(today).AddDate(0, 0, -1)
			}
			label = "last week " + englishWeekday(w)
		} else if strings.Contains(s, "这周"+k) || strings.Contains(s, "本周"+k) {
			d = weekStart(today).AddDate(0, 0, int(w-time.Monday))
			if w == time.Sunday {
				d = weekStart(today).AddDate(0, 0, 6)
			}
			label = "this week " + englishWeekday(w)
		} else {
			d = today.AddDate(0, 0, -((int(today.Weekday()) - int(w) + 7) % 7))
			if d.Equal(today) {
				d = d.AddDate(0, 0, -7)
			}
		}
		return dayRange(d, label)
	}
	if strings.Contains(s, "lastweek") {
		a := weekStart(today).AddDate(0, 0, -7)
		return makeRange(a, a.AddDate(0, 0, 7), "last week", "week")
	}
	return Range{}, false
}
func englishWeekday(w time.Weekday) string {
	return []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}[w]
}

func resolvePart(s string, today time.Time) (Range, bool) {
	part := ""
	h1, h2 := 0, 0
	if strings.Contains(s, "凌晨") {
		part = "early morning"
		h1, h2 = 0, 6
	} else if strings.Contains(s, "早上") || strings.Contains(s, "早晨") || strings.Contains(s, "上午") || strings.Contains(s, "morning") || s == "今早" {
		part = "morning"
		h1, h2 = 6, 12
	} else if strings.Contains(s, "中午") {
		part = "noon"
		h1, h2 = 11, 14
	} else if strings.Contains(s, "下午") {
		part = "afternoon"
		h1, h2 = 12, 18
	} else if strings.Contains(s, "晚上") || strings.Contains(s, "昨晚") {
		part = "evening"
		h1, h2 = 18, 24
	}
	if part == "" {
		return Range{}, false
	}
	d := today
	if strings.Contains(s, "昨晚") || strings.Contains(s, "昨天") {
		d = d.AddDate(0, 0, -1)
	} else if strings.Contains(s, "大前天") {
		d = d.AddDate(0, 0, -3)
	} else if strings.Contains(s, "前天") {
		d = d.AddDate(0, 0, -2)
	} else if strings.Contains(s, "明天") {
		d = d.AddDate(0, 0, 1)
	}
	a := time.Date(d.Year(), d.Month(), d.Day(), h1, 0, 0, 0, d.Location())
	b := time.Date(d.Year(), d.Month(), d.Day(), h2, 0, 0, 0, d.Location())
	return makeRange(a, b, part, "part_of_day")
}

func resolveMonthDay(s string, now time.Time) (Range, bool) {
	month := int(now.Month())
	year := now.Year()
	day := 0
	if strings.Contains(s, "上个月") || strings.Contains(s, "上月") {
		month--
		if month == 0 {
			month = 12
			year--
		}
	}
	if strings.Contains(s, "月") {
		parts := strings.Split(s, "月")
		if len(parts) > 1 {
			if parts[0] != "上个" && parts[0] != "上" {
				n, ok := parseNum(parts[0])
				if !ok || n < 1 || n > 12 {
					return Range{}, false
				}
				month = n
			}
			tail := parts[1]
			if n, ok := parseNum(strings.TrimRight(tail, "日号")); ok {
				day = n
			}
		}
	}
	if day == 0 {
		if m := regexp.MustCompile(`(?:^|[^0-9])([0-9]+|[一二两三四五六七八九十百]+)(?:日|号)`).FindStringSubmatch(s); len(m) > 0 {
			day, _ = parseNum(m[1])
		}
	}
	if day == 0 {
		return Range{}, false
	}
	if day < 1 || day > 31 {
		return Range{}, false
	}
	d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, now.Location())
	if d.Month() != time.Month(month) {
		return Range{}, false
	}
	return dayRange(d, "date")
}

func resolveAgo(s string, now, today time.Time) (Range, bool) {
	if strings.Contains(s, "刚才") || strings.Contains(s, "刚刚") {
		return makeRange(now.Add(-30*time.Minute), now, "just now", "range")
	}
	m := regexp.MustCompile(`(\d+|[零〇一二两三四五六七八九十百]+)个?小时前`).FindStringSubmatch(s)
	if len(m) > 0 {
		n, ok := parseNum(m[1])
		if ok && n > 0 && n <= 100000 {
			return makeRange(now.Add(-time.Duration(n)*time.Hour), now, "hours ago", "range")
		}
		return Range{}, false
	}
	m = regexp.MustCompile(`(\d+|[零〇一二两三四五六七八九十百]+)(天|周|个月)前`).FindStringSubmatch(s)
	if len(m) == 0 {
		m = regexp.MustCompile(`(\d+)(days?)ago`).FindStringSubmatch(s)
	}
	if len(m) > 0 {
		n, ok := parseNum(m[1])
		if !ok || n < 1 || n > 100000 {
			return Range{}, false
		}
		unit := m[2]
		if unit == "天" || unit == "day" || unit == "days" {
			return dayRange(today.AddDate(0, 0, -n), "days ago")
		}
		if unit == "周" {
			a := weekStart(today).AddDate(0, 0, -7*n)
			return makeRange(a, a.AddDate(0, 0, 7), "weeks ago", "week")
		}
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location()).AddDate(0, -n, 0)
		last := first.AddDate(0, 1, -1).Day()
		d := today.Day()
		if d > last {
			d = last
		}
		return dayRange(first.AddDate(0, 0, d-1), "months ago")
	}
	return Range{}, false
}

func parseNum(s string) (int, bool) {
	if n, e := strconv.Atoi(s); e == nil {
		return n, true
	}
	vals := map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	total, cur := 0, 0
	for _, r := range s {
		if r == '十' {
			if cur == 0 {
				cur = 1
			}
			total += cur * 10
			cur = 0
		} else if v, ok := vals[r]; ok {
			cur = cur*10 + v
		} else {
			return 0, false
		}
	}
	return total + cur, total+cur > 0
}
