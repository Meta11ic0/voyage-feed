package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"voyage-feed/httpapi"
	"voyage-feed/store"
	"voyage-feed/stream"
)

type serveConfig struct {
	DatabaseURL string
	HTTPAddr    string
	MarketID    string
	DatasetPath string
	Dataset     []byte
	Seed        uint64
	Start       *time.Time
	Interval    time.Duration
	Probability int
	MaxMove     int
	Budget      int64
	Retention   int
	MaxClients  int
	SendBuffer  int
}

var marketIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)

func serveCommand(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "serve 不接受参数。")
		return 2
	}
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	svc, err := runServe(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	<-ctx.Done()
	<-svc.done
	return 0
}

type service struct {
	URL    string
	done   chan struct{}
	pause  func()
	resume func() error
}

func configFromEnv() (serveConfig, error) {
	cfg := serveConfig{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    getenv("HTTP_ADDR", ":8787"),
		MarketID:    getenv("MARKET_ID", "one-piece-dev"),
		DatasetPath: getenv("DATASET_PATH", "/app/data/dev.json"),
		Interval:    time.Second,
		Probability: 200,
		MaxMove:     20,
		Budget:      100000000000,
		Retention:   7,
		MaxClients:  64,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("serve 需要环境变量 DATABASE_URL。")
	}
	if !marketIDPattern.MatchString(cfg.MarketID) {
		return cfg, fmt.Errorf("环境变量 MARKET_ID 无效。")
	}
	var err error
	if cfg.Seed, err = parseUintEnv("MARKET_SEED", "7"); err != nil {
		return cfg, err
	}
	intervalMS, err := parseIntEnv("TICK_INTERVAL_MS", "1000", 10, 60000)
	if err != nil {
		return cfg, err
	}
	cfg.Interval = time.Duration(intervalMS) * time.Millisecond
	if cfg.Probability, err = parseIntEnv("TRADE_PROBABILITY_BPS", "200", 1, 10000); err != nil {
		return cfg, err
	}
	if cfg.MaxMove, err = parseIntEnv("MAX_MOVE_BPS", "20", 1, 1000); err != nil {
		return cfg, err
	}
	budget, err := parseUintEnv("INITIAL_CAP_BUDGET_CENTS", "100000000000")
	if err != nil || budget < 1 || budget > 10000000000000 {
		return cfg, fmt.Errorf("环境变量 INITIAL_CAP_BUDGET_CENTS 无效。")
	}
	cfg.Budget = int64(budget)
	if cfg.Retention, err = parseIntEnv("EVENT_RETENTION_DAYS", "7", 1, 30); err != nil {
		return cfg, err
	}
	if cfg.MaxClients, err = parseIntEnv("MAX_STREAM_CLIENTS", "64", 1, 512); err != nil {
		return cfg, err
	}
	if raw := os.Getenv("MARKET_START_TIME"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return cfg, fmt.Errorf("环境变量 MARKET_START_TIME 无效。")
		}
		value := parsed.UTC().Truncate(time.Second)
		cfg.Start = &value
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func parseIntEnv(key, fallback string, min, max int) (int, error) {
	raw := getenv(key, fallback)
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("环境变量 %s 无效。", key)
	}
	return value, nil
}

func parseUintEnv(key, fallback string) (uint64, error) {
	raw := getenv(key, fallback)
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("环境变量 %s 无效。", key)
	}
	return value, nil
}

