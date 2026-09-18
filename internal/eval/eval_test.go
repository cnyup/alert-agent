package eval

import (
	"context"
	"testing"

	einoengine "github.com/cnyup/alert-agent/internal/agent/eino"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

func mkCase(name, route string) Case {
	return Case{
		Name:  name,
		Alert: AlertSpec{Title: "t", Labels: coremodel.Labels{"a": "b"}, Severity: "warning"},
		Assert: AssertSpec{
			Route:            route,
			FirstExecPrefix:  []string{"tt-devops-cli", "databases", "query"},
			TraceNotContains: []string{"resources", "+search"},
			StepsLe:          15,
			NeedsHuman:       boolPtr(false),
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func TestRunRouteAssertions(t *testing.T) {
	cases := []Case{
		{Name: "命中", Alert: AlertSpec{Title: "x"}, Assert: AssertSpec{Route: "workflow-failed"}},
		{Name: "集合", Alert: AlertSpec{Title: "x"}, Assert: AssertSpec{RouteIn: []string{"generic", "workflow-failed"}}},
		{Name: "误路由", Alert: AlertSpec{Title: "x"}, Assert: AssertSpec{RouteNot: []string{"pg-conn-exhaust"}}},
		{Name: "不命中", Alert: AlertSpec{Title: "x"}, Assert: AssertSpec{Route: "workflow-failed"}},
	}
	rs := RunRoute(context.Background(), cases, func(context.Context, *coremodel.AlertEvent) string {
		return "workflow-failed" // 恒定路由结果：前四例的断言均应通过
	})
	for i := range rs {
		if !rs[i].Passed {
			t.Fatalf("第 %d 例应通过: %+v", i, rs[i])
		}
	}
	// 换一个必挂的：RouteNot 命中实际值
	bad := []Case{{Name: "必挂", Alert: AlertSpec{Title: "x"}, Assert: AssertSpec{RouteNot: []string{"workflow-failed"}}}}
	rs2 := RunRoute(context.Background(), bad, func(context.Context, *coremodel.AlertEvent) string { return "workflow-failed" })
	if rs2[0].Passed || len(rs2[0].Errors) != 1 {
		t.Fatalf("必挂例应失败且仅一条错误: %+v", rs2[0])
	}
}

type fakeEngine struct {
	routeName string
	report    *coremodel.DiagnosisReport
	evidence  []einoengine.Evidence
}

func (f *fakeEngine) Route(context.Context, *coremodel.AlertEvent) string { return f.routeName }
func (f *fakeEngine) Diagnose(context.Context, *coremodel.AlertEvent) (*coremodel.DiagnosisReport, []einoengine.Evidence, error) {
	return f.report, f.evidence, nil
}

func TestRunFullAssertions(t *testing.T) {
	rep := &coremodel.DiagnosisReport{
		Summary:  "s",
		Severity: "warning",
		RootCauses: []coremodel.RootCause{
			{Hypothesis: "h", Confidence: 0.9, Evidence: []string{"T3"}},
		},
		NeedsHuman: false,
		Cost:       coremodel.ReportCost{Steps: 20},
	}
	ev := []einoengine.Evidence{
		{ID: "T1", Tool: "read", Args: `{"path":"db_info/SKILL.md"}`, Result: "知识"},
		{ID: "T2", Tool: "exec", Args: `{"argv":["tt-devops-cli","whoami"]}`, Result: "ok"},
		{ID: "T3", Tool: "exec", Args: `{"argv":["tt-devops-cli","databases","query","+execute","--input","-"],"stdin":"{}"}`, Result: "rows"},
		{ID: "T4", Tool: "exec", Args: `{"argv":["tt-devops-cli","databases","resources","+search"]}`, Result: "instances"},
	}
	eng := &fakeEngine{routeName: "workflow-failed", report: rep, evidence: ev}

	rs := RunFull(context.Background(), []Case{mkCase("全断言", "workflow-failed")}, eng)
	// mkCase 断言：StepsLe=15 但实际 20（预算回归应挂）；FirstExecPrefix 期望 databases query
	// 但首个 exec 是 whoami（应挂）；evidence T3 是 exec 步（通过）
	if rs[0].Passed {
		t.Fatal("步数超限应失败")
	}
	joined := ""
	for _, e := range rs[0].Errors {
		joined += e + ";"
	}
	for _, want := range []string{"步数", "首个 exec"} {
		if !containsStr(joined, want) {
			t.Fatalf("缺少断言错误 %q: %s", want, joined)
		}
	}

	// 放宽到合理基线后应通过（trace 里有 resources 探测步，去掉 NotContains）
	c := mkCase("放宽", "workflow-failed")
	c.Assert.StepsLe = 25
	c.Assert.FirstExecPrefix = []string{"tt-devops-cli", "whoami"}
	c.Assert.TraceNotContains = nil
	rs = RunFull(context.Background(), []Case{c}, eng)
	if !rs[0].Passed {
		t.Fatalf("放宽后应通过: %+v", rs[0])
	}

	// 反臆造：证据引用 read 步应挂
	c2 := mkCase("反臆造", "workflow-failed")
	c2.Assert.StepsLe = 25
	c2.Assert.FirstExecPrefix = []string{"tt-devops-cli", "whoami"}
	c2.Assert.TraceNotContains = nil
	c2.Assert.EvidenceExecOnly = true
	rep2 := *rep
	rep2.RootCauses = []coremodel.RootCause{{Hypothesis: "h", Confidence: 0.9, Evidence: []string{"T1"}}} // read 步
	eng2 := &fakeEngine{routeName: "workflow-failed", report: &rep2, evidence: ev}
	rs = RunFull(context.Background(), []Case{c2}, eng2)
	if rs[0].Passed || !containsStr(joinErrs(rs[0].Errors), "未绑定 exec") {
		t.Fatalf("反臆造断言未生效: %+v", rs[0])
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func joinErrs(es []string) string {
	out := ""
	for _, e := range es {
		out += e + ";"
	}
	return out
}
