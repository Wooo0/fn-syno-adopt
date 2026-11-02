package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEmbeddedDistIsConsistent 校验 go:embed 的 web/dist 是否自洽。
//
// 为什么需要这条测试：dist/index.html 里写死了 assets 的内容 hash 文件名
// （如 ./assets/index-DDUFU0rI.js）。index.html 与 assets 必须严格配套——
// 三者中任一落后一步，go build 出来的二进制打开就是白屏，而本地磁盘上文件都在，
// 看起来一切正常，只有别人 clone 之后才会炸。这个坑真机上已经踩过一次：
// 旧 hash 的 assets 被删、index.html 已指向新 hash、新 assets 忘了提交。
//
// 把它做成测试而不是靠记性，是因为 build.sh 的 [0/5] 门禁与 CI 都会跑 go test，
// 单独执行 go test ./... 同样能拦住，不依赖谁记得去跑构建脚本。
func TestEmbeddedDistIsConsistent(t *testing.T) {
	const distDir = "web/dist"
	idx := filepath.Join(distDir, "index.html")

	raw, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("读不到内嵌首页 %s：%v\n"+
			"  该文件必须入库，否则 go:embed 打不出可运行的二进制。", idx, err)
	}

	refs := localAssetRefs(string(raw))
	if len(refs) == 0 {
		// 自包含占位页：不引用任何外部文件，天然自洽。
		return
	}

	for _, ref := range refs {
		p := filepath.Join(distDir, filepath.FromSlash(ref))
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			t.Errorf("index.html 引用了 %s，但该文件不存在。\n"+
				"  这就是「clone 后打开白屏」的原因：hash 文件名换了，文件没跟着提交。\n"+
				"  修法：前端跑 npm run build，把 frontend/dist 整体同步到 server/web/dist，"+
				"或直接跑 ./build.sh（它会 rm -rf 后重建）；提交时 index.html 与 assets 必须一起进。",
				ref)
		}
	}

	// 反向检查：assets 里存在但 index.html 没引用的文件 = 旧构建残留。
	// 只提示不失败——将来若启用代码分割，动态加载的 chunk 本来就不会出现在 index.html 里，
	// 拿它当错误会让正常构建误报。
	refSet := make(map[string]bool, len(refs))
	for _, r := range refs {
		refSet[r] = true
	}
	entries, err := os.ReadDir(filepath.Join(distDir, "assets"))
	if err != nil {
		return // 没有 assets 目录也算自洽（自包含占位页）
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !refSet["assets/"+e.Name()] {
			t.Logf("提示：%s/assets/%s 未被 index.html 引用，疑似旧构建残留（删除不影响构建）",
				distDir, e.Name())
		}
	}
}

// assetRefRE 匹配 HTML 里 src / href 属性的取值。
var assetRefRE = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// localAssetRefs 提取指向本地文件的引用，跳过外链、协议相对地址与内联数据。
func localAssetRefs(html string) []string {
	var out []string
	for _, m := range assetRefRE.FindAllStringSubmatch(html, -1) {
		ref := m[1]
		switch {
		case ref == "",
			strings.HasPrefix(ref, "http://"),
			strings.HasPrefix(ref, "https://"),
			strings.HasPrefix(ref, "//"),
			strings.HasPrefix(ref, "data:"),
			strings.HasPrefix(ref, "#"):
			continue
		}
		// vite 用相对路径（base: './'），统一去掉前导 ./ 与 / 再比对磁盘路径
		ref = strings.TrimPrefix(ref, "./")
		ref = strings.TrimPrefix(ref, "/")
		if ref == "" || strings.HasSuffix(ref, "/") {
			continue
		}
		out = append(out, ref)
	}
	return out
}
