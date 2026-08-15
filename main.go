package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// 未配置管理密码：首次运行自动生成随机密码，写回配置文件并打印日志
	if cfg.AdminPassword == "" {
		cfg.AdminPassword, err = generatePassword()
		if err != nil {
			slog.Error("generate admin password", "err", err)
			os.Exit(1)
		}
		if err := saveConfigPassword(*configPath, cfg.AdminPassword); err != nil {
			slog.Error("persist admin password to config", "err", err)
			os.Exit(1)
		}
		slog.Info("generated admin password",
			"password", cfg.AdminPassword,
			"persisted_to", *configPath,
		)
	}

	pool := NewPool(cfg.Keys)
	proxy := NewProxy(pool, cfg.Upstream, cfg.MaxRetries, time.Duration(cfg.RequestTimeout)*time.Second)
	proxy.SetBlockRetry(cfg.blockRetryEnabled(), cfg.MaxBlockRetries, cfg.BlockRetryMode)
	web := &WebUI{pool: pool, adminPassword: cfg.AdminPassword, configPath: *configPath}

	// protect 为管理 API 套 Bearer Token 认证（密码保证非空，见上方生成逻辑）
	protect := func(h http.Handler) http.Handler {
		return bearerAuth(h, cfg.AdminPassword)
	}
	slog.Info("webui auth enabled")

	mux := http.NewServeMux()
	mux.Handle("/v1beta", proxy)
	mux.Handle("/v1beta/", proxy)
	mux.HandleFunc("GET /health", web.handleHealth)    // 健康探针免认证
	mux.HandleFunc("POST /api/login", web.handleLogin) // 登录换取 token
	mux.Handle("GET /api/pool", protect(http.HandlerFunc(web.handlePool)))
	mux.Handle("POST /api/keys", protect(http.HandlerFunc(web.handleAddKey)))
	mux.Handle("DELETE /api/keys/{id}", protect(http.HandlerFunc(web.handleDeleteKey)))
	mux.Handle("POST /api/keys/{id}/state", protect(http.HandlerFunc(web.handleSetState)))
	mux.HandleFunc("/", web.handleIndex) // 页面 HTML 公开，数据都在受保护的 API 里

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}

	slog.Info("gemini key gateway started",
		"listen", cfg.Listen,
		"upstream", cfg.Upstream,
		"keys", len(cfg.Keys),
		"max_retries", cfg.MaxRetries,
		"request_timeout", cfg.RequestTimeout,
		"block_retry", cfg.blockRetryEnabled(),
		"max_block_retries", cfg.MaxBlockRetries,
		"block_retry_mode", cfg.BlockRetryMode,
	)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
