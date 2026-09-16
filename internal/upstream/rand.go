package upstream

import "crypto/rand"

// randomBytes 返回 16 字节安全随机数（用于无前缀可哈希时的兜底隔离段）。
func randomBytes() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败几乎不可能；退化用固定值仅保功能不中断。
		for i := range b {
			b[i] = byte(i)
		}
	}
	return b
}
