package server

import "testing"

// @think 变体：同元数据克隆 + 后缀；auto 路由别名与已带后缀的不生成。
func TestThinkVariant(t *testing.T) {
	base := map[string]any{"id": "workbuddy/kimi-k3-1", "object": "model", "created": int64(1753600000), "owned_by": "workbuddy", "context_length": 131072}
	v, ok := thinkVariant(base)
	if !ok {
		t.Fatal("expected variant for normal model")
	}
	if v["id"] != "workbuddy/kimi-k3-1@think" {
		t.Errorf("id = %v", v["id"])
	}
	if v["context_length"] != 131072 || v["owned_by"] != "workbuddy" {
		t.Errorf("metadata not cloned: %v", v)
	}
	if base["id"] != "workbuddy/kimi-k3-1" {
		t.Errorf("base entry mutated: %v", base["id"])
	}

	for _, id := range []string{"workbuddy/auto", "workbuddy/kimi-k3-1@think"} {
		if _, ok := thinkVariant(map[string]any{"id": id}); ok {
			t.Errorf("%s should not yield variant", id)
		}
	}
}

// TestVisionVariantAndSuffix 视觉别名：-vl 变体暴露 + 后缀剥离路由。
func TestVisionVariantAndSuffix(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"workbuddy/deepseek-v4.1-flash", true},     // 视觉模型、名字无视觉字样 → 需要 -vl 别名
		{"workbuddy/glm-5v-turbo", true},            // "5v" 是视觉标记但客户端未必识别 → 也给别名
		{"workbuddy/deepseek-v4-pro", false},        // 非视觉
		{"workbuddy/deepseek-v4.1-flash-vl", false}, // 已是别名不叠加
		{"workbuddy/glm-5.3", false},                // 非视觉
	}
	for _, c := range cases {
		entry := map[string]any{"id": c.id}
		v, ok := visionVariant(entry)
		if ok != c.want {
			t.Fatalf("visionVariant(%q) = %v, want %v", c.id, ok, c.want)
		}
		if ok {
			got, _ := v["id"].(string)
			if got != c.id+"-vl" {
				t.Fatalf("别名 id = %q", got)
			}
			if _, changed := entry["id"]; !changed || entry["id"] != c.id {
				t.Fatal("原条目被改写")
			}
		}
	}
	// 后缀剥离：-vl / -vision / @think 组合
	strip := map[string]string{
		"deepseek-v4.1-flash-vl":     "deepseek-v4.1-flash",
		"deepseek-v4.1-flash-vision": "deepseek-v4.1-flash",
		"glm-5v-turbo-vl@think":      "glm-5v-turbo",
		"deepseek-v4.1-flash@think":  "deepseek-v4.1-flash",
		"glm-5.3":                    "glm-5.3",
	}
	for in, want := range strip {
		got, _ := stripThinkSuffix(in)
		if got != want {
			t.Fatalf("stripThinkSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestModelAlias 官方名别名：deepseek-flash → deepseek-v4.1-flash 路由归一。
func TestModelAlias(t *testing.T) {
	if resolveModelAlias("deepseek-flash") != "deepseek-v4.1-flash" {
		t.Fatal("deepseek-flash 别名未归一")
	}
	if resolveModelAlias("deepseek-v4.1-flash") != "deepseek-v4.1-flash" {
		t.Fatal("非别名被改写")
	}
	if resolveModelAlias("glm-5.3") != "glm-5.3" {
		t.Fatal("非别名被改写")
	}
}
