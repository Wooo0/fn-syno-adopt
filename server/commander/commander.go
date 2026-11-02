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
	// 内核日志：只用于把 mount 失败翻译成可读原因（见 kernel_log.go）
	"dmesg":      true,
	"journalctl": true,

	// —— 写阶段（接管/修复/清理）——
	"vgrename":    true,
	"lvrename":    true,
	"vgchange":    true,
	"vgcfgbackup": true,
	// udevadm 仅用于 settle：改名会让 LVM 重建 dm 设备、符号链接由 udev 异步建立，
	// 不等它 settle 就 mount 会拿到陈旧/别人的 dm（真机 2026-10-31 事故）
	"udevadm":  true,
	"modprobe": true,
	"mount":    true,
	"umount":   true,
	"chown":    true,
	// chmod 仅用于所有权修复的「补权限位」：把群晖 synoacl 遗留的 000 位
	// 补成 755/644。形态在 validateArgs 里写死为 -R u+rwX,a+rX <path>。
	// 注意：漏登记这一个会让整个修复流程在最后一步必然失败（真机 2026-10-31 踩到）
	"chmod":   true,
	"setfacl": true,
	"pg_dump": true,
	"psql":    true,
	"du":      true,
	"df":      true,
	"find":    true,
	"mv":      true,
	// cp 仅用于「共享文件夹跨子卷迁移」：rename(2) 跨子卷必返 EXDEV，
	// 只能退化成 reflink 克隆 + 删源。形态在 validateArgs 里写死。
	"cp": true,
	// dm-linear 包装（直建 btrfs 接管）：仅允许 info/create/remove 三个子命令
	"dmsetup": true,
	// 开机自动包装 unit 的启用/停用：仅允许固定动词
	"systemctl": true,
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
	case "dmesg":
		// 只读内核环形缓冲，不接受任何参数（避免 -C 清空、-n 之外的花样）
		if len(args) != 0 {
			return fmt.Errorf("commander: dmesg 不允许带参数，收到: %s", strings.Join(args, " "))
		}
	case "chmod":
		// 只放行「补权限位」这一个形态：chmod -R u+rwX,a+rX <绝对路径>
		// 策略本身是只加位不删位；写成白名单是因为 chmod 一旦放开就等于给了
		// 改任意文件权限（含 setuid）的能力。
		if len(args) != 3 || args[0] != "-R" || args[1] != "u+rwX,a+rX" {
			return fmt.Errorf("commander: chmod 仅允许 'chmod -R u+rwX,a+rX <path>'，收到: %s %s",
				name, strings.Join(args, " "))
		}
		if !strings.HasPrefix(args[2], "/") {
			return fmt.Errorf("commander: chmod 的目标必须是绝对路径，收到: %s", args[2])
		}
	case "cp":
		// 只放行一个形态：cp -a --reflink=auto -- <src> <dst>
		// 不给任何自由度——不能让上层借 cp 做任意目录操作。
		if len(args) != 5 || args[0] != "-a" || args[1] != "--reflink=auto" || args[2] != "--" {
			return fmt.Errorf("commander: cp 仅允许 'cp -a --reflink=auto -- <src> <dst>'，收到: %s %s",
				name, strings.Join(args, " "))
		}
		if !strings.HasPrefix(args[3], "/") || !strings.HasPrefix(args[4], "/") {
			return fmt.Errorf("commander: cp 的源与目标必须是绝对路径，收到: %s → %s", args[3], args[4])
		}
	case "udevadm":
		// 只允许 settle（可带 --timeout=N）：不能让上层借 udevadm 做别的
		if len(args) == 0 || args[0] != "settle" {
			return fmt.Errorf("commander: udevadm 仅允许 settle，收到: %s", strings.Join(args, " "))
		}
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "--timeout=") ||
				!allDigits(strings.TrimPrefix(a, "--timeout=")) {
				return fmt.Errorf("commander: udevadm settle 仅允许 --timeout=N，收到: %s", a)
			}
		}
	case "journalctl":
		// 只允许 "journalctl -k -n <N> --no-pager -o cat"：读内核消息、限行数、不落 pager
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-k", "--no-pager", "-o":
				if args[i] == "-o" {
					if i+1 >= len(args) || args[i+1] != "cat" {
						return fmt.Errorf("commander: journalctl -o 仅允许 cat")
					}
					i++
				}
			case "-n":
				if i+1 >= len(args) || !allDigits(args[i+1]) {
					return fmt.Errorf("commander: journalctl -n 需要数字行数")
				}
				i++
			default:
				return fmt.Errorf("commander: journalctl 仅允许 -k/-n/--no-pager/-o cat，收到: %s",
					strings.Join(args, " "))
			}
		}
	case "dmsetup":
		if len(args) == 0 {
			return fmt.Errorf("commander: dmsetup 需要子命令")
		}
		switch args[0] {
		case "info", "create", "remove":
			// 仅放行直建包装用到的三个子命令
		default:
			return fmt.Errorf("commander: dmsetup 仅允许 info/create/remove，收到: %s",
				strings.Join(args, " "))
		}
	case "systemctl":
		if len(args) == 0 {
			return fmt.Errorf("commander: systemctl 需要子命令")
		}
		switch args[0] {
		case "daemon-reload":
		case "enable", "disable", "start", "stop":
			// 其余参数只允许标志（--now 等）或本应用的 unit 文件名
			for _, a := range args[1:] {
				if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "fn-syno-adopt-") {
					return fmt.Errorf("commander: systemctl %s 仅允许标志或 fn-syno-adopt-* unit，收到: %s",
						args[0], a)
				}
			}
		default:
			return fmt.Errorf("commander: systemctl 仅允许 daemon-reload/enable/disable/start/stop，收到: %s",
				strings.Join(args, " "))
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