func runServe(ctx context.Context, cfg serveConfig) (*service, error) {
	if err := prepareMarket(ctx, cfg); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	hub := stream.NewHub()
	api := &httpapi.Server{
		MarketID: cfg.MarketID, Interval: cfg.Interval, Pool: pool, Hub: hub, MaxClients: cfg.MaxClients,
	}
	hub.Fetch = api.Fetch
	hub.Watermark = api.Watermark
	hub.OnDBError = api.NoteDBError
	hub.OnSlow = api.NoteDisconnect
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if cfg.SendBuffer > 0 {
		listener = bufListener{Listener: listener, size: cfg.SendBuffer}
	}
	srv := &http.Server{Handler: api.Handler()}
	go srv.Serve(listener)
	go hub.Run(ctx)
	var genMu sync.Mutex
	var genCancel context.CancelFunc
	startGen := func() error {
		genMu.Lock()
		defer genMu.Unlock()
		if genCancel != nil {
			return nil
		}
		conn, err := pgx.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			api.NoteDBError()
			return err
		}
		gctx, cancel := context.WithCancel(ctx)
		runner := &store.Runner{
			MarketID: cfg.MarketID, Interval: cfg.Interval, RetentionDays: cfg.Retention, Notify: hub.Notify,
			Dial:   func(dialCtx context.Context) (*pgx.Conn, error) { return pgx.Connect(dialCtx, cfg.DatabaseURL) },
			Open:   func(openCtx context.Context) (*pgx.Conn, error) { return pgx.Connect(openCtx, cfg.DatabaseURL) },
			OnLock: func() { api.SetLocked(true) },
			OnStop: func() { api.SetLocked(false) },
			OnDBError: func(error) {
				api.NoteDBError()
				api.SetLocked(false)
			},
			OnCommit: func(result store.StepResult, elapsed time.Duration) {
				api.Observe(result.LastSeq, result.Tick, elapsed)
			},
		}
		genCancel = cancel
		go func() {
			runErr := runner.Run(gctx, conn)
			conn.Close(context.Background())
			genMu.Lock()
			genCancel = nil
			genMu.Unlock()
			if runErr != nil && strings.Contains(runErr.Error(), "同市场已经启动") {
				fmt.Fprintln(os.Stderr, runErr.Error())
				cancelProcess()
			}
		}()
		return nil
	}
	if err := startGen(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		genMu.Lock()
		if genCancel != nil {
			genCancel()
		}
		genMu.Unlock()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shut)
		pool.Close()
		close(done)
	}()
	return &service{
		URL:  "http://" + listener.Addr().String(),
		done: done,
		pause: func() {
			genMu.Lock()
			cancel := genCancel
			genMu.Unlock()
			if cancel != nil {
				cancel()
			}
		},
		resume: func() error {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				genMu.Lock()
				idle := genCancel == nil
				genMu.Unlock()
				if idle && !api.Locked() {
					return startGen()
				}
				time.Sleep(10 * time.Millisecond)
			}
			return fmt.Errorf("生成器没有停下来")
		},
	}, nil
}

type bufListener struct {
	net.Listener
	size int
}

func (b bufListener) Accept() (net.Conn, error) {
	conn, err := b.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(b.size)
	}
	return conn, nil
}

func prepareMarket(ctx context.Context, cfg serveConfig) error {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := prepareOnce(ctx, cfg)
		if err == nil {
			return nil
		}
		if !temporary(err) {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func prepareOnce(ctx context.Context, cfg serveConfig) error {
	conn, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if err := store.Migrate(ctx, conn); err != nil {
		return err
	}
	origin, err := savedOrigin(ctx, conn, cfg)
	if err != nil {
		return err
	}
	raw := cfg.Dataset
	if raw == nil {
		raw, err = os.ReadFile(cfg.DatasetPath)
		if err != nil {
			return fmt.Errorf("数据集文件 %s 无法读取。", cfg.DatasetPath)
		}
	}
	_, err = store.Publish(ctx, conn, store.PublishConfig{
		MarketID: cfg.MarketID, Dataset: raw, Seed: cfg.Seed, TradeProbabilityBps: cfg.Probability,
		MaxMoveBps: cfg.MaxMove, CapBudgetCents: cfg.Budget, ClockOrigin: origin,
	})
	return err
}

func savedOrigin(ctx context.Context, conn *pgx.Conn, cfg serveConfig) (time.Time, error) {
	var origin time.Time
	err := conn.QueryRow(ctx, `SELECT clock_origin FROM market_instances WHERE market_id = $1`, cfg.MarketID).Scan(&origin)
	if errors.Is(err, pgx.ErrNoRows) {
		if cfg.Start != nil {
			return *cfg.Start, nil
		}
		return time.Now().UTC().Truncate(time.Second), nil
	}
	if err != nil {
		return time.Time{}, err
	}
	origin = origin.UTC().Truncate(time.Second)
	if cfg.Start != nil && !cfg.Start.Equal(origin) {
		return time.Time{}, fmt.Errorf("环境变量 MARKET_START_TIME 与已保存起点冲突。")
	}
	return origin, nil
}

func temporary(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	if strings.Contains(text, "不一致") || strings.Contains(text, "冲突") || strings.Contains(text, "无效") || strings.Contains(text, "无法读取") || strings.Contains(text, "没有标的") {
		return false
	}
	return true
}

func cancelProcess() {
	p, err := os.FindProcess(os.Getpid())
	if err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}
