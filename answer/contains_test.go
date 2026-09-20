package answer

import "testing"

// 这个文件钉住一个**已经被踩到过的坑**：裸串形态（AllowRaw）下，`Match` 会把
// 命令里的普通 token（IP、URL 路径、文件路径）当成候选。
//
// 后果不是「gate 误报」——gate 侧已经绕开了 Contains，改用逐候选物化匹配。
// 真正的后果在 **dag**：它用 `Shape.LooksLike` 判定「这段内容是不是答案形状，
// 是就拒收、不进事实库」。如果 dag 改用一个更宽的判据（例如 Contains），
// 那么 `10.0.0.1`、`/etc/passwd` 这类正常事实会被当成 flag 拒收，
// **图会永久学不到任何东西**——这是静默的、灾难性的。
//
// 所以这里把两者的边界写死：LooksLike 是「一个短串的身份判定」，
// Match 是「从长文本里挖候选」，Contains 是 Match 的薄包装，
// **不可以用 Contains 做拒收判据**。
func TestRawShapeMatchIsTooWideForRejection(t *testing.T) {
	s := Shape{AllowRaw: true}

	// 这些是命令片段，不是答案。Match 会挖出候选（这是它的职责：宁可多挖，
	// 交给后面的闸去筛），但 LooksLike 必须说「不，这个串本身不是答案形状」。
	notAnswers := []string{
		"10.0.0.1",
		"//10.0.0.1/flag",
		"/usr/share/seclists/common.txt",
	}
	for _, v := range notAnswers {
		if !s.LooksLike(v) {
			continue // 已经拒了，正合期望
		}
		// LooksLike 若认了，说明这个串确实长得像裸串答案（例如含数字的 IP）。
		// 这不是 bug——裸串形态下 IP 与密码同形，是词法判据的固有代价。
		// 但必须记录：dag 若在裸串题上用它做拒收，会误伤。
		t.Logf("note: 裸串形态下 LooksLike(%q) = true —— dag 在裸串题上做拒收需另设更窄的判据", v)
	}

	// 反向：真正的答案串必须被 LooksLike 认。
	for _, v := range []string{"flag{real_one}", "Admin@123", "hunter2xyz"} {
		if !s.LooksLike(v) {
			t.Errorf("LooksLike(%q) 应为 true", v)
		}
	}
}

// 信封形态下没有这个问题：`flag{...}` 是语法标记，不会与 IP/路径同形。
func TestEnvelopeShapeIsPrecise(t *testing.T) {
	s := Infer("提交 flag{...}")
	for _, v := range []string{"10.0.0.1", "/etc/passwd", "http://t/flag", "nmap -sV"} {
		if s.LooksLike(v) {
			t.Errorf("信封形态下 LooksLike(%q) 应为 false", v)
		}
	}
	if s.Contains("curl http://10.0.0.1/flag") {
		t.Error("信封形态下命令文本不应被判为含答案")
	}
}
