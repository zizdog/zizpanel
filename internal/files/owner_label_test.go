package files

// 用户 2026-09-29 报障：外置盘（noowners）满屏 `_unknown:_unknown`，要求简洁成 `-`。
// 判据：只有"系统占位身份"变 `-`；查不到名字的普通数字（NAS 的 1000:1001）必须保留。

import "testing"

func TestOwnerLabelPlaceholderBecomesDash(t *testing.T) {
	cases := []struct {
		uname, gname string
		uid, gid     uint32
		want         string
	}{
		{"_unknown", "_unknown", unknownID, unknownID, "-:-"},
		{"_unknown", "staff", unknownID, 20, "-:staff"},
		{"zizdog", "staff", 501, 20, "zizdog:staff"},
		// NAS 上的数字身份：查不到名字，但是有用信息，必须原样显示。
		{"1000", "1001", 1000, 1001, "1000:1001"},
	}
	for _, c := range cases {
		if got := ownerLabel(c.uname, c.gname, c.uid, c.gid); got != c.want {
			t.Errorf("ownerLabel(%q,%q,%d,%d)=%q，想要 %q", c.uname, c.gname, c.uid, c.gid, got, c.want)
		}
	}
}
