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
