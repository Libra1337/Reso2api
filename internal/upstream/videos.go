// videos.go — video_url 抽帧适配。
//
// 背景（2026-10-28 实测）：上游中继层拦截 video_url 内容段——kimi-k3 /
// kimi-k3.1 发真 mp4 也被注入「current model does not support videos」并
// 过滤，视频永远无法透传；而同一通道识图正常。适配方式：出站前把 base64
// 视频用 ffmpeg 抽成 N 帧 image_url 按时间序替换原段，模型按帧序列理解。
//
// 约束：
//   - 仅处理 data:video/*;base64, 内嵌视频（客户端本地文件的常见形态）；
//     http(s) URL 视频不下载（体积/时延不可控），原样透传交上游处理；
//   - 1 fps、最长边 768、最多 10 帧、jpeg q5——单帧 ~40-80KB，总增量 <1MB；
//   - ffmpeg 不在（本地开发/CI）：放行原段（上游回「不支持视频」），不阻塞。
package upstream

import (
	"bytes"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// videoMaxBytes 单个内嵌视频的解码上限（base64 后）。
const videoMaxBytes = 32 << 20

// videoMaxFrames 抽帧上限。
const videoMaxFrames = 10

var (
	ffmpegOnce   sync.Once
	ffmpegAbsent bool
)

// ffmpegPath 探测 ffmpeg（缺失只提示一次）。
func ffmpegPath() string {
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		ffmpegOnce.Do(func() {
			log.Printf("video adapt: ffmpeg not found, video_url passthrough (upstream will reject)")
		})
		ffmpegAbsent = true
		return ""
	}
	return p
}

// expandVideoParts 遍历 messages[].content 多模态数组，把内嵌视频段替换为
// 抽帧图片段。无视频段时零开销（调用方已用 strings.Contains 预筛）。
func expandVideoParts(obj map[string]any) {
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
		for i, pi := range parts {
			part, ok := pi.(map[string]any)
			if !ok || part["type"] != "video_url" {
				continue
			}
			frames := videoFramesFor(part)
			if len(frames) == 0 {
				continue
			}
			repl := make([]any, 0, len(frames)+1)
			repl = append(repl, map[string]any{
				"type": "text",
				"text": "[视频已按 1fps 抽帧为下列 " + itoa(len(frames)) + " 张图，按时间顺序]",
			})
			for _, f := range frames {
				repl = append(repl, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:image/jpeg;base64," + f},
				})
			}
			msg["content"] = append(parts[:i:i], append(repl, parts[i+1:]...)...)
			break // 每条消息抽一次帧；多视频段的罕见场景按首段处理
		}
	}
}

// videoFramesFor 从 video_url 段提取帧 base64。支持三种形态：
// data:video/*;base64,内嵌 / 裸 base64（无前缀，按魔数识别，KVV 实测上游
// 静默丢弃）/ http(s) URL（上游不拉取，网关限时限体积代下载后抽帧）。
// 无法处理（超限/下载失败/ffmpeg 缺失/抽帧失败）返回 nil，原段不变。
func videoFramesFor(part map[string]any) []string {
	url := ""
	switch v := part["video_url"].(type) {
	case map[string]any:
		url, _ = v["url"].(string)
	case string:
		url = v
	}
	var raw []byte
	switch {
	case strings.HasPrefix(url, "data:video/"):
		b64 := url
		if i := strings.Index(b64, ","); i >= 0 {
			b64 = b64[i+1:]
		}
		if len(b64) > base64.StdEncoding.EncodedLen(videoMaxBytes) {
			log.Printf("video adapt: video too large (%dB base64), passthrough", len(b64))
			return nil
		}
		d, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(d) == 0 || !isVideoBytes(d) {
			return nil
		}
		raw = d
	case strings.HasPrefix(url, "data:"):
		return nil
	case strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://"):
		d, ok := fetchVideo(url)
		if !ok {
			return nil
		}
		raw = d
	default:
		// 裸 base64：上游静默丢弃（KVV 实测），网关按魔数识别后抽帧。
		if len(url) < 64 || len(url) > base64.StdEncoding.EncodedLen(videoMaxBytes) {
			return nil
		}
		d, err := base64.StdEncoding.DecodeString(url)
		if err != nil || len(d) == 0 || !isVideoBytes(d) {
			return nil
		}
		raw = d
	}
	return extractFrames(raw)
}

// isVideoBytes 视频容器魔数：mp4([size]ftyp)/webm·mkv(EBML)/avi(RIFF)。
func isVideoBytes(d []byte) bool {
	if len(d) < 12 {
		return false
	}
	if bytes.HasPrefix(d[4:], []byte("ftyp")) {
		return true
	}
	if d[0] == 0x1A && d[1] == 0x45 && d[2] == 0xDF && d[3] == 0xA3 {
		return true
	}
	return bytes.HasPrefix(d, []byte("RIFF"))
}

// videosHTTP 代下载客户端：总超时兜底体积。
var videosHTTP = &http.Client{Timeout: 25 * time.Second}

// fetchVideo 限时限体积下载 URL 视频（≤videoMaxBytes）。
func fetchVideo(url string) ([]byte, bool) {
	resp, err := videosHTTP.Get(url)
	if err != nil {
		log.Printf("video adapt: fetch failed: %v", err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("video adapt: fetch status %d", resp.StatusCode)
		return nil, false
	}
	d, err := io.ReadAll(io.LimitReader(resp.Body, videoMaxBytes+1))
	if err != nil || len(d) == 0 || len(d) > videoMaxBytes || !isVideoBytes(d) {
		if len(d) > videoMaxBytes {
			log.Printf("video adapt: fetched video too large (%dB), passthrough", len(d))
		}
		return nil, false
	}
	return d, true
}

// extractFrames 落盘临时文件后调 ffmpeg 抽帧（fps=1 / 768 / ≤10 帧 / q5）。
func extractFrames(video []byte) []string {
	bin := ffmpegPath()
	if bin == "" || ffmpegAbsent {
		return nil
	}
	tmp, err := os.MkdirTemp("", "wwvid-*")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, "in.mp4")
	if err := os.WriteFile(src, video, 0o600); err != nil {
		return nil
	}
	out := filepath.Join(tmp, "f%03d.jpg")
	cmd := exec.Command(bin,
		"-y", "-loglevel", "error",
		"-i", src,
		"-vf", "fps=1,scale=768:-2:flags=lanczos",
		"-frames:v", itoa(videoMaxFrames),
		"-q:v", "5",
		out,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("video adapt: ffmpeg failed: %v: %.200s", err, stderr.String())
		return nil
	}
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".jpg") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	frames := make([]string, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(tmp, n))
		if err != nil || len(b) == 0 {
			continue
		}
		frames = append(frames, base64.StdEncoding.EncodeToString(b))
	}
	return frames
}

// itoa 轻量整数转字符串（避免只为 itoa 引 strconv 到热路径文件）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
