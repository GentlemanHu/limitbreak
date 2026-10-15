package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"quota-session-orchestrator/internal/adapters"
	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/host"
	"quota-session-orchestrator/internal/scheduler"
	"quota-session-orchestrator/internal/server"
	"quota-session-orchestrator/internal/signals"
	"quota-session-orchestrator/internal/store"
)

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	cmd := args[0]
	if cmd == "mcp" {
		return runMCP(args[1:])
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	path := f.String("config", "runtime/config.json", "私有配置文件")
	listen := f.String("listen", "", "仅 run：覆盖 loopback 监听地址")
	startupPaused := f.Bool("startup-paused", false, "仅 run：启动前暂停持久策略；保留其他设置")
	engineVersion := f.String("engine-version", "", "生成 Hooks 时提供已确认的 Desktop 内嵌 Code 版本")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	switch cmd {
	case "validate":
		// Decode/validate only: no secret resolution, database, network or bridge.
		if _, err := config.Load(*path); err != nil {
			return err
		}
		fmt.Println("配置有效")
		return nil
	case "init":
		return initialize(*path)
	case "set-key":
		return setKey(*path)
	case "run":
		return runDaemon(*path, *listen, *startupPaused)
	case "hook":
		return forwardHook(*path)
	case "hooks-config":
		return hookSettings(*path, *engineVersion)
	case "doctor":
		return doctor(*path)
	case "status", "refresh", "pause", "unpause":
		return remote(*path, cmd)
	case "signal":
		return sendSignal(*path, f.Args())
	case "event":
		return forwardEvent(*path, f.Args())
	case "launchd":
		return launchd(*path)
	default:
		usage()
		return errors.New("未知子命令")
	}
}

func usage() {
	fmt.Println("quota-session-orchestrator <init|validate|run|doctor|hooks-config|hook|status|refresh|pause|unpause|launchd> --config <私有配置路径>")
	fmt.Println("quota-session-orchestrator signal --config <路径> -- --provider codex --id <重置ID> [--source cli] [--kind reset_executed|reset_scheduled] [--text 说明]")
	fmt.Println("quota-session-orchestrator event --config <路径> -- --client <客户端ID> --session <会话ID> --type <limit_hit|turn_started|turn_finished|turn_failed|session_start|session_end> [--cwd 目录] [--model 模型] [--title 标题] [--event-id 幂等ID]")
	fmt.Println("quota-session-orchestrator mcp --profile <独立 0600 scoped profile>（stdio；仅连接已有 daemon）")
}

func initialize(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return errors.New("配置已存在，不覆盖")
	} else if !os.IsNotExist(err) {
		return err
	}
	base := filepath.Dir(abs)
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	c := config.Default(base)
	project, err := os.Getwd()
	if err != nil {
		return err
	}
	c.BridgePath = filepath.Join(project, "bin", "desktop-bridge")
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	c.Token = hex.EncodeToString(b)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Println("已创建私有配置：", abs)
	fmt.Println("自动恢复默认暂停。管理密钥使用 CPA_MANAGEMENT_KEY 环境变量；本地面板令牌保存在配置 token 字段。")
	return nil
}

func run(path, listen string) error {
	return runDaemon(path, listen, false)
}

func pauseBeforeRun(ctx context.Context, engine server.Engine) error {
	// Snapshot initializes and reads the durable policy; cfg defaults must not
	// replace existing user settings. SetPolicy also invalidates dispatch leases.
	snap, err := engine.Snapshot(ctx)
	if err != nil {
		return err
	}
	p := snap.Policy
	p.Paused = true
	return engine.SetPolicy(ctx, p)
}

func runDaemon(path, listen string, startupPaused bool) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if listen != "" {
		c.Listen = listen
		if err := c.Validate(); err != nil {
			return err
		}
	}
	// Only the synthesized legacy source may use the legacy key-file flow.
	if len(c.Sources) == 0 {
		if err := loadManagementKey(c); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(c.Database), 0700); err != nil {
		return err
	}
	// A process-scoped lock prevents two daemons sharing a GUI dispatch queue.
	lock, err := os.OpenFile(c.Database+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("已有守护器正在使用该数据库")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	old := syscall.Umask(0077)
	db, err := store.Open(c.Database)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	defer db.Close()
	quota, err := adapters.NewConfigured(context.Background(), c, db)
	if err != nil {
		return err
	}
	executor, err := adapters.ConfiguredExecutor(c)
	if err != nil {
		return err
	}
	engine := scheduler.New(db, quota, executor, c.Policy)
	if runtime.GOOS == "darwin" {
		engine.SetProcessLookup(func(pid int) (time.Time, bool) {
			p, ok := host.Lookup(pid)
			return p.Start, ok
		})
	}
	if err := engine.SetPollInterval(time.Duration(c.PollSeconds) * time.Second); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if startupPaused {
		if err := pauseBeforeRun(ctx, engine); err != nil {
			return err
		}
	}
	runner, err := signals.New(c, engine)
	if err != nil {
		return err
	}
	options := []server.Option{server.WithStore(db)}
	secret, err := loadWebhookSecret(c.Signals.WebhookSecretFile)
	if err != nil {
		return err
	}
	if secret != nil {
		options = append(options, server.WithSignalSecret(secret))
	}
	srv := &http.Server{Addr: c.Listen, Handler: server.New(c, engine, options...), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	errCh := make(chan error, 2)
	go func() { errCh <- engine.Run(ctx) }()
	go runner.Run(ctx)
	go drainSpool(ctx, path, engine)
	go watchCatalog(ctx, engine, catalogRoots())
	go func() {
		fmt.Println("管理面板：http://" + c.Listen + "（恢复默认暂停，令牌不写入 URL）")
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
	case err = <-errCh:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
