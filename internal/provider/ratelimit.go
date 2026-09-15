// ratelimit.go — 429 模型级限流（业务码 6004）识别与重置时刻解析。
//
// 上游用 code 6004 表达「该模型的使用量超限」（msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（上游 issue #31）。
// 网关据此做模型级冷却：账号对其他模型保持可用，冷却精确到上游明说的重置墙钟。
package provider

import (
	"regexp"
	"strings"
	"time"
)

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// modelRateLimitCode 明确指向「模型级 429 限流」的业务码。
const modelRateLimitCode = "6004"

var (
	modelRateLimitRe = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	softRateResetRe  = regexp.MustCompile(`将在 (.+?) 重置`)
)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
func IsModelRateLimit(body string) bool {
	return modelRateLimitRe.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的墙钟时刻，失败返回零值 + false。
// 非 6004 即使带「重置」字样也不解析——其重置无冷却语义，解析反而错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	m := softRateResetRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8")
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// blockModelCode 上游业务码 11102：该后端无此模型的确定性答复（非账号故障）。
// 与 6004 的区别：6004 是「该模型用量超限，等重置」，11102 是「这个后端没有
// 这个模型」，重试无意义——按 (账号, 模型) 写负缓存避让，指数退避。
const blockModelCode = "11102"

var blockModelRe = regexp.MustCompile(`"code"\s*:\s*"?` + blockModelCode + `"?`)

// IsModelAbsent 报告 body 是否为 11102「该后端无此模型」。
func IsModelAbsent(body string) bool {
	return blockModelRe.MatchString(body)
}
