// stubpi 是 piai 测试用的**假 pi**。
//
// 它存在的唯一理由：在没有 LLM、没有网络、也没有真实 262 KB 工具输出的条件下，
// 把 pi RPC 的失效模式逐个复现出来。最要紧的是 **writer 死锁**——脚本先吐 1 MB
// 事件、期间不读 stdin，同时把 extension_ui_request 也塞进这坨事件里；客户端若
// 在 reader 协程里内联写 stdin（应答对话框），就会卡在写满的 stdin 管道上，而
// stub 卡在写 stdout 上，双方互等。这是唯一能在离线环境复现该死锁的手段。
//
// 用法：stubpi --scenario <名字> [--session-dir D] [--provider P] [--model M]
//
//	[-e PATH] [--approve] [--thinking L] [--append-system-prompt S]
//	--version        打印 0.86.0 并退出 0
//	--bad-version    打印 9.9.9（超出版本区间）并退出 0
//	--version-fail   往 stderr 写一行并以退出码 3 结束
//
// 环境变量：
//
//	STUBPI_SCENARIO  同 --scenario
//	STUBPI_CRLF=1    所有帧以 \r\n 结尾（协议要求接受 \r\n）
//	STUBPI_U2028=1   帧内嵌 U+2028/U+2029（不能当分隔符）
//	STUBPI_NOTES     收到的帧与应答的记录文件路径（测试断言用）
//
// 场景：
//
//	normal       正常一轮：prompt → 文本增量 → 工具调用（含 details）→ settled
//	ui           正常一轮 + 两个 dialog 与三个 fire-and-forget UI 请求
//	eof          prompt 后吐几条事件然后退出（进程中途死亡）
//	stall        prompt 后吐 1 MB 事件、把 UI 请求塞在中间、期间不读 stdin
//	stallforever 同 stall，且此后永不读 stdin（测 ctx 取消能返回）
//	slowstate    前两次 get_state 正常答，之后一律不答（看门狗探活超时 → wedged）
//	noreset      new_session 后 sessionId 不变、messageCount 不减
//	nosettle     prompt 后只吐事件、从不 agent_settled（abort 宽限路径）
//	nocommands   get_commands 返回空列表（extension 自检失败）
//	providerr    provider 失败：stopReason=error + 空 content + 照常 settled
//	provider1st  第 1 轮 provider 失败、第 2 轮起正常（测 ProviderError 的轮级语义）
//	big          单帧 512 KB（reader 缓冲上限回归）
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const stubVersion = "0.86.0"

func main() {
	args := os.Args[1:]
	scenario := os.Getenv("STUBPI_SCENARIO")
	var exts []string
	approve := false
	crlf := os.Getenv("STUBPI_CRLF") == "1"
	u2028 := os.Getenv("STUBPI_U2028") == "1"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			fmt.Println(stubVersion)
			return
		case "--bad-version":
			fmt.Println("9.9.9")
			return
		case "--version-fail":
			fmt.Fprintln(os.Stderr, "stubpi: 版本探测失败")
			os.Exit(3)
		case "--scenario":
			if i+1 < len(args) {
				i++
				scenario = args[i]
			}
		case "-e":
			if i+1 < len(args) {
				i++
				exts = append(exts, args[i])
			}
		case "--approve":
			approve = true
		case "--session-dir", "--provider", "--model", "--thinking", "--append-system-prompt":
			i++ // 吃掉值：这些参数对 stub 的行为没有影响
		}
	}

	s := &stub{
		scenario: scenario,
		exts:     exts,
		approve:  approve,
		crlf:     crlf,
		u2028:    u2028,
		session:  fmt.Sprintf("sess-%d-0001", os.Getpid()),
		notes:    openNotes(os.Getenv("STUBPI_NOTES")),
	}
	// 模拟 pi 的 bundled node 子进程：killpg 若没覆盖整组，这个子进程会成为
	// 孤儿继续活着（前身 _stop_process_tree 的教训）。
	if f := os.Getenv("STUBPI_SPAWN_CHILD"); f != "" {
		child := exec.Command("sleep", "300")
		if err := child.Start(); err == nil {
			os.WriteFile(f, []byte(fmt.Sprint(child.Process.Pid)), 0o600)
		}
	}
	s.run()
}

func openNotes(p string) *os.File {
	if p == "" {
		return nil
	}
	f, err := os.Create(p)
	if err != nil {
		return nil
	}
	return f
}

