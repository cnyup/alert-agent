package execenv

import (
	"context"
	"strings"
	"testing"

	"github.com/cnyup/alert-agent/internal/config"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

func riskCfg() config.ToolsConfig {
	var c config.ToolsConfig
	c.Exec.Enabled = true
	c.Exec.Backend = "local"
	c.Exec.Allow = []string{"echo", "cat"}
	c.Exec.Readonly = map[string][]string{
		"tt-devops-cli": {
			"databases query",
			"databases +doctor",
			"alerts +list-rules",
		},
		"echo": {"hi"},
	}
	c.Exec.Timeout = "5s"
	c.Exec.MaxOutputBytes = 4096
	return c
}

func TestClassify(t *testing.T) {
	ro := riskCfg().Exec.Readonly
	cases := []struct {
		argv []string
		want coremodel.ToolRisk
	}{
		{[]string{"tt-devops-cli", "databases", "query", "+validate", "--input", "-"}, coremodel.RiskReadOnly},
		{[]string{"tt-devops-cli", "databases", "+doctor"}, coremodel.RiskReadOnly},
		{[]string{"tt-devops-cli", "alerts", "+list-rules", "--data", "{}"}, coremodel.RiskReadOnly},
		// 词对齐：queryx 不等于 query
		{[]string{"tt-devops-cli", "databases", "queryx"}, coremodel.RiskMutating},
		// 前缀更长的只读不覆盖兄弟命令
		{[]string{"tt-devops-cli", "databases", "tickets", "+change-create"}, coremodel.RiskMutating},
		{[]string{"tt-devops-cli", "alerts", "+create"}, coremodel.RiskMutating},
		// 未声明任何只读前缀的二进制：默认 mutating
		{[]string{"tt-devops-cli", "whoami"}, coremodel.RiskMutating},
		// 裸二进制（无子命令）也默认 mutating
		{[]string{"tt-devops-cli"}, coremodel.RiskMutating},
		{[]string{}, coremodel.RiskMutating},
		// 其它 bin 的前缀独立判定
		{[]string{"echo", "hi"}, coremodel.RiskReadOnly},
		{[]string{"echo", "rm -rf /"}, coremodel.RiskMutating},
	}
	for _, c := range cases {
		if got := classify(ro, c.argv); got != c.want {
			t.Errorf("classify(%v) = %s, want %s", c.argv, got, c.want)
		}
	}
}

// 排查环境拒绝变更命令（软失败并引导走审批），全量环境放行。
func TestReadOnlyGate(t *testing.T) {
	f, err := NewFactory(riskCfg())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	diag, _ := f.Acquire(ctx)
	res, err := diag.Run(ctx, []string{"echo", "hi"}, "")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("只读命令应放行: err=%v res=%+v", err, res)
	}
	// echo 未声明的子命令 → mutating → 排查期拒绝
	res, err = diag.Run(ctx, []string{"echo", "danger"}, "")
	if err != nil || res.ExitCode != -2 || !strings.Contains(res.Stderr, "审批") {
		t.Fatalf("变更命令应被只读门禁软拒绝: err=%v res=%+v", err, res)
	}

	full, _ := f.AcquireFull(ctx)
	res, err = full.Run(ctx, []string{"echo", "danger"}, "")
	if err != nil || res.ExitCode != 0 || res.Stdout != "danger\n" {
		t.Fatalf("全量环境应放行: err=%v res=%+v", err, res)
	}

	if f.RiskOf([]string{"echo", "hi"}) != coremodel.RiskReadOnly {
		t.Fatal("RiskOf 应与 classify 一致")
	}
}
