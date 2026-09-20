package domain

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// Time-sortable IDs: 10 chars of millisecond timestamp + 10 random chars, so
// ORDER BY id DESC really means "newest first" without a second column.
var idEnc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// NewID returns a prefixed, lexicographically time-sortable identifier.
func NewID(prefix string) string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	ms := uint64(time.Now().UnixMilli()) & 0xFFFFFFFFFF
	var ts [7]byte
	for i := 0; i < 7; i++ {
		ts[6-i] = byte(ms >> (8 * i))
	}
	enc := idEnc.EncodeToString(ts[:]) // 12 chars, last 2 always padding-free
	enc = enc[len(enc)-10:]
	rnd := idEnc.EncodeToString(buf[:])[:10]
	return prefix + "_" + strings.ToLower(enc) + strings.ToLower(rnd)
}

// NowString is the canonical storage timestamp format (RFC3339, UTC, nanos).
func NowString() string { return FormatTime(time.Now()) }

// FormatTime renders a time in the storage format.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseTime reads a storage timestamp; invalid or empty input yields zero time.
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
