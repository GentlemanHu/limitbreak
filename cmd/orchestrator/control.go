package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quota-session-orchestrator/internal/adapters"
	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/proxy"
)

func remote(path, command string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	method, endpoint := "GET", "/api/snapshot"
	var body []byte
	switch command {
	case "refresh":
		method, endpoint = "POST", "/api/refresh"
	case "pause", "unpause":
		data, err := call(ctx, c, "GET", "/api/snapshot", nil)
		if err != nil {
			return err
		}
		var snapshot domain.Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return err
		}
		snapshot.Policy.Paused = command == "pause"
		body, err = json.Marshal(snapshot.Policy)
		if err != nil {
			return err
		}
		method, endpoint = "POST", "/api/policy"
	}
	data, err := call(ctx, c, method, endpoint, body)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func doctor(path string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := loadManagementKey(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := proxy.New(c)
	accounts, proxyErr := p.Refresh(ctx)
	result := map[string]any{"config_valid": true, "management_key_present": os.Getenv(c.ManagementKeyEnv) != "", "capabilities": p.Capabilities(), "account_count": len(accounts)}
	if executor, err := adapters.ConfiguredExecutor(c); err != nil {
		result["bridge"] = "执行器配置无效"
	} else {
		result["bridge"] = executor.Probe(ctx)
	}
	if proxyErr != nil {
		result["proxy_error"] = "代理查询失败；检查服务、管理密钥与网络权限"
	}
	result["hook_requirements"] = map[string]string{"StopFailure": "内嵌 Code >= 2.1.78", "PostModelSwitch": "内嵌 Code >= 2.1.251", "desktop": "必须确认事件实际到达；standalone CLI 版本不等于 Desktop 内嵌版本"}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func launchd(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	c, err := config.Load(abs)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	base := filepath.Dir(abs)
	x := func(s string) string { var b strings.Builder; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	// The generated file contains paths only. Never export a secret into the plist.
	fmt.Printf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>local.quota-session-orchestrator</string>
<key>ProgramArguments</key><array><string>%s</string><string>run</string><string>--config</string><string>%s</string></array>
<key>WorkingDirectory</key><string>%s</string>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>15</integer>
<key>ProcessType</key><string>Background</string>
<key>LimitLoadToSessionType</key><string>Aqua</string>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
<key>Umask</key><integer>63</integer>
</dict></plist>
`, x(executable), x(abs), x(base), x(filepath.Join(base, "daemon.stdout.log")), x(filepath.Join(base, "daemon.stderr.log")))
	_ = c
	return nil
}
