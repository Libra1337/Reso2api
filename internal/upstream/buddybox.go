// buddybox.go Buddy 盲盒（能量消耗出口）。
//
// 能量（任务/签到/连登奖励攒的 reward_energy）唯一的用途就是这个盲盒：
// 抽新 Buddy（猫）。端点（chatBase 域 /v2/activity/growth，来源
// 88lin/workbuddy-auto-signin 实测）：
//
//	GET  /v2/activity/growth/buddy/quota  → {affordable, max_open_count}
//	POST /v2/activity/growth/buddy/open   {count, client_token} → {buddy|buddies}
package upstream

import (
	"encoding/json"
	"net/http"

	"wild-work/internal/auth"
)

// BuddyQuota 查询能量可开的盲盒数（affordable=能量够开的次数，maxOpen=单次上限）。
func (c *Client) BuddyQuota(a *auth.Auth) (affordable, maxOpen int, err error) {
	data, err := c.growthJSON(a, http.MethodGet, "/buddy/quota", nil)
	if err != nil {
		return 0, 0, err
	}
	var resp struct {
		Affordable   int `json:"affordable"`
		MaxOpenCount int `json:"max_open_count"`
	}
	if err := jsonUnmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.MaxOpenCount <= 0 {
		resp.MaxOpenCount = 1
	}
	return resp.Affordable, resp.MaxOpenCount, nil
}

// BuddyOpen 开 Buddy 盲盒（消耗能量），返回新 Buddy 名。
func (c *Client) BuddyOpen(a *auth.Auth, count int) (string, error) {
	data, err := c.growthJSON(a, http.MethodPost, "/buddy/open", map[string]any{
		"count":        count,
		"client_token": clientToken(),
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Buddy   string `json:"buddy"`
		Name    string `json:"name"`
		Buddies string `json:"buddies"`
	}
	_ = jsonUnmarshal(data, &resp)
	if resp.Buddy != "" {
		return resp.Buddy, nil
	}
	if resp.Name != "" {
		return resp.Name, nil
	}
	if resp.Buddies != "" {
		return resp.Buddies, nil
	}
	return "新 Buddy", nil
}

// jsonUnmarshal 局部别名（避免每个文件重复 import 块）。
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
