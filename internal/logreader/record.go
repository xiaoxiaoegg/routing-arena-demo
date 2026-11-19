// internal/logreader/record.go
// LogRecord 对应生产网关消费日志的一行（Type=2 的 logs 表记录）。

package logreader

// LogRecord 一条真实消费记录
type LogRecord struct {
	Time            int64   // 请求时间（unix 秒）
	Group           string  // token 分组
	Model           string  // 模型名
	ChannelID       int     // 实际选中的渠道
	PromptTokens    int     // 输入 token
	CompletionTokens int    // 输出 token
	Quota           float64 // 真实花费（元）
	UseTimeMs       int     // 耗时（毫秒）
}