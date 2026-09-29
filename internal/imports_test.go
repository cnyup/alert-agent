// imports 守卫测试（PLAN Wave 3.4）：固化 eino 类型边界——
// internal/{caseflow,store,webhook,feishu,pipeline,policy,notifier,dispatch,
// config,metrics,eval,skills,inflight,fanout} 与 cmd/ 禁止 import eino。
// 白名单：internal/agent、internal/agent/eino、internal/llm、internal/mcp、internal/execenv
//（后三者向 Investigator 抽象收敛后逐步从白名单移除）。
package imports_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const einoPkg = "github.com/cloudwego/eino"

var allowEino = map[string]bool{
	"internal/agent":        true,
	"internal/agent/eino":   true,
	"internal/llm":          true,
	"internal/mcp":          true,
	"internal/execenv":      true,
}

func TestNoEinoImportsOutsideAllowlist(t *testing.T) {
	// 测试工作目录在 internal/，仓库根是其父目录
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	violations := scanDir(t, root, filepath.Join(root, "internal"))
	violations = append(violations, scanDir(t, root, filepath.Join(root, "cmd"))...)
	if len(violations) > 0 {
		t.Errorf("eino import 越界（%d 处）：\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// scanDir 递归扫描目录下所有非 _test.go 文件的 import。
func scanDir(t *testing.T, root, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if err != nil {
				return err
			}
			if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // 非本测试职责
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		if allowEino[rel] {
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(ip, einoPkg) {
				out = append(out, rel+": "+ip+"（"+filepath.Base(path)+"）")
			}
		}
		return nil
	})
	return out
}
