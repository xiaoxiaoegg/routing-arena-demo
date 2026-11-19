// internal/logreader/csv.go
// 读取 CSV 格式的消费日志。

package logreader

import (
	"encoding/csv"
	"os"
	"strconv"
)

// LoadFromCSV 从 CSV 文件读取消费记录。
// 列顺序：time,group,model,channel_id,prompt_tokens,completion_tokens,quota,use_time_ms
func LoadFromCSV(path string) ([]LogRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}

	var out []LogRecord
	for i, rec := range records {
		if i == 0 { // 跳过表头
			continue
		}
		t, _ := strconv.ParseInt(rec[0], 10, 64)
		pt, _ := strconv.Atoi(rec[4])
		ct, _ := strconv.Atoi(rec[5])
		q, _ := strconv.ParseFloat(rec[6], 64)
		us, _ := strconv.Atoi(rec[7])
		ch, _ := strconv.Atoi(rec[3])

		out = append(out, LogRecord{
			Time:             t,
			Group:            rec[1],
			Model:            rec[2],
			ChannelID:        ch,
			PromptTokens:     pt,
			CompletionTokens: ct,
			Quota:            q,
			UseTimeMs:        us,
		})
	}
	return out, nil
}