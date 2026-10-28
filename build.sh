#!/usr/bin/env bash
# 一键构建：前端 -> Go 后端(交叉编译 linux/amd64) -> FPK
# 依赖: Node.js >= 18, Go >= 1.22, fnpack (https://developer.fnnas.com/docs/cli/fnpack/)
# Windows 用户请在 WSL / Git Bash 中执行。
set -euo pipefail
cd "$(dirname "$0")"

echo "==> [0/5] 门禁：Go 格式与静态检查"
if command -v gofmt >/dev/null 2>&1; then
  BAD="$(gofmt -l server)"
  if [ -n "$BAD" ]; then
    echo "以下 Go 文件未格式化（请先执行 gofmt -w server）："
    echo "$BAD"
    exit 1
  fi
fi
(cd server && go vet ./... && go test ./... ) || { echo "go vet / go test 未通过"; exit 1; }

echo "==> [1/5] 前端类型检查（vue-tsc，缺失则跳过）"
if (cd frontend && npm run type-check); then
  echo "类型检查通过"
else
  echo "警告：前端类型检查未通过或未安装 vue-tsc，继续构建"
fi

echo "==> [2/5] 构建前端 (Vue3 + Vite)"
(cd frontend && npm install && npm run build)

echo "==> [3/5] 同步前端产物到 Go 内嵌目录"
rm -rf server/web/dist
mkdir -p server/web/dist
cp -r frontend/dist/. server/web/dist/

echo "==> [4/5] 交叉编译 Go 后端 (linux/amd64)"
(cd server && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../app/server/fn-syno-adopt .)

chmod +x cmd/main

echo "==> [5/5] 打包 FPK"
# 优先使用项目内自带的 fnpack（tools/fnpack.exe 或 tools/fnpack），否则用 PATH 里的
if [ -x "tools/fnpack.exe" ]; then FNPACK="tools/fnpack.exe"; elif [ -x "tools/fnpack" ]; then FNPACK="tools/fnpack"; else FNPACK="fnpack"; fi
"$FNPACK" build

echo ""
echo "完成。安装测试：appcenter-cli install-local <生成的>.fpk"
echo "（fpk 产物在当前目录，或按 fnpack 输出路径为准）"
