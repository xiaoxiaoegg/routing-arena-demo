// internal/logreader/csv_test.go
package logreader

import "testing"

func TestLoadFromCSV(t *testing.T) {
	recs, err := LoadFromCSV("../../testdata/sample_logs.csv")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(recs) != 40 {
		t.Fatalf("应读取 40 条，实际 %d", len(recs))
	}
	// 第一条验证
	first := recs[0]
	if first.Model != "gpt-4o" || first.ChannelID != 1 {
		t.Errorf("首条数据不对: %+v", first)
	}
	if first.PromptTokens != 1200 || first.Quota != 0.72 {
		t.Errorf("首条字段解析不对: %+v", first)
	}
	t.Logf("读取成功，共 %d 条，首条: %+v", len(recs), first)
}