type stub struct {
	scenario string
	exts     []string
	approve  bool
	crlf     bool
	u2028    bool

	session string
	gen     int
	turns   int
	// stopReading 让 run() 的读循环退出（stallforever 场景：应答完 prompt
	// 之后再不看 stdin，制造「writer 被堵住而 reader 必须继续消费 stdout」）。
	stopReading bool
	// stallForever 表示退出读循环后不退出进程。
	stallForever bool
	// prompts 是收到的 prompt 条数。provider1st 靠它区分「第 1 轮失败」与
	// 「之后都正常」——「轮级 provider 错误不能污染后续轮次」这条只能这样测。
	prompts int
	// stateQueries 是收到的 get_state 次数。slowstate 场景靠它区分「启动期
	// 握手」与「看门狗探活」：前两次必须正常答，否则 Start 本身就会失败，
	// 根本走不到看门狗那条路径。
	stateQueries int

	mu     sync.Mutex
	uiSeen []string
	notes  *os.File
}

// note 往 NOTES 文件追加一行。它把「客户端到底发了什么」变成可断言的事实，
// 而不是靠测试端猜测（尤其是 UI 应答的键名）。
func (s *stub) note(format string, a ...any) {
	if s.notes == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.notes, format+"\n", a...)
	s.notes.Sync()
}

func (s *stub) write(b []byte) {
	if s.u2028 {
		// 把 U+2028/U+2029 塞进 JSON **字符串值内部**（不是行尾）。任何按
		// Unicode 行分隔符切分的 reader（Node readline 语义）都会在这里把一帧
		// 切成两帧；严格 LF 的 reader 不受影响。rpc.md 专门点名了这一点。
		sep := " SEP "
		b = []byte(strings.Replace(string(b), `"delta":"`, `"delta":"`+sep, 1))
	}
	os.Stdout.Write(b)
	if s.crlf {
		os.Stdout.Write([]byte("\r\n"))
	} else {
		os.Stdout.Write([]byte("\n"))
	}
}

func (s *stub) send(obj map[string]any) {
	b, _ := json.Marshal(obj)
	s.write(b)
}

func (s *stub) respond(id, command string, data any) {
	obj := map[string]any{"type": "response", "command": command, "success": true, "id": id}
	if data != nil {
		obj["data"] = data
	}
	s.send(obj)
}

func (s *stub) stateData() map[string]any {
	return map[string]any{
		"sessionId":             s.session,
		"sessionFile":           "/tmp/stubpi/" + s.session + ".jsonl",
		"isStreaming":           false,
		"isCompacting":          false,
		"messageCount":          0,
		"pendingMessageCount":   0,
		"thinkingLevel":         "medium",
		"steeringMode":          "all",
		"followUpMode":          "one-at-a-time",
		"autoCompactionEnabled": true,
	}
}

func (s *stub) statsData() map[string]any {
	return map[string]any{
		"sessionFile":       "/tmp/stubpi/" + s.session + ".jsonl",
		"sessionId":         s.session,
		"userMessages":      2,
		"assistantMessages": 2,
		"toolCalls":         s.turns,
		"toolResults":       s.turns,
		"totalMessages":     2*s.turns + 2,
		"tokens": map[string]any{
			"input": 1000, "output": 200, "cacheRead": 0, "cacheWrite": 0, "total": 1200,
		},
		"cost": 0.0123,
		"contextUsage": map[string]any{
			"tokens": 4321, "contextWindow": 200000, "percent": 2.2,
		},
	}
}

func (s *stub) commandsData() map[string]any {
	cmds := []any{}
	if s.scenario == "nocommands" {
		return map[string]any{"commands": cmds}
	}
	// 没开 --approve 时**不报**扩展命令：这正是「项目资源被静默忽略」那条会
	// 悄悄失效的路径（M0 实测的坑），必须能被测试复现。
	if len(s.exts) > 0 && !s.approve {
		return map[string]any{"commands": cmds}
	}
	for _, e := range s.exts {
		cmds = append(cmds, map[string]any{
			"name": "stub-ext", "description": "stub extension", "source": "extension",
			"sourceInfo": map[string]any{"path": e},
		})
	}
	return map[string]any{"commands": cmds}
}

