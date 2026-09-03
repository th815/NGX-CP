package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// ConfigFile 是 `nginx -T` 解析出的单个配置文件（含内容、哈希、大小）。
// 注意：切勿读取 ssl_certificate_key 指向的文件内容，仅记录路径（见 T018 日志探测）。
type ConfigFile struct {
	Path    string `json:"path"`    // /etc/nginx/nginx.conf
	Content string `json:"content"` // 去首尾空行后的文件原文
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// reConfFile 匹配 nginx -T 输出的文件边界标记：
//
//	# configuration file /etc/nginx/nginx.conf:
var reConfFile = regexp.MustCompile(`(?m)^# configuration file (.+):$`)

// ParseConfigTree 从 `nginx -T` 完整输出解析配置树。
// -T 输出到 stdout；配置有语法错误时 -T 会失败（这是特性，用于检出坏配置，见 T017 陷阱）。
// 返回的每个文件内容严格落在两个边界标记之间，不会串入下一个文件。
func ParseConfigTree(dump string) ([]ConfigFile, error) {
	matches := reConfFile.FindAllStringSubmatchIndex(dump, -1)
	if len(matches) == 0 {
		return nil, errors.New("未找到配置文件边界标记，可能 nginx 版本不支持 -T 或输出不完整")
	}
	files := make([]ConfigFile, 0, len(matches))
	for i, m := range matches {
		path := strings.TrimSpace(dump[m[2]:m[3]])
		start := m[1] // 标记行结束位置
		end := len(dump)
		if i+1 < len(matches) {
			end = matches[i+1][0] // 下一个标记行起始位置
		}
		content := strings.TrimSpace(dump[start:end])
		sum := sha256.Sum256([]byte(content))
		files = append(files, ConfigFile{
			Path:    path,
			Content: content,
			SHA256:  hex.EncodeToString(sum[:]),
			Size:    int64(len(content)),
		})
	}
	return files, nil
}
