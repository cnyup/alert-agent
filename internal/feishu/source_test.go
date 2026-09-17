package feishu

import "testing"

func TestCommandParsing(t *testing.T) {
	cases := []struct {
		in    string
		isCmd bool
		typ   string
		act   string
	}{
		{"批准 A1", true, "approve", "A1"},
		{"批准", true, "approve", "A1"},       // 缺省动作号回退 A1
		{"approve a2", true, "approve", "A2"}, // 大小写归一
		{"拒绝 A1", true, "reject", "A1"},
		{"认领", true, "claim", ""},
		{"误报", true, "false-positive", ""},
		{"根因确认", true, "root-confirmed", ""},
		{"确认根因", true, "root-confirmed", ""},
		{"服务器炸了快看看", false, "", ""},
		{"批准一下那个发布", false, "", ""}, // 模糊文本不算指令（要求精确格式）
	}
	for _, c := range cases {
		if got := isCommand(c.in); got != c.isCmd {
			t.Errorf("isCommand(%q) = %v", c.in, got)
			continue
		}
		if !c.isCmd {
			continue
		}
		// 类型与动作号解析
		typ, act := "", ""
		switch {
		case reApprove.MatchString(c.in):
			typ = "approve"
			act = actionID(c.in, "A1")
		case reReject.MatchString(c.in):
			typ = "reject"
			act = actionID(c.in, "A1")
		case reClaim.MatchString(c.in):
			typ = "claim"
		case reFalsePos.MatchString(c.in):
			typ = "false-positive"
		case reConfirm.MatchString(c.in):
			typ = "root-confirmed"
		}
		if typ != c.typ || act != c.act {
			t.Errorf("parse(%q) = %s/%s, want %s/%s", c.in, typ, act, c.typ, c.act)
		}
	}
}

func TestExtractText(t *testing.T) {
	if got := extractText(`{"text":"@_user_1 订单服务 5xx 飙升"}`); got != "订单服务 5xx 飙升" {
		t.Fatalf("去 @占位失败: %q", got)
	}
	if got := extractText("裸文本"); got != "裸文本" {
		t.Fatalf("非 JSON 容错失败: %q", got)
	}
}

func TestExtractQuotedAlert(t *testing.T) {
	// text
	title, desc := extractQuotedAlert("text", `{"text":"[FIRING] CPU 使用率 92%"}`)
	if title != "[FIRING] CPU 使用率 92%" || desc != title {
		t.Fatalf("text 解析失败: %q", title)
	}
	// interactive 卡片：标题 + 展平文本作描述
	title2, desc2 := extractQuotedAlert("interactive", `{"title":{"content":"🔴 严重告警"},"elements":[{"text":{"content":"CPU 92%","tag":"lark_md"}}]}`)
	if title2 != "🔴 严重告警" {
		t.Fatalf("卡片标题提取失败: %q", title2)
	}
	if !contains(desc2, "CPU 92%") {
		t.Fatalf("卡片文本应进描述: %q", desc2)
	}
	// post 富文本
	title3, _ := extractQuotedAlert("post", `{"content":[[{"tag":"text","text":"磁盘告警"},{"tag":"text","text":"：使用率95%"}]]}`)
	if !contains(title3, "磁盘告警") || !contains(title3, "95%") {
		t.Fatalf("post 解析失败: %q", title3)
	}
	// 非标准形态：title 是纯字符串，elements 是嵌套数组分段文本
	nonStdCard := `{"title":"恢复 - 生产-AVL短剧出海-整剧工作流失败数>=1","elements":[[{"tag":"text","text":"🎰 对象类型："},{"tag":"text","text":"\n业务自定义指标"},{"tag":"text","text":"🔢 告警级别："}],[{"tag":"text","text":"📋告警内容："},{"tag":"text","text":"\n工作流：原剧边传边处理工作流: 失败数为1 个"}]]}`
	title4, desc4 := extractQuotedAlert("interactive", nonStdCard)
	if title4 != "恢复 - 生产-AVL短剧出海-整剧工作流失败数>=1" {
		t.Fatalf("字符串 title 应被提取: %q", title4)
	}
	if !contains(desc4, "业务自定义指标") || !contains(desc4, "失败数为1") || contains(desc4, `{"tag"`) {
		t.Fatalf("非标准卡 elements 应展平为可读文本: %q", desc4)
	}
	// title 缺失回落兜底标题
	title5, _ := extractQuotedAlert("interactive", `{"elements":[]}`)
	if title5 != "飞书引用卡片告警" {
		t.Fatalf("缺 title 应回落兜底: %q", title5)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
