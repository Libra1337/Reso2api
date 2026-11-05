package server

import (
	"strings"
	"testing"
)

// 快路径 extractModel：头部扫描应与结构化解析一致；命中形态不合法时回退。
func TestExtractModelFastPath(t *testing.T) {
	cases := map[string]string{
		`{"model":"deepseek-v4-pro","messages":[]}`:                      "deepseek-v4-pro",
		`{"stream":true,"model":"glm-5.3-flash@think","messages":[]}`:     "glm-5.3-flash@think",
		`{"model":"workbuddy/kimi-k3.1-pro","messages":[]}`:               "workbuddy/kimi-k3.1-pro",
		`{"messages":[{"content":"say \"model\" loudly"}],"model":"x.y"}`: "x.y",
		`{"messages":[]}`: "",
	}
	for body, want := range cases {
		if got := extractModel([]byte(body)); got != want {
			t.Fatalf("extractModel(%s)=%q want %q", body, got, want)
		}
	}
	// model 不在头 2KB（长前缀）→ 回退全量解析
	long := `{"pad":"` + strings.Repeat("p", 3000) + `","model":"tail-model"}`
	if got := extractModel([]byte(long)); got != "tail-model" {
		t.Fatalf("回退路径失败: %q", got)
	}
}
