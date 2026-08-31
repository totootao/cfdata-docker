// CFData Web 控制台：支持按数据中心触发测速、查看实时进度与历史结果。
// 与定时模式（run-scheduled.sh）共用同一把文件锁 /tmp/cfdata-scan.lock，互斥执行。
package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFS embed.FS

const (
	lockDir         = "/tmp/cfdata-scan.lock"
	staleLockMin    = 180 // 锁超过 3 小时视为残留（容器曾在扫描中被强杀）
	dcPattern       = `^[A-Za-z]{3}$`
	maxStatusLines  = 300 // 内存中保留的最近输出行数
	defaultScanLine = 40  // 非本服务触发的扫描（如 cron）从 cron.log 读取的行数
)

var (
	dcRe        = regexp.MustCompile(dcPattern)
	progressRe  = regexp.MustCompile(`\[([A-Za-z]{3})\]`)
	egressRe    = regexp.MustCompile(`colo=([A-Za-z]{3})`)
	errScanBusy = errors.New("scan busy")
)

type server struct {
	mu       sync.Mutex
	running  bool      // 本服务触发的扫描是否在跑
	current  string    // 当前正在扫描的数据中心
	started  time.Time // 本次扫描开始时间
	finished time.Time // 最近一次扫描结束时间
	trigger  string    // 触发来源（web / cron / region:XX）
	lines    []string  // 最近输出（环形缓冲）

	scanScript string
	resultsDir string
	logFile    string

	// 地区定时任务（每个地区一条 cron，进程内调度）
	regionMu sync.Mutex
	regions  map[string]*regionTask
	nextRun  map[string]time.Time
}

type statusResp struct {
	Running  bool     `json:"running"`
	Current  string   `json:"current_dc"`
	Started  string   `json:"started_at,omitempty"`
	Duration string   `json:"duration,omitempty"`
	Finished string   `json:"last_finished_at,omitempty"`
	Trigger  string   `json:"trigger,omitempty"`
	Lines    []string `json:"lines"`
}

type resultRow struct {
	IP      string  `json:"ip"`
	Port    string  `json:"port"`
	IPPort  string  `json:"ipport"`
	DC      string  `json:"dc"`
	City    string  `json:"city"`
	Latency string  `json:"latency"`
	Speed   string  `json:"speed"`
	SpeedMB float64 `json:"speed_mb"`
}

type resultDC struct {
	DC      string      `json:"dc"`
	Count   int         `json:"count"`
	Rows    []resultRow `json:"rows"`
	ModTime string      `json:"mod_time"`
}

type configResp struct {
	DefaultDCList string         `json:"default_dc_list"`
	TopN          int            `json:"top_n"`
	SpeedMin      string         `json:"speed_min"`
	Locations     []location     `json:"locations"`
	Egress        string         `json:"egress_colo"`
	CronSchedule  string         `json:"cron_schedule"`
	Countries     []countryEntry `json:"countries"`
}

type location struct {
	Code string `json:"code"`
	City string `json:"city"`
}

func (s *server) now() string { return time.Now().Format("2006-01-02 15:04:05") }

// 锁是否被占用（web 或 cron 任一触发的扫描都会持有）
func (s *server) lockHeld() bool {
	_, err := os.Stat(lockDir)
	return err == nil
}

// 尝试获取锁；若被占用且超时视为残留则清除后重试
func (s *server) acquireLock() bool {
	if err := os.Mkdir(lockDir, 0o755); err == nil {
		return true
	}
	// 已被占用：检查是否为残留锁（mtime 超过 staleLockMin）
	info, err := os.Stat(lockDir)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) > staleLockMin*time.Minute {
		log.Printf("检测到残留锁（%s），自动清除", info.ModTime().Format(time.RFC3339))
		os.RemoveAll(lockDir)
		return os.Mkdir(lockDir, 0o755) == nil
	}
	return false
}

func (s *server) releaseLock() { os.RemoveAll(lockDir) }

