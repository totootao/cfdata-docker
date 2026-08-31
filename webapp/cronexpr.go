package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronExpr 为标准 5 段 cron 表达式的最小实现：
// 支持 *、*/n、a、a-b、a-b/n 与逗号列表；另支持 "HH:MM" 每日简写。
type cronExpr struct {
	min, hour, dom, mon, dow map[int]bool
	domStar, dowStar         bool
}

// parseCron 解析表达式，失败返回错误
func parseCron(expr string) (*cronExpr, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("空的 cron 表达式")
	}
	// "HH:MM" 简写（无空格）→ "M H * * *"
	if fields := strings.Fields(expr); len(fields) == 1 && strings.Contains(expr, ":") {
		if parts := strings.SplitN(expr, ":", 2); len(parts) == 2 {
			h, err1 := strconv.Atoi(parts[0])
			m, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59 {
				expr = fmt.Sprintf("%d %d * * *", m, h)
			}
		}
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式应为 5 段（分 时 日 月 周），如 '0 6 * * *'")
	}
	c := &cronExpr{}
	var err error
	if c.min, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("分钟字段: %w", err)
	}
	if c.hour, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("小时字段: %w", err)
	}
	if c.dom, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("日字段: %w", err)
	}
	c.domStar = fields[2] == "*"
	if c.mon, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("月字段: %w", err)
	}
	if c.dow, err = parseCronField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("周字段: %w", err)
	}
	c.dowStar = fields[4] == "*"
	// 周字段的 7 等价于周日 0
	if c.dow[7] {
		delete(c.dow, 7)
		c.dow[0] = true
	}
	return c, nil
}

func parseCronField(s string, lo, hi int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("存在空段")
		}
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			v, err := strconv.Atoi(part[i+1:])
			if err != nil || v < 1 {
				return nil, fmt.Errorf("步长 %q 无效", part)
			}
			step = v
			part = part[:i]
		}
		start, end := lo, hi
		if part != "*" {
			nums := strings.SplitN(part, "-", 2)
			a, err := strconv.Atoi(nums[0])
			if err != nil || a < lo || a > hi {
				return nil, fmt.Errorf("值 %q 超出范围 %d-%d", part, lo, hi)
			}
			start, end = a, a
			if len(nums) == 2 {
				b, err := strconv.Atoi(nums[1])
				if err != nil || b < lo || b > hi || b < a {
					return nil, fmt.Errorf("区间 %q 无效", part)
				}
				end = b
			}
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("未匹配任何值")
	}
	return set, nil
}

// dayMatch 实现 Vixie cron 的日/周语义：
// 两者都为 * 时恒真；一方为 * 时看另一方；都受限时任一匹配即可
func (c *cronExpr) dayMatch(t time.Time) bool {
	domOK := c.dom[t.Day()]
	dowOK := c.dow[int(t.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next 返回 after 之后的下一个触发时刻（不含 after 所在分钟）
func (c *cronExpr) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(2, 0, 0) // 最多向前找 2 年
	loc := after.Location()
	for t.Before(limit) {
		if !c.mon[int(t.Month())] {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !c.dayMatch(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !c.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !c.min[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
