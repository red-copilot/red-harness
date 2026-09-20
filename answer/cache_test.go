package answer

import (
	"regexp"
	"testing"
)

// 缓存的两条契约：命中/未命中结果**等价**（缓存只是加速，不改变语义），
// 以及到顶后**降级为每次编译**而不是失效或返回 nil。
//
// 为什么要专门钉「到顶」这条：它是本次修复新加的、唯一一个「缓存不写入」
// 的分支。如果这个分支写错（例如返回 nil），表现不是崩溃而是**静默漏判**
// —— flag 抽不出来、题拿不到分，而且只在跨了几十道题之后才出现，几乎无法
// 归因。所以它必须有一条直接用例。

func TestEnvelopeCacheHitEqualsMiss(t *testing.T) {
	// 清空缓存，保证第一次是「未命中 → 编译」，第二次是「命中」。
	envelopeCacheMu.Lock()
	envelopeCache = map[string]*regexp.Regexp{}
	envelopeCacheMu.Unlock()

	e := Envelope{Prefix: "cacheprobe{", Suffix: "}"}
	first := envelopeMatchRe(e)  // 未命中
	second := envelopeMatchRe(e) // 命中

	// 命中与未命中必须给出**同一个** *Regexp（否则缓存没有意义），
	// 且匹配结果一致（正则的语义没有被缓存路径改变）。
	if first != second {
		t.Errorf("缓存命中应返回同一个 *Regexp 实例")
	}
	const text = "noise cacheprobe{a1b2c3} tail"
	if got, want := second.FindAllString(text, -1), []string{"cacheprobe{a1b2c3}"}; len(got) != len(want) || got[0] != want[0] {
		t.Errorf("命中路径的匹配结果变了: got %v, want %v", got, want)
	}
}

func TestEnvelopeCacheBounded(t *testing.T) {
	envelopeCacheMu.Lock()
	envelopeCache = map[string]*regexp.Regexp{}
	envelopeCacheMu.Unlock()

	// 塞满到上界，再多塞几个。
	for i := 0; i < maxEnvelopeCacheEntries+10; i++ {
		re := envelopeMatchRe(Envelope{Prefix: "probe" + itoa(i) + "{", Suffix: "}"})
		if re == nil {
			t.Fatalf("第 %d 个信封拿到 nil 正则（到顶分支写错了）", i)
		}
	}
	envelopeCacheMu.RLock()
	n := len(envelopeCache)
	envelopeCacheMu.RUnlock()
	if n > maxEnvelopeCacheEntries {
		t.Errorf("缓存越过上界: %d > %d", n, maxEnvelopeCacheEntries)
	}

	// 关键：到顶之后**仍然能正确匹配**（降级为每次编译，不是失效）。
	// 这条断言才是这个上界「安全」的全部依据。
	e := Envelope{Prefix: "probe999{", Suffix: "}"}
	got := envelopeMatchRe(e).FindAllString("x probe999{abc} y", -1)
	if len(got) != 1 || got[0] != "probe999{abc}" {
		t.Errorf("缓存到顶后匹配失效: %v", got)
	}
}
