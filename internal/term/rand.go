package term

import "crypto/rand"

// randRead 是对 crypto/rand.Read 的薄封装，便于在测试里替换。
func randRead(b []byte) (int, error) { return rand.Read(b) }
