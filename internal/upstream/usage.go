// usage.go 官方请求用量接口（积分消耗统计的数据源）。
//
// 端点（Web 域，来源 workbuddy-switch 的 official_usage.rs 实测）：
//
//	POST https://www.workbuddy.cn/billing/meter/get-user-request-usage
//	{startTime:"YYYY-MM-DD 00:00:00", endTime:"...", pageNum:1, pageSize:3000}
//
// 响应行为请求粒度的扣分明细（requestId/credit/model/client/requestTime），
// 分页直至取完；客户端（workbuddy-switch）按 31 天窗口聚合日趋势/模型分布。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"wild-work/internal/auth"
)

const (
	usageURL      = "/billing/meter/get-user-request-usage"
	usagePageSize = 3000
	usageMaxPages = 100
)

// UsageRow 单次请求的扣分明细。
type UsageRow struct {
	RequestID string    `json:"request_id"`
	Credit    float64   `json:"credit"`
	Model     string    `json:"model"`
	Client    string    `json:"client,omitempty"`
	Time      time.Time `json:"time"`
}

// FetchRequestUsage 拉取最近 days 天的请求用量明细（分页取完，requestId+时间去重）。
func (c *Client) FetchRequestUsage(a *auth.Auth, days int) ([]UsageRow, error) {
	if days <= 0 || days > 90 {
		days = 31
	}
	snap := a.Snapshot()
	end := time.Now()
	start := end.AddDate(0, 0, -(days - 1))
	seen := map[string]bool{}
	var rows []UsageRow
	for page := 1; page <= usageMaxPages; page++ {
		body, _ := json.Marshal(map[string]any{
			"startTime": start.Format("2006-01-02") + " 00:00:00",
			"endTime":   end.Format("2006-01-02") + " 23:59:59",
			"pageNum":   page,
			"pageSize":  usagePageSize,
		})
		req, err := http.NewRequest(http.MethodPost, c.webBase()+usageURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+snap.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Origin", c.webBase())
		req.Header.Set("Referer", c.webBase()+"/profile/growth-center")
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("User-Agent", c.chatUA())
		if snap.UID != "" {
			req.Header.Set("X-User-Id", snap.UID)
		}
		data, err := c.doJSON(req)
		if err != nil {
			return nil, err
		}
		// data 两形态：有记录 {total,data:[...]}；无记录直接 []（空数组）。
		var page struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("usage parse: %w", err)
		}
		var rowsIn []struct {
			RequestID   string  `json:"requestId"`
			Credit      float64 `json:"credit"`
			Model       string  `json:"model"`
			Client      string  `json:"client"`
			RequestTime string  `json:"requestTime"`
			// 注意：响应还携带 inputTrunc（请求内容截断）等敏感字段，
			// 此处结构体不声明即不解析、不透出（隐私红线）。
		}
		total := 0
		if len(page.Data) > 0 && page.Data[0] == '{' {
			var obj struct {
				Total int             `json:"total"`
				List  json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(page.Data, &obj); err != nil {
				return nil, fmt.Errorf("usage parse: %w", err)
			}
			total = obj.Total
			_ = json.Unmarshal(obj.List, &rowsIn)
		} else {
			_ = json.Unmarshal(page.Data, &rowsIn)
		}
		pageLen := len(rowsIn)
		for _, r := range rowsIn {
			t, err := time.ParseInLocation("2006-01-02 15:04:05", r.RequestTime, time.Local)
			if err != nil {
				// 兼容毫秒时间戳形态
				if ts := parseInt64(r.RequestTime); ts > 0 {
					if ts < 1e12 {
						ts *= 1000
					}
					t = time.UnixMilli(ts)
				} else {
					continue
				}
			}
			key := fmt.Sprintf("%s@%d", r.RequestID, t.UnixMilli())
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, UsageRow{
				RequestID: r.RequestID, Credit: r.Credit,
				Model: r.Model, Client: r.Client, Time: t,
			})
		}
		if pageLen < usagePageSize || len(rows) >= total {
			break
		}
	}
	return rows, nil
}

func parseInt64(s string) int64 {
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
	}
	return v
}
