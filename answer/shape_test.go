package answer

import (
	"strings"
	"testing"
)

func TestInferEnvelope(t *testing.T) {
	cases := []struct {
		desc string
		want []string
	}{
		{"拿到 flag 后提交，格式 flag{...}", []string{"flag{"}},
		{"提交形如 CTF{your_answer} 的答案", []string{"CTF{"}},
		{"", nil}, // 空题面 ⇒ 兜底
	}
	for _, c := range cases {
		s := Infer(c.desc)
		if s.Empty() {
			t.Fatalf("Infer(%q) produced empty shape", c.desc)
		}
		if len(c.want) == 0 {
			continue
		}
		found := false
		for _, e := range s.Envelopes {
			if e.Prefix == c.want[0] {
				found = true
			}
		}
		if !found {
			t.Errorf("Infer(%q) envelopes = %v, want prefix %q", c.desc, s.Envelopes, c.want[0])
		}
	}
}

func TestInferRawHint(t *testing.T) {
	s := Infer("请提交管理员密码")
	if !s.AllowRaw {
		t.Error("题面提到「密码」应允许裸串")
	}
	s2 := Infer("提交 flag{...}")
	if !s2.AllowRaw {
		// 题面只说了 flag{ 时，AllowRaw 应为 false（更严格）
		t.Log("note: envelope-only description keeps AllowRaw=false")
	}
}

// TestInferDefaultIsEnvelopeOnly 是 v0.5 **刻意反转**的一条。
//
// 它此前叫 TestInferDefaultAcceptsBoth，断言空题面同时接受信封与裸串，理由是
// 「宁可多认，不可漏认」。那条理由低估了「多认」的代价——它以为代价是「一条
// 候选被平台判错」，实际代价是**提交次数**，而提交次数是有配额的：
//
//	2026-09-22 授权真跑：一道题面既无 `flag{` 也无密码/密钥字样的题，因为这条
//	兜底把每一个原始 token 都收成了候选，打出 147 次 `POST /submit`（146 次是
//	原始 token）。平台上只看得到一个「提交过」的计数，配额已经烧掉了。
//
// 反转而不是删除：这个行为的**变化本身**需要一条断言钉住，否则下一个人会
// 照着「宁可多认」的直觉把它加回来。
func TestInferDefaultIsEnvelopeOnly(t *testing.T) {
	s := Infer("这是一道题")
	if len(s.Envelopes) != 1 || s.Envelopes[0] != DefaultEnvelope {
		t.Errorf("空题面应只认默认信封，got %+v", s)
	}
	if s.AllowRaw {
		t.Error("空题面**不得**默认允许裸串——那正是 147 次提交的成因")
	}
	// 反差面：题面真的提到裸串形态时（规则 3），AllowRaw 仍必须为真。
	// 少了这条，上面那两条断言无法区分「收紧了」与「裸串支持坏了」。
	if !Infer("请提交管理员密码").AllowRaw {
		t.Error("题面提到「密码」时应允许裸串——收紧的是**兜底**，不是裸串支持本身")
	}
}

func TestMatchEnvelope(t *testing.T) {
	s := New("flag")
	got := s.Match("output: flag{abc123} and flag{def456} and nothing else")
	if len(got) != 2 {
		t.Fatalf("want 2 matches, got %v", got)
	}
	if got[0] != "flag{abc123}" || got[1] != "flag{def456}" {
		t.Errorf("got %v", got)
	}
}

func TestMatchDedup(t *testing.T) {
	s := New("flag")
	got := s.Match("flag{same} flag{same} flag{same}")
	if len(got) != 1 {
		t.Errorf("want dedup to 1, got %v", got)
	}
}

func TestMatchDoesNotSpanWhitespace(t *testing.T) {
	s := New("flag")
	// 信封内不允许空白——否则会把一整段输出吞成一个候选
	got := s.Match("flag{a b c}")
	if len(got) != 0 {
		t.Errorf("信封内不应跨越空白，got %v", got)
	}
}

func TestMatchMaxLen(t *testing.T) {
	s := New("flag")
	long := "flag{" + strings.Repeat("A", MaxEnvelopeLen+10) + "}"
	if got := s.Match(long); len(got) != 0 {
		t.Errorf("超长载荷不应匹配，got %d 个", len(got))
	}
}

// 前身 B14 的真实噪音语料：这些**不得**被判成答案。
func TestB14NoiseRejected(t *testing.T) {
	noise := []string{
		"login ==", "pass ==", "admin ==", "login: 500", "admin: 500",
		"zzzzz", "css", "final", "enable_queue",
	}
	s := Infer("提交 flag{...} 与管理员密码")
	for _, n := range noise {
		if s.LooksLike(n) {
			t.Errorf("噪音 %q 不应被判为答案形状", n)
		}
	}
}

// 真凭证必须过。
func TestRealCredsAccepted(t *testing.T) {
	s := Infer("请提交管理员密码")
	for _, c := range []string{"Admin@123", "hunter2xyz", "john.doe"} {
		if !s.LooksLike(c) {
			t.Errorf("真凭证 %q 应被判为答案形状", c)
		}
	}
}

// LooksLike 是 DAG 不变量「答案形状内容拒入图」的判据，必须能识别被洗白的 flag。
func TestLooksLikeCatchesLaunderedFlag(t *testing.T) {
	s := Infer("提交 flag{...}")
	// agent 自己编了一个 flag 写进文件，再读回来 —— 内容必须被识别为答案形状
	if !s.LooksLike("flag{i_made_this_up}") {
		t.Error("自造 flag 必须被判为答案形状，否则会被洗成「观测事实」")
	}
}

func TestLooksLikeRejectsProse(t *testing.T) {
	s := Infer("提交 flag{...}")
	for _, p := range []string{
		"nginx 1.18 running on port 80",
		"the server responded with 200 OK",
		"",
	} {
		if s.LooksLike(p) {
			t.Errorf("散文 %q 不应被判为答案形状", p)
		}
	}
}

func TestEmptyShapeMatchesNothing(t *testing.T) {
	var s Shape
	if got := s.Match("flag{abc}"); len(got) != 0 {
		t.Errorf("零值 Shape 不应匹配任何东西，got %v", got)
	}
	if s.LooksLike("flag{abc}") {
		t.Error("零值 Shape 的 LooksLike 应为 false")
	}
}

func TestRawMinLenEnforced(t *testing.T) {
	s := Shape{AllowRaw: true}
	if s.LooksLike("abc") {
		t.Error("短于 RawMinLen 的裸串不应被接受")
	}
	if !s.LooksLike("x7Kq29fz") {
		t.Error("够长且含数字的裸串应被接受")
	}
}
