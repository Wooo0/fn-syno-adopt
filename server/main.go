// fn-syno-adopt 后端入口。
//
// 两种运行模式（自动判断）：
//   - 统一网关模式（飞牛应用内安装时）：监听 ${TRIM_APPDEST}/app.sock unix socket，
//     由飞牛网关校验登录态后转发 /app/fn-syno-adopt/... 请求。不占 TCP 端口、
//     不直接暴露到局域网、复用飞牛登录鉴权。
//   - 开发模式（无 TRIM_APPDEST 环境）：监听 TCP :18099，供本地调试。
//
// 安全设计：检测阶段全部操作只读；所有系统命令经 commander 白名单执行。
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"fnos-syno-adopt/api"
	"fnos-syno-adopt/commander"
)

const appName = "fn-syno-adopt"

//go:embed all:web/dist
var distFS embed.FS

func main() {
	port := flag.String("port", "18099", "HTTP 监听端口（仅开发模式）")
	flag.Parse()

	mux := http.NewServeMux()
	api.Register(mux)

	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		log.Fatalf("内嵌前端资源不可用: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// 统一网关把带前缀的请求转发进来（/app/fn-syno-adopt/xxx），
	// 这里剥掉前缀后再路由；前端资源与 API 全部使用相对路径适配。
	root := stripGatewayPrefix(mux, "/app/"+appName)

	if sock := socketPath(); sock != "" {
		listenUnix(sock, root)
		return
	}

	addr := ":" + *port
	log.Printf("fn-syno-adopt 开发模式启动: http://0.0.0.0%s (DRYRUN=%v)", addr, commander.IsDryRun())
	if err := http.ListenAndServe(addr, root); err != nil {
		log.Fatal(err)
	}
}

// socketPath 返回 unix socket 路径；非飞牛应用环境（无 TRIM_APPDEST）返回空。
func socketPath() string {
	dest := os.Getenv("TRIM_APPDEST")
	if dest == "" {
		return ""
	}
	return dest + "/app.sock"
}

// listenUnix 在 unix socket 上服务，权限 0770（飞牛网关账号需可读写）。
func listenUnix(sock string, root http.Handler) {
	_ = os.Remove(sock) // 清理残留 socket 文件
	ln, err := net.Listen("unix", sock)
	if err != nil {
		log.Fatalf("监听 unix socket 失败(%s): %v", sock, err)
	}
	if err := os.Chmod(sock, 0o770); err != nil {
		log.Printf("警告: socket 权限设置失败: %v", err)
	}
	log.Printf("fn-syno-adopt 统一网关模式启动: %s (DRYRUN=%v)", sock, commander.IsDryRun())
	if err := http.Serve(ln, root); err != nil {
		log.Fatal(err)
	}
}

// stripGatewayPrefix 剥离统一网关前缀；无斜杠根路径重定向补齐，
// 保证前端相对路径（./assets/... 、api/...）解析正确。
func stripGatewayPrefix(next http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == prefix:
			http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
			return
		case strings.HasPrefix(p, prefix+"/"):
			r.URL.Path = strings.TrimPrefix(p, prefix)
		}
		next.ServeHTTP(w, r)
	})
}
