//go:build windows

// sample - 长时间采集私有遥测帧，输出逐字节统计，用于推断字段边界与量纲。
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	hid "ugreen-ups/hid"
)

func main() {
	n := flag.Int("n", 60, "采样次数")
	interval := flag.Int("i", 1000, "采样间隔(ms)")
	flag.Parse()

	dev, err := hid.OpenVendor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开设备失败: %v\n", err)
		os.Exit(1)
	}
	defer dev.Close()

	fmt.Printf("设备: %s (VID:%04X PID:%04X) 输入报告 %d 字节\n\n",
		dev.Product, dev.VendorID, dev.ProductID, dev.Caps.InputReportByteLength)

	rows := make([][]byte, 0, *n)
	fmt.Println("采集原始数据:")
	for i := 0; i < *n; i++ {
		buf, err := dev.Read(3000)
		if err != nil {
			fmt.Printf("  #%-3d %v\n", i+1, err)
			time.Sleep(time.Duration(*interval) * time.Millisecond)
			continue
		}
		cp := append([]byte(nil), buf...)
		rows = append(rows, cp)
		fmt.Printf("  #%-3d %s\n", i+1, hex.EncodeToString(cp))
		if i < *n-1 {
			time.Sleep(time.Duration(*interval) * time.Millisecond)
		}
	}

	if len(rows) == 0 {
		return
	}
	width := len(rows[0])

	fmt.Printf("\n逐字节统计 (%d 个样本, 宽度 %d):\n", len(rows), width)
	fmt.Printf("  %-4s %-6s %-6s %-6s %-6s %s\n", "偏移", "最小", "最大", "唯一值", "变化", "值域/全部取值")
	for i := 0; i < width; i++ {
		seen := map[byte]int{}
		min, max := 255, 0
		for _, r := range rows {
			if i < len(r) {
				v := r[i]
				seen[v]++
				if int(v) < min {
					min = int(v)
				}
				if int(v) > max {
					max = int(v)
				}
			}
		}
		keys := make([]int, 0, len(seen))
		for k := range seen {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		var sb strings.Builder
		if len(keys) <= 12 {
			for j, k := range keys {
				if j > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, "%d", k)
			}
		} else {
			sb.WriteString(fmt.Sprintf("<%d种>", len(keys)))
		}
		changed := "常量"
		if len(keys) > 1 {
			changed = "动态"
		}
		fmt.Printf("  %-4d %-6d %-6d %-6d %-6s %s\n", i, min, max, len(keys), changed, sb.String())
	}

	fmt.Println("\n16 位组合（大端 BE / 小端 LE），仅列出存在变化的偏移:")
	for i := 0; i < width-1; i++ {
		beSeen := map[uint16]bool{}
		leSeen := map[uint16]bool{}
		for _, r := range rows {
			if i+1 < len(r) {
				beSeen[uint16(r[i])<<8|uint16(r[i+1])] = true
				leSeen[uint16(r[i+1])<<8|uint16(r[i])] = true
			}
		}
		if len(beSeen) <= 1 && len(leSeen) <= 1 {
			continue
		}
		beVals := sortedKeys(beSeen)
		leVals := sortedKeys(leSeen)
		last := rows[len(rows)-1]
		beRecent := uint16(last[i])<<8 | uint16(last[i+1])
		leRecent := uint16(last[i+1])<<8 | uint16(last[i])
		fmt.Printf("  [%02d-%02d] BE=%-6d (唯一%2d, 值:%v)   LE=%-6d (唯一%2d, 值:%v)\n",
			i, i+1, beRecent, len(beVals), limit(beVals), leRecent, len(leVals), limit(leVals))
	}
}

func sortedKeys(m map[uint16]bool) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	return keys
}

func limit(v []int) []int {
	if len(v) > 6 {
		return v[len(v)-6:]
	}
	return v
}
