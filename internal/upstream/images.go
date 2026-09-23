// images.go 出站多模态图片 URL 归一化。
//
// 背景：部分客户端（部分中转站 / 第三方 GUI）发图时 image_url.url 只给
// 裸 base64，不带 "data:image/xxx;base64," 前缀。WorkBuddy 上游
// /v2/chat/completions 的模型提供方对这种形态直接 400 code=11133
// （"the request parameters were rejected by the model provider"），
// 表现为「视觉模型（glm-5v-turbo / deepseek-v4.1-flash 等）反代后无法
// 使用视觉功能」。网关出站前按 base64 魔数嗅探补全 data URL 前缀；
// 已带 scheme（data:/http: 等）、过短或未命中已知魔数的值一律不动，
// 避免误伤 URL 引用与非图片字符串。
package upstream

import "strings"

// base64Magic 常见图片 base64 魔数前缀 → MIME。
var base64Magic = [][2]string{
	{"iVBORw0KGgo", "image/png"},
	{"/9j/", "image/jpeg"},
	{"R0lGOD", "image/gif"},
	{"UklGR", "image/webp"},
}

// hasScheme 报告 s 是否带 URL scheme（"data:"/"https:" 等）。
// base64 标准字母表不含 ':'，开头 16 字节内出现即可判定。
func hasScheme(s string) bool {
	for i := 0; i < len(s) && i < 16; i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}

// dataURLFor 裸 base64 → data URL；未命中已知魔数或过短返回空串（调用方不动原值）。
func dataURLFor(raw string) string {
	if len(raw) < 64 { // 短值（路径片段/占位符）不按图片处理
		return ""
	}
	for _, m := range base64Magic {
		if strings.HasPrefix(raw, m[0]) {
			return "data:" + m[1] + ";base64," + raw
		}
	}
	return ""
}

// normalizeImageURLs 遍历 messages[].content 多模态数组，修补裸 base64 图片。
func normalizeImageURLs(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, mi := range msgs {
		msg, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, pi := range parts {
			part, ok := pi.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			iu, ok := part["image_url"].(map[string]any)
			if !ok {
				continue
			}
			url, _ := iu["url"].(string)
			if url == "" || hasScheme(url) {
				continue
			}
			if fixed := dataURLFor(url); fixed != "" {
				iu["url"] = fixed
			}
		}
	}
}

// requestHasImage 报告请求体任一消息 content 数组是否含 image_url part。
// 供 thinking 注入等管线判断（带图请求的思考期长，见 thinking.go）。
func requestHasImage(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	for _, mi := range msgs {
		msg, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, pi := range parts {
			if part, ok := pi.(map[string]any); ok && part["type"] == "image_url" {
				return true
			}
		}
	}
	return false
}

// relocateAssistantImages 把 assistant 消息 content 数组里的 image_url part
// 挪到紧随其后合成的 user 消息。上游丢弃 assistant 轮的图片（实测同样的图
// 在 user 轮可见、在 assistant 轮模型回答 NO IMAGE），部分客户端（agent 回放
// 历史/截图挂 assistant 轮）因此视觉失效。挪动保序：图仍在原 assistant 消息
// 之后，语义不漂移；实测合成 user 消息（纯图无文本）上游正常识图。
func relocateAssistantImages(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs)+2)
	changed := false
	for _, mi := range msgs {
		msg, ok := mi.(map[string]any)
		if !ok {
			out = append(out, mi)
			continue
		}
		role, _ := msg["role"].(string)
		parts, ok := msg["content"].([]any)
		if !ok || role != "assistant" {
			out = append(out, mi)
			continue
		}
		var imgs []any
		kept := make([]any, 0, len(parts))
		for _, pi := range parts {
			if part, ok := pi.(map[string]any); ok && part["type"] == "image_url" {
				imgs = append(imgs, pi)
				continue
			}
			kept = append(kept, pi)
		}
		if len(imgs) == 0 {
			out = append(out, mi)
			continue
		}
		changed = true
		if len(kept) == 0 {
			msg["content"] = ""
		} else {
			msg["content"] = kept
		}
		out = append(out, msg)
		out = append(out, map[string]any{"role": "user", "content": imgs})
	}
	if changed {
		obj["messages"] = out
	}
}
