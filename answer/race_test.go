package answer_test

// 本文件钉住一个**曾经真实存在**的数据竞争：answer 包的全局信封正则缓存
// （原 shape.go 的 `var envelopeCache = map[string]*regexp.Regexp{}`）在
// `envelopeMatchRe` 里无锁读、无锁写。
//
// 为什么它长期没被发现（必须记住这个教训）：在 gate 里它**当前不可达** ——
// `gate.Observe` 全程持锁，所有 `Shape.Match` 调用被串行化了，所以 gate 自己的
// 测试（包括 -race）永远是绿的。但 answer 是**导出包**：obs（态势台）、多题
// 并行、CLI 并发调用都会直接从包外调 `Shape.Match` / `Shape.LooksLike`。
//
// 为什么必须修而不是"忍一忍"：Go 的并发 map 写不是"偶尔算错"，是 runtime
// 直接 fatal（"concurrent map writes"），整个宿主进程死掉 —— 正在跑的题、
// 正在写的 DAG 落盘全部丢。而宿主是个长驻进程（跑几十道题不重启），
// 触发概率随运行时长单调上升。
//
// 用例从**包外**发起，就是为了复现真实调用方（obs / CLI）的视角：
// 用一条 cache 里没有的信封前缀，让多个 goroutine 同时走
// 「未命中 → 编译 → 写入」这条路。

import (
	"sync"
	"testing"

	"github.com/red-copilot/red-harness/answer"
)

func TestEnvelopeCacheConcurrentMatch(t *testing.T) {
	// 前缀特意取得生僻：确保缓存里没有它，多个 goroutine 才会同时走到写入。
	// 若复用 DefaultEnvelope，别的用例可能已经把缓存填好了，就测不出写竞争。
	sh := answer.New("rcraceprobe{")
	// 顺带覆盖 AllowRaw 分支（它走 rawTokenRe，不经过缓存，但会并发跑 Match 的
	// 后半段）以及 LooksLike（它内部也调 Match）。
	sh.AllowRaw = true

	const goroutines = 8
	const rounds = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				_ = sh.Match("rcraceprobe{race_probe_value} password=hunter2xyz")
				_ = sh.LooksLike("rcraceprobe{race_probe_value}")
				_ = sh.Contains("rcraceprobe{race_probe_value}")
			}
		}()
	}
	wg.Wait()
}
