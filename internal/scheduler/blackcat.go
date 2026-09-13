// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"log"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/upstream"
)

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	type nightAPI interface {
		BlackcatNeed(a *auth.Auth) (int64, error)
		RunNightChats(a *auth.Auth, need int) (int64, error)
	}
	api, ok := s.cfg.Upstream.(nightAPI)
	if !ok {
		return
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		need, err := api.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", a.UID, err)
			continue
		}
		if need <= 0 {
			continue
		}
		done, err := api.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", a.UID, done, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", a.UID, done)
		time.Sleep(activityAccountDelay)
	}
}
