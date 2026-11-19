// internal/sim/source.go
// 统一取数入口：数据源唯一的切换点。
//
// 设计意图：所有 cmd 都从这里取数据，将来接入生产网关真实消费日志时，
// 只要在这里把"生成"换成"查库"，上层代码一行不用改。
//   - 给了 csv 路径 → 读文件（外部数据，无故障窗口）
//   - 没给 → 用生成器造模拟数据（可配条数/渠道/模型/窗口）

package sim

import (
	"routing-arena-demo/internal/logreader"
)

// Source 取数结果：记录 + 故障窗口（回放判定成功/失败用）
type Source struct {
	Records   []logreader.LogRecord
	Faults    []FaultWindow
	FromFile  string // 非空表示来自 CSV 文件
	Simulated bool   // 是否来自模拟生成器
}

// Summarize 数据规模摘要（打印用）
func (s Source) Describe() string {
	kind := "模拟生成"
	if !s.Simulated {
		kind = "外部数据: " + s.FromFile
	}
	return kind
}

// LoadOrGenerate 统一取数：csvPath 非空则读 CSV，否则生成模拟数据。
func LoadOrGenerate(csvPath string, cfg Config) (Source, error) {
	if csvPath != "" {
		recs, err := logreader.LoadFromCSV(csvPath)
		if err != nil {
			return Source{}, err
		}
		return Source{Records: recs, FromFile: csvPath, Simulated: false}, nil
	}
	recs, faults := Generate(cfg)
	return Source{Records: recs, Faults: faults, Simulated: true}, nil
}
