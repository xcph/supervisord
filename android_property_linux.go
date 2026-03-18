// +build linux

package main

import (
	"bytes"
	"os"
	"strings"
)

const (
	propValueMax = 92 // PROP_VALUE_MAX in bionic
)

// isBootCompletedFromPropertyFile 读取 /dev/__properties__/u:object_r:boot_status_prop:s0
// 判断 sys.boot_completed 或 dev.bootcomplete 是否为 "1"
func isBootCompletedFromPropertyFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return getPropValue(data, "sys.boot_completed") == "1" ||
		getPropValue(data, "dev.bootcomplete") == "1"
}

// getPropValue 从 prop_area 二进制中解析指定属性的值
// prop_info 布局: serial(4) + value(92) + name(null-terminated)
// 在 data 中查找 name\x00，其前的 92 字节为 value
func getPropValue(data []byte, name string) string {
	needle := append([]byte(name), 0)
	idx := bytes.Index(data, needle)
	if idx < 0 {
		return ""
	}
	if idx < propValueMax {
		return ""
	}
	valueBytes := data[idx-propValueMax : idx]
	if i := bytes.IndexByte(valueBytes, 0); i >= 0 {
		valueBytes = valueBytes[:i]
	}
	return strings.TrimSpace(string(valueBytes))
}
