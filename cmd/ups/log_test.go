//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setupLog 把日志重定向到临时目录，返回目录路径。
// 测试结束会还原全局状态，避免污染其它用例。
func setupLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ups-monitor.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("打开临时日志失败: %v", err)
	}

	oldPath, oldFile, oldSize := logPath, logFile, logSize
	logPath, logFile, logSize = path, f, 0
	t.Cleanup(func() {
		if logFile != nil {
			_ = logFile.Close()
		}
		logPath, logFile, logSize = oldPath, oldFile, oldSize
	})
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", filepath.Base(path), err)
	}
	return string(b)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// 程序要能连续跑几年，日志必须自己滚：写入量超限就归档，
// 且历史份数固定，磁盘占用有上限。
func TestLogRotatesOnSizeLimit(t *testing.T) {
	dir := setupLog(t)
	logf("第一段")

	// 把计数器顶到临界，下一次写入就该触发滚动
	logSize = logMaxBytes - 1
	logf("第二段")

	base := filepath.Join(dir, "ups-monitor.log")
	if !fileExists(base + ".1") {
		t.Fatal("超过上限后未生成 ups-monitor.log.1")
	}
	// 注意写入顺序：logf 先落盘再判断大小，所以触发滚动的那一行留在归档文件里，
	// 新文件只有 rotateLog 写的滚动说明行。
	archived := readFile(t, base+".1")
	for _, want := range []string{"第一段", "第二段"} {
		if !strings.Contains(archived, want) {
			t.Errorf(".1 中未找到 %q，实际内容:\n%s", want, archived)
		}
	}
	cur := readFile(t, base)
	if !strings.Contains(cur, "日志已滚动") {
		t.Errorf("当前日志缺少滚动说明行，实际内容:\n%s", cur)
	}
	if strings.Contains(cur, "第二段") {
		t.Errorf("归档内容不应残留在当前日志中，实际内容:\n%s", cur)
	}
	if logSize >= logMaxBytes {
		t.Errorf("滚动后 logSize = %d，应已清零重计", logSize)
	}
}

// 历史文件数必须封顶：滚很多轮之后不能冒出 .4，最旧的内容要被丢弃。
func TestLogRotationKeepsOnlyNBackups(t *testing.T) {
	dir := setupLog(t)

	const rounds = logKeepFiles + 3
	for i := 0; i < rounds; i++ {
		logf("第 %d 段", i)
		rotateLog("测试滚动")
	}

	base := filepath.Join(dir, "ups-monitor.log")
	for i := 1; i <= logKeepFiles; i++ {
		if !fileExists(base + "." + strconv.Itoa(i)) {
			t.Errorf("缺少历史文件 ups-monitor.log.%d", i)
		}
	}
	if fileExists(base + "." + strconv.Itoa(logKeepFiles+1)) {
		t.Errorf("历史文件数超过上限，不该存在 ups-monitor.log.%d", logKeepFiles+1)
	}

	// 保留的应是最新的 logKeepFiles 段：.1 最新、.3 最旧 = 第 rounds-logKeepFiles 段
	wantOldest := rounds - logKeepFiles
	oldest := readFile(t, base+"."+strconv.Itoa(logKeepFiles))
	if !strings.Contains(oldest, "第 "+strconv.Itoa(wantOldest)+" 段") {
		t.Errorf("最旧历史文件应含「第 %d 段」，实际:\n%s", wantOldest, oldest)
	}
	// 更早的内容必须已经被丢掉
	if strings.Contains(oldest, "第 "+strconv.Itoa(wantOldest-1)+" 段") {
		t.Errorf("超出保留份数的旧内容仍在，实际:\n%s", oldest)
	}
}

// 下次启动时若发现上次留下的日志已超限，应立即滚动，而不是继续往里写。
func TestLogRotatesOnStartupWhenOversized(t *testing.T) {
	dir := setupLog(t)
	base := filepath.Join(dir, "ups-monitor.log")

	// 模拟"上次退出时已经写满"
	if err := logFile.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	logFile = nil
	big := make([]byte, logMaxBytes+1)
	if err := os.WriteFile(base, big, 0644); err != nil {
		t.Fatalf("写入大文件失败: %v", err)
	}

	// 重新走一遍 logInit 的超限判断
	st, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	f, err := os.OpenFile(base, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	logFile, logSize = f, st.Size()
	if logSize >= logMaxBytes {
		rotateLog("启动时发现日志已超限")
	}

	if !fileExists(base + ".1") {
		t.Fatal("启动时发现超限日志，未滚动")
	}
	if logSize >= logMaxBytes {
		t.Errorf("滚动后 logSize = %d，应已清零", logSize)
	}
}