func (s *stub) run() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var cmd map[string]any
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			s.send(map[string]any{"type": "response", "command": "parse", "success": false,
				"error": "Failed to parse command: " + err.Error()})
			continue
		}
		typ, _ := cmd["type"].(string)
		if typ == "" {
			// 缺 type 时 pi 回 `Unknown command: undefined`（M0 实测）。stub
			// 照做，好让「出站帧必须带 type」这条纪律有回归。
			s.send(map[string]any{"type": "response", "command": "", "success": false,
				"error": "Unknown command: undefined"})
			continue
		}
		s.handle(cmd, typ)
		if s.stopReading {
			break
		}
	}
	if s.stallForever {
		// 永不退出。这里**不能**写 `select {}`：Go 的运行时死锁检测会判定
		// 「所有 goroutine 都睡着了」，直接 fatal 退出——那就变成了进程死亡，
		// 而不是我们要测的「活着但不读 stdin」。
		for {
			time.Sleep(time.Hour)
		}
	}
}

// burst 吐出约 n 字节事件，期间不读 stdin。
func (s *stub) burst(n int) {
	payload := strings.Repeat("x", 512)
	for written := 0; written < n; {
		s.send(map[string]any{"type": "tool_execution_update", "toolCallId": "call_burst",
			"toolName": "bash",
			"partialResult": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": payload}}}})
		written += len(payload) + 160
	}
}

func (s *stub) handle(cmd map[string]any, typ string) {
	id, _ := cmd["id"].(string)
	s.note("CMD %s", typ)

	switch typ {
	case "get_commands":
		s.respond(id, typ, s.commandsData())

	case "get_state":
		s.mu.Lock()
		s.stateQueries++
		n := s.stateQueries
		s.mu.Unlock()
		if s.scenario == "slowstate" && n > 2 {
			return // 永不回答：看门狗探活超时 → 连续两次 → wedged
		}
		st := s.stateData()
		if s.scenario == "noreset" {
			st["sessionId"] = "sess-0001" // 永远不变
			st["messageCount"] = 7
			// isStreaming 保持 false：客户端会先断言 streaming，再断言
			// sessionId/messageCount。让它过掉前一条，才能走到后面两条。
		}
		s.respond(id, typ, st)

	case "get_session_stats":
		s.respond(id, typ, s.statsData())

	case "new_session":
		if s.scenario != "noreset" {
			s.gen++
			s.session = fmt.Sprintf("sess-%04d", s.gen+1)
		}
		s.turns = 0
		s.respond(id, typ, map[string]any{"cancelled": false})

	case "steer":
		if msg, _ := cmd["message"].(string); msg != "" {
			s.note("STEER %s", msg)
		}
		s.respond(id, typ, nil)

	case "abort":
		s.respond(id, typ, nil)
		if s.scenario == "nosettle" {
			// abort 之后补一个 settled：让宽限期路径走到「收到 settled」。
			s.send(map[string]any{"type": "agent_settled"})
		}

	case "clear_queue":
		s.respond(id, typ, map[string]any{"steering": []any{}, "followUp": []any{}})

	case "prompt":
		if _, has := cmd["streamingBehavior"]; has {
			s.note("PROMPT_HAS_STREAMING_BEHAVIOR")
		}
		s.respond(id, typ, nil)
		if s.scenario == "stall" || s.scenario == "stallforever" {
			// 真实 pi 的次序：先应答 prompt，再开始吐事件流。这里在应答之后
			// 灌 1 MB 事件，并且**不再读 stdin**——stdin 管道被堵住的窗口就是
			// 这么造出来的。用 goroutine 是为了让读循环立刻停下（stallforever）。
			s.stopReading = true
			s.stallForever = s.scenario == "stallforever"
			go func() {
				// 先等一会儿再发 UI 请求：这段时间里客户端会继续灌 steer，
				// stdin 管道（64 KB）必然写满、writer 阻塞在 Write 上。UI 请求
				// 必须在**这个时刻之后**到达——否则「内联写 stdin」那条错误
				// 写法会因为管道还空着而侥幸不阻塞，用例就测不出死锁。
				time.Sleep(400 * time.Millisecond)
				s.dialogs()
				s.burst(1 << 20)
			}()
			return
		}
		go s.runTurn()

	case "extension_ui_response":
		rid, _ := cmd["id"].(string)
		if _, ok := cmd["requestId"]; !ok {
			rid = "BAD:" + rid // 缺 requestId：设计 §二 要求两个键都带
		}
		s.note("UIRESP %s", rid)
		s.mu.Lock()
		s.uiSeen = append(s.uiSeen, rid)
		s.mu.Unlock()

	default:
		s.send(map[string]any{"type": "response", "command": typ, "success": false, "id": id,
			"error": "Unknown command: " + typ})
	}
}

// runTurn 演一轮。所有场景共享同一骨架，差异只在「何时/是否收尾」。
func (s *stub) runTurn() {
	s.mu.Lock()
	s.turns++
	s.prompts++
	promptNo := s.prompts
	s.mu.Unlock()

	s.send(map[string]any{"type": "agent_start"})
	s.send(map[string]any{"type": "turn_start"})
	s.send(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant"}})

	if s.scenario == "big" {
		// 单帧 512 KB：reader 缓冲上限的回归（M0 实测真实帧可达 262 KB）。
		s.send(map[string]any{"type": "tool_execution_end", "toolCallId": "call_big", "toolName": "bash",
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": strings.Repeat("B", 512<<10)}},
			}})
	}

	s.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_start", "contentIndex": 0}})
	// 预热：把 1/s 的节流窗口先占掉。这样紧随其后的三段增量必然被节流成 1 条
	// 事件，而 acc.text 仍然一个字符不少——节流只该影响事件密度，不该影响
	// RoundResult.Text（它是权威产物）。
	s.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "预热。"}})
	s.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "第一段文本。"}})
	s.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": 1, "delta": "（思考）"}})
	s.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "第二段文本。"}})

	if s.scenario == "eof" {
		// 进程中途死亡：吐几条事件后直接退出，**不发 agent_settled**。
		os.Exit(9)
	}

	s.send(map[string]any{"type": "tool_execution_start", "toolCallId": "call_1", "toolName": "bash",
		"args": map[string]any{"command": "ls -la"}})

	if s.scenario == "ui" {
		s.dialogs()
	}

	s.send(map[string]any{"type": "tool_execution_end", "toolCallId": "call_1", "toolName": "bash",
		"isError": false,
		"result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "total 0\n"}},
			"details": map[string]any{"report_fact": map[string]any{
				"facts":  []any{map[string]any{"kind": "service", "content": "nginx 1.18"}},
				"next":   "试试 /admin",
				"filler": strings.Repeat("A", 4096),
			}},
		}})

	if s.scenario == "providerr" || (s.scenario == "provider1st" && promptNo == 1) {
		// M0 实测的 401 呈现方式：末条 assistant 消息 stopReason=error + 空
		// content，然后**照常** agent_settled。不识别就会静默烧题库。
		s.send(map[string]any{"type": "turn_end", "message": map[string]any{
			"role": "assistant", "stopReason": "error", "errorMessage": "API key is invalid",
			"content": []any{},
		}, "toolResults": []any{}})
		s.send(map[string]any{"type": "agent_end", "messages": []any{
			map[string]any{"role": "assistant", "stopReason": "error",
				"errorMessage": "API key is invalid", "content": []any{}},
		}, "willRetry": false})
		s.send(map[string]any{"type": "agent_settled"})
		return
	}

	s.send(map[string]any{"type": "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_end", "contentIndex": 0,
			"content": "预热。第一段文本。第二段文本。"}})
	s.send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop"}})
	s.send(map[string]any{"type": "turn_end", "message": map[string]any{"role": "assistant", "stopReason": "stop"}, "toolResults": []any{}})
	s.send(map[string]any{"type": "agent_end", "messages": []any{
		map[string]any{"role": "assistant", "stopReason": "stop"},
	}, "willRetry": false})

	if s.scenario == "nosettle" || s.scenario == "slowstate" {
		// 永不收尾：只等 abort。slowstate 也要走这条路——否则一轮会在毫秒内
		// 正常 settled，看门狗根本没机会触发（它只在「安静太久」时才探活）。
		return
	}
	s.send(map[string]any{"type": "agent_settled"})
}

// dialogs 发两类 UI 请求：两个 dialog（阻塞等应答）与三个 fire-and-forget
// （**不应**应答）。
func (s *stub) dialogs() {
	s.send(map[string]any{"type": "extension_ui_request", "id": "ui-confirm-1", "method": "confirm",
		"title": "允许危险命令？", "message": "rm -rf /"})
	s.send(map[string]any{"type": "extension_ui_request", "id": "ui-select-1", "method": "select",
		"title": "选一个", "options": []any{"A", "B"}})
	s.send(map[string]any{"type": "extension_ui_request", "id": "ui-notify-1", "method": "notify",
		"message": "只是通知", "notifyType": "info"})
	s.send(map[string]any{"type": "extension_ui_request", "id": "ui-status-1", "method": "setStatus",
		"statusKey": "k", "statusText": "v"})
	s.send(map[string]any{"type": "extension_ui_request", "id": "ui-title-1", "method": "setTitle",
		"title": "t"})
	// 给客户端一点时间把应答写出来：真实 pi 在这里会阻塞等，stub 不阻塞，
	// 等一小会儿让断言有东西可看。
	time.Sleep(300 * time.Millisecond)
}
