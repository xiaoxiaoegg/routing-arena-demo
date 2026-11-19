// internal/strategy/s4_robust.go
// S4_Robust：稳健优先策略——在 S3 基础上大幅调高健康度权重
// （0.2*成本 + 0.2*延迟 + 0.6*健康度）。
// 作用：应对大促或上游大面积故障时段，宁可多花点钱也要保证可用。
// 复用 S3 的 Scorer，只换权重。

package strategy

// S4Robust 稳健优先策略
type S4Robust struct {
	Scorer
}

func (s *S4Robust) ID() string { return "S4_Robust" }

// SetWeights 动态调整权重
func (s *S4Robust) SetWeights(w HybridWeights) {
	s.Weights = w
}