// POST /api/scan：触发一次扫描（dc_list 形如 "LHR,FRA"）
func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req struct {
		DCList   string `json:"dc_list"`
		TopN     int    `json:"top_n"`
		SpeedMin string `json:"speed_min"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}

	// 数据中心参数校验：仅允许 3 字母 IATA 码，防注入（值会作为环境变量传给脚本）
	dcs := parseDCList(req.DCList)
	if len(dcs) == 0 {
		httpError(w, http.StatusBadRequest, "请提供至少一个有效的数据中心（3 字母 IATA 码）")
		return
	}
	topN := req.TopN
	if topN <= 0 || topN > 100 {
		topN = envInt("TOP_N", 10)
	}
	speedMin := strings.TrimSpace(req.SpeedMin)
	if speedMin == "" {
		speedMin = os.Getenv("SPEED_MIN")
	}
	if speedMin == "" {
		speedMin = "1"
	}
	if _, err := strconv.ParseFloat(speedMin, 64); err != nil {
		httpError(w, http.StatusBadRequest, "speed_min 必须是数字")
		return
	}

	dcList := strings.Join(dcs, ",")
	if err := s.runScanAsync(dcList, topN, speedMin, "web"); err != nil {
		httpError(w, http.StatusConflict, "已有扫描任务进行中，请稍候")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "started",
		"dc_list": dcList,
	})
}

// scanState 返回当前扫描状态与触发来源
func (s *server) scanState() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running, s.trigger
}

// runScanAsync 启动一次后台扫描（web 触发与地区定时任务共用同一实现）。
// 返回非 nil 表示已有任务在跑（互斥：进程内状态 + 文件锁双重保护）。
func (s *server) runScanAsync(dcList string, topN int, speedMin, trigger string) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errScanBusy
	}
	s.mu.Unlock()

	if !s.acquireLock() {
		return errScanBusy
	}

	s.mu.Lock()
	s.running = true
	s.current = ""
	s.started = time.Now()
	s.trigger = trigger
	s.lines = []string{}
	s.mu.Unlock()

	logFile, err := os.OpenFile(s.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("打开日志文件失败: %v", err)
	}

	cmd := exec.Command(s.scanScript)
	cmd.Env = filteredEnv("DC_LIST", "TOP_N", "SPEED_MIN")
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("DC_LIST=%s", dcList),
		fmt.Sprintf("TOP_N=%d", topN),
		fmt.Sprintf("SPEED_MIN=%s", speedMin),
	)

	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		s.releaseLock()
		if logFile != nil {
			logFile.Close()
		}
		return err
	}

	logLine := func(line string) {
		s.mu.Lock()
		s.lines = append(s.lines, line)
		if len(s.lines) > maxStatusLines {
			s.lines = s.lines[len(s.lines)-maxStatusLines:]
		}
		s.mu.Unlock()
	}

	go func() {
		fmt.Fprintf(logFile, "[%s] ===== 开始扫描（%s 触发: %s，Top%d） =====\n", s.now(), trigger, dcList, topN)
		scanner := bufio.NewScanner(io.TeeReader(stdout, logFile))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if m := progressRe.FindStringSubmatch(line); len(m) > 1 {
				s.mu.Lock()
				if strings.Contains(line, "开始扫描") {
					s.current = strings.ToUpper(m[1])
				}
				s.mu.Unlock()
			}
			logLine(line)
		}
		err := cmd.Wait()
		s.mu.Lock()
		s.running = false
		s.current = ""
		s.finished = time.Now()
		s.mu.Unlock()
		if err != nil {
			fmt.Fprintf(logFile, "[%s] ===== 扫描异常退出（exit=%v） =====\n", s.now(), err)
		} else {
			fmt.Fprintf(logFile, "[%s] ===== 扫描完成 =====\n", s.now())
		}
		logFile.Sync()
		logFile.Close()
		s.releaseLock()
		s.onScanDone(trigger, err)
		log.Printf("扫描完成（%s 触发: %s）", trigger, dcList)
	}()
	return nil
}

// GET /api/status：运行状态 + 最近输出
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	resp := statusResp{
		Running: s.running,
		Current: s.current,
		Trigger: s.trigger,
		Lines:   append([]string{}, s.lines...),
	}
	if !s.started.IsZero() {
		resp.Started = s.started.Format("2006-01-02 15:04:05")
	}
	if !s.finished.IsZero() {
		resp.Finished = s.finished.Format("2006-01-02 15:04:05")
	}
	if s.running {
		resp.Duration = time.Since(s.started).Round(time.Second).String()
	}
	s.mu.Unlock()

	// 非 web 触发的扫描（cron）：锁被占用但本服务状态为空闲
	if !resp.Running && s.lockHeld() {
		resp.Running = true
		resp.Trigger = "cron"
		if lines := tailFile(s.logFile, defaultScanLine); len(lines) > 0 {
			resp.Lines = lines
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/results：各数据中心 TopN 结果
func (s *server) handleResults(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.resultsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, []resultDC{})
		return
	}
	var out []resultDC
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".csv") {
			continue
		}
		dc := strings.ToUpper(strings.TrimSuffix(name, ".csv"))
		if !dcRe.MatchString(dc) {
			continue
		}
		rows := parseCSV(filepath.Join(s.resultsDir, name))
		info, _ := e.Info()
		mod := ""
		if info != nil {
			mod = info.ModTime().Format("2006-01-02 15:04:05")
		}
		out = append(out, resultDC{DC: dc, Count: len(rows), Rows: rows, ModTime: mod})
	}
	if out == nil {
		out = []resultDC{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/logs：cron.log 尾部
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 2000 {
			n = p
		}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"lines": tailFile(s.logFile, n)})
}

// GET /api/config：默认配置 + 常用机房列表
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	resp := configResp{
		DefaultDCList: os.Getenv("DC_LIST"),
		TopN:          envInt("TOP_N", 10),
		SpeedMin:      os.Getenv("SPEED_MIN"),
		CronSchedule:  os.Getenv("CRON_SCHEDULE"),
		Locations: []location{
			{"LHR", "伦敦"}, {"FRA", "法兰克福"}, {"SEA", "西雅图"}, {"AMS", "阿姆斯特丹"},
			{"CDG", "巴黎"}, {"IAD", "华盛顿"}, {"LAX", "洛杉矶"}, {"SJC", "圣何塞"},
			{"HKG", "香港"}, {"NRT", "东京"}, {"ICN", "首尔"}, {"SIN", "新加坡"},
			{"DUB", "都柏林"}, {"MAD", "马德里"}, {"MXP", "米兰"}, {"ARN", "斯德哥尔摩"},
			{"YYZ", "多伦多"}, {"GRU", "圣保罗"}, {"JNB", "约翰内斯堡"}, {"SYD", "悉尼"},
			{"BOM", "孟买"}, {"DXB", "迪拜"},
		},
		Countries: countryColos,
	}
	if resp.DefaultDCList == "" {
		resp.DefaultDCList = "LHR,FRA,SEA"
	}
	if resp.SpeedMin == "" {
		resp.SpeedMin = "1"
	}
	// 出口机房从 summary.txt 解析（scan.sh 每次运行都会记录）
	if data, err := os.ReadFile(filepath.Join(s.resultsDir, "summary.txt")); err == nil {
		if m := egressRe.FindSubmatch(data); len(m) > 1 {
			resp.Egress = strings.ToUpper(string(m[1]))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleSummary(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(filepath.Join(s.resultsDir, "summary.txt"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"summary": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"summary": string(data)})
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ---------- 工具函数 ----------

func parseDCList(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(s, ",") {
		dc := strings.ToUpper(strings.TrimSpace(part))
		if dc == "" || !dcRe.MatchString(dc) {
			continue
		}
		if !seen[dc] {
			seen[dc] = true
			out = append(out, dc)
		}
	}
	return out
}

// 解析结果 CSV（含 UTF-8 BOM），列: IP,端口,ip:port,数据中心,城市,延迟,速度
// 速度列非法（如"测速失败"）的行直接跳过：延迟达标但下载测速失败的 IP 不参与优选
func parseCSV(path string) []resultRow {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var rows []resultRow
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || i == 0 { // 跳过表头
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 7 {
			continue
		}
		speed := strings.TrimSpace(f[6])
		speedMB := 0.0
		if v, err := strconv.ParseFloat(strings.TrimSuffix(speed, "MB/s"), 64); err != nil {
			continue // 测速失败/格式异常的行丢弃
		} else {
			speedMB = v
		}
		rows = append(rows, resultRow{
			IP: f[0], Port: f[1], IPPort: f[2], DC: f[3],
			City: f[4], Latency: f[5], Speed: speed, SpeedMB: speedMB,
		})
	}
	return rows
}

func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
	}
	return lines
}

func filteredEnv(keys ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		skip := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func main() {
	port := os.Getenv("WEB_PORT")
	if port == "" {
		port = "8080"
	}
	s := &server{
		scanScript: envOr("SCAN_SCRIPT", "/app/scan.sh"),
		resultsDir: envOr("RESULTS_DIR", "/app/results"),
		logFile:    filepath.Join(envOr("RESULTS_DIR", "/app/results"), "cron.log"),
		regions:    map[string]*regionTask{},
		nextRun:    map[string]time.Time{},
	}
	os.MkdirAll(s.resultsDir, 0o755)
	s.loadRegions()
	s.startScheduler()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/results", s.handleResults)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/summary", s.handleSummary)
	// 地区定时任务管理（每地区一条 cron，Web 配置，regions.json 持久化）
	mux.HandleFunc("GET /api/regions", s.handleRegionsList)
	mux.HandleFunc("POST /api/regions", s.handleRegionSave)
	mux.HandleFunc("DELETE /api/regions/{region}", s.handleRegionDelete)
	mux.HandleFunc("POST /api/regions/{region}/scan", s.handleRegionScan)
	// bestcf 风格优选输出 API
	mux.HandleFunc("GET /random-region/{region}/{file}", s.handleRandomRegion)
	mux.HandleFunc("GET /region/", s.handleRegionTop)

	addr := ":" + port
	log.Printf("CFData Web 控制台启动: http://0.0.0.0%s (结果目录: %s)", addr, s.resultsDir)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
