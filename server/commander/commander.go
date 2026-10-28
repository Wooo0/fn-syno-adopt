// Package commander 是所有系统命令调用的唯一出口。
// 任何外部命令都必须在这里登记白名单并经 Run() 执行，保证：
//  1. 可审计：全部命令调用集中一处
//  2. 可演练：DRYRUN 模式只打印将要执行的命令，绝不真实执行
//  3. 不可注入：参数以 argv 形式传递，用户输入永远不拼接进 shell 字符串
package commander

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result 保存一次命令执行的完整信息。
type Result struct {
	Command  string // 实际执行的完整命令行
	Stdout   string
	Stderr   string
	ExitCode int
}

// allowedBinaries 外部命令白名单。新增命令必须先在这里登记。
var allowedBinaries = map[string]bool{
	// —— 只读检测 ——
	"lsblk": true,
	"blkid": true,
	"mdadm": true,
	"pvs":   true,
	"vgs":   true,
	"lvs":   true,
	"btrfs": true,
	"uname": true,

	// —— 写阶段（接管/修复/清理）——
	"vgrename":    true,
	"lvrename":    true,
	"vgchange":    true,
	"vgcfgbackup": true,
	"modprobe":    true,
	"mount":       true,
	"umount":      true,
	"chown":       true,
	"setfacl":     true,
	"pg_dump":     true,
	"psql":        true,
	"du":          true,
	"find":        true,
	"mv":          true,
	// sudo 仅允许固定形态：sudo -u postgres psql/pg_dump ...（飞牛 PostgreSQL 本机 peer 认证）
	"sudo": true,
	// runuser 为 sudo 缺失时的兜底（Debian 自带）：仅放行 runuser -u postgres -- psql|pg_dump ...
	"runuser": true,
	// 注意：rm 永不加入白名单。@eaDir 清理采用"移入回收站目录"设计，
	// 最终删除由用户在飞牛文件管理中手动完成，应用绝不直接删数据。
}

// validateArgs 命令级参数校验：白名单之外的第二道闸。
// 返回 error 表示该次调用形态不被允许。
func validateArgs(name string, args []string) error {
	switch name {
	case "sudo":
		if len(args) < 3 || args[0] != "-u" || args[1] != "postgres" ||
			(args[2] != "psql" && args[2] != "pg_dump") {
			return fmt.Errorf("commander: sudo 仅允许 'sudo -u postgres psql|pg_dump ...' 形态，收到: %s %s",
				name, strings.Join(args, " "))
		}
	case "runuser":
		if len(args) < 5 || args[0] != "-u" || args[1] != "postgres" || args[2] != "--" ||
			(args[3] != "psql" && args[3] != "pg_dump") {
			return fmt.Errorf("commander: runuser 仅允许 'runuser -u postgres -- psql|pg_dump ...' 形态，收到: %s %s",
				name, strings.Join(args, " "))
		}
	}
	return nil
}

var dryRun = os.Getenv("SYNOKIT_DRYRUN") == "1"

// RunStream 流式执行：逐行回调 stdout，不在内存中积累全部输出。
// 适用于 find/du 等可能输出百万行的扫描命令。DRYRUN 模式不执行、不回调。
func RunStream(timeout time.Duration, name string, onLine func(line string), args ...string) (Result, error) {
	if !allowedBinaries[name] {
		return Result{}, fmt.Errorf("commander: 命令不在白名单内: %s", name)
	}
	if err := validateArgs(name, args); err != nil {
		return Result{}, err
	}
	full := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if dryRun {
		return Result{Command: full, ExitCode: 0}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Command: full}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Result{Command: full}, fmt.Errorf("commander: %s 启动失败: %w", name, err)
	}
	buf := make([]byte, 0, 64*1024)
	var pending []byte
	for {
		n, rerr := out.Read(buf[:cap(buf)])
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				i := strings.IndexByte(string(pending), '\n')
				if i < 0 {
					break
				}
				onLine(string(pending[:i]))
				pending = pending[i+1:]
			}
			if len(pending) > 1024*1024 { // 超长行保护
				onLine(string(pending))
				pending = nil
			}
		}
		if rerr != nil {
			if len(pending) > 0 {
				onLine(string(pending))
			}
			break
		}
	}
	_ = cmd.Wait()
	res := Result{Command: full, Stderr: stderr.String()}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		res.ExitCode = code
		return res, fmt.Errorf("commander: %s 执行失败（退出码 %d）", name, code)
	}
	return res, nil
}

// RunPostgres 以 postgres 身份执行 psql / pg_dump。
//
// 优先 sudo -u postgres；若失败原因是 sudo 本身不存在（退出码 127 或 stderr 提示
// 未找到），自动降级到 Debian 自带的 runuser -u postgres -- ...。
// 两种形态都在白名单里固定，参数仍以 argv 传递。
func RunPostgres(timeout time.Duration, sub string, args ...string) (Result, error) {
	r, err := Run(timeout, "sudo", append([]string{"-u", "postgres", sub}, args...)...)
	if err != nil && missingBinary(r) {
		return Run(timeout, "runuser", append([]string{"-u", "postgres", "--", sub}, args...)...)
	}
	return r, err
}

// missingBinary 判断失败是否源于 sudo 不存在（而非 SQL 本身出错）。
// 只有"命令找不到"才兜底，避免把真正的 SQL 错误再跑一遍。
func missingBinary(r Result) bool {
	if r.ExitCode == 127 {
		return true
	}
	s := strings.ToLower(r.Stderr)
	// 必须点名 sudo，避免把真正的 SQL 错误（如库/表不存在）误判成"命令缺失"再跑一遍
	if !strings.Contains(s, "sudo") {
		return false
	}
	return strings.Contains(s, "command not found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "no such file or directory")
}

// ManualPostgres 返回等价的手工命令文本（写进失败日志，供用户复制排障）。
func ManualPostgres(sub string, args ...string) string {
	return "sudo -u postgres " + sub + " " + strings.Join(args, " ")
}

// IsDryRun 返回是否处于演练模式（SYNOKIT_DRYRUN=1）。
func IsDryRun() bool { return dryRun }

// Run 执行白名单内的命令。DRYRUN 模式下只返回将要执行的命令行。
func Run(timeout time.Duration, name string, args ...string) (Result, error) {
	if !allowedBinaries[name] {
		return Result{}, fmt.Errorf("commander: 命令不在白名单内: %s", name)
	}
	if err := validateArgs(name, args); err != nil {
		return Result{}, err
	}
	full := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if dryRun {
		return Result{Command: full, ExitCode: 0}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{
		Command: full,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		}
		return res, fmt.Errorf("commander: %s 执行失败: %w", name, err)
	}
	return res, nil
}
