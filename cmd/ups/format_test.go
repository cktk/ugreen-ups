//go:build windows

package main

import "testing"

// 帧数要能撑住"跑好几年"：1 Hz 下 1 亿帧 ≈ 3.17 年，
// 到这个量级必须换成「亿」，否则一串 9 位数字没法一眼读。
func TestFmtFrames(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{99999999, "99,999,999"}, // 刚不到 1 亿，仍是精确千分位
		{100000000, "1.00 亿"},    // 1 亿 ≈ 3.17 年
		{157680000, "1.58 亿"},    // 5 年 @1Hz
		{7890000000, "78.90 亿"},  // 250 年，64 位 int 不会溢出
		{-1, "—"},                // 无数据
	}
	for _, c := range cases {
		if got := fmtFrames(c.in); got != c.want {
			t.Errorf("fmtFrames(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestGroupDigits(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{123, "123"},
		{1234, "1,234"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		if got := groupDigits(c.in); got != c.want {
			t.Errorf("groupDigits(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 续航显示要覆盖从「秒」到「年」的全部量级 —— 小容量 UPS 只撑几分钟，
// 而设备续航字段在离线时可能是极大值，不能出现 "3650 天" 这种读法。
func TestFmtDuration(t *testing.T) {
	const (
		minute = 60
		hour   = 60 * minute
		day    = 24 * hour
	)
	cases := []struct {
		in   int
		want string
	}{
		{-1, "—"}, // 不可用
		{0, "0 秒"},
		{59, "59 秒"},
		{60, "1 分 0 秒"},
		{90, "1 分 30 秒"},
		{hour, "1 小时 0 分"},
		{day, "1 天 0 小时"},
		{400 * day, "1 年 35 天"}, // 超过一年折算成年 + 天
		{3650 * day, "10 年 0 天"},
	}
	for _, c := range cases {
		if got := fmtDuration(c.in); got != c.want {
			t.Errorf("fmtDuration(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
