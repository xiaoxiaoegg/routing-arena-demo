// internal/store/store.go
// 极简 JSONL 存储：一行一条 JSON，追加写、顺序读。
//
// 用途：把切换历史 / 评估记录 / 告警从"内存里、重启就没了"变成"落盘可查"。
// 设计取舍：
//   - JSONL 而不是 SQLite：零依赖、人类可读、天然追加语义，demo 阶段够用；
//     接生产网关后直接换成消费日志库（策略决策本来就要写进日志）。
//   - 只追加不修改：审计日志就该是不可变的。
package store

import (
	"bufio"
	"encoding/json"
	"os"
)

// Append 追加一条 JSON 记录到文件（不存在则创建，权限 0644）。
// 失败返回 error（调用方决定是否降级为纯内存模式）。
func Append(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// Load 读取 JSONL 文件，逐行反序列化交给 fn；文件不存在返回 found=false（不算错误）。
// 坏行会跳过并计数（审计日志允许个别损坏行，不拖垮整体读取）。
// 返回 (found, 处理行数, 跳过行数, err)。
func Load(path string, fn func(data []byte) error) (found bool, n, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, 0, nil
		}
		return false, 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			skipped++
			continue
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return true, n, skipped, err
	}
	return true, n, skipped, nil
}
