package capability

import (
	"os"
	"strings"
	"testing"
)

func TestParseConfigTree(t *testing.T) {
	raw, err := os.ReadFile("testdata/nginx_T_dump.txt")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	files, err := ParseConfigTree(string(raw))
	if err != nil {
		t.Fatalf("ParseConfigTree: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4; paths=%v", len(files), pathsOf(files))
	}
	wantPaths := []string{
		"/etc/nginx/nginx.conf",
		"/etc/nginx/mime.types",
		"/etc/nginx/conf.d/default.conf",
		"/etc/nginx/conf.d/api.conf",
	}
	for i, p := range wantPaths {
		if files[i].Path != p {
			t.Errorf("files[%d].Path = %q, want %q", i, files[i].Path, p)
		}
		if files[i].Content == "" {
			t.Errorf("files[%d].Content empty", i)
		}
		if files[i].SHA256 == "" {
			t.Errorf("files[%d].SHA256 empty", i)
		}
		// 内容不得串入下一个文件的边界标记
		if strings.Contains(files[i].Content, "# configuration file ") {
			t.Errorf("files[%d].Content bleeds into next marker", i)
		}
	}

	// 同内容哈希稳定：两次解析首个文件哈希一致
	files2, err := ParseConfigTree(string(raw))
	if err != nil {
		t.Fatalf("ParseConfigTree(2): %v", err)
	}
	if files[0].SHA256 != files2[0].SHA256 {
		t.Errorf("SHA256 not stable across parses")
	}
	// 主配置应已 include conf.d（内容里含 include 行，证明未被截断）
	if !strings.Contains(files[0].Content, "include /etc/nginx/conf.d/*.conf") {
		t.Errorf("main config truncated: missing include line")
	}
}

func TestParseConfigTreeEdge(t *testing.T) {
	// 空输出 → 报错
	if _, err := ParseConfigTree(""); err == nil {
		t.Errorf("expected error for empty dump")
	}
	// 无边界标记 → 报错
	if _, err := ParseConfigTree("nginx: configuration file /etc/nginx/nginx.conf test is successful\nuser nginx;\n"); err == nil {
		t.Errorf("expected error when no marker present")
	}
	// 路径含空格 → 正常解析
	dump := "# configuration file /opt/my conf/nginx.conf:\n\nuser nginx;\nworker_processes 1;\n"
	files, err := ParseConfigTree(dump)
	if err != nil {
		t.Fatalf("ParseConfigTree space-path: %v", err)
	}
	if len(files) != 1 || files[0].Path != "/opt/my conf/nginx.conf" {
		t.Errorf("space-in-path not handled: got %+v", files)
	}
	// 单个文件 → 正常
	single := "# configuration file /etc/nginx/nginx.conf:\n\nevents {}\nhttp {}\n"
	files, err = ParseConfigTree(single)
	if err != nil || len(files) != 1 {
		t.Errorf("single-file case failed: files=%v err=%v", files, err)
	}
}

func pathsOf(fs []ConfigFile) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return out
}
