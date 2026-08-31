package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// regionTask 一个地区的定时优选任务：cron 到点自动扫描该地区的机房，
// 结果供 /random-region/{region}/{n}.txt 与 /region/{region}.txt 输出
type regionTask struct {
	Region     string   `json:"region"`
	Name       string   `json:"name"`
	Colos      []string `json:"colos"`
	Cron       string   `json:"cron"`
	TopN       int      `json:"top_n"`
	SpeedMin   string   `json:"speed_min"`
	Enabled    bool     `json:"enabled"`
	LastRun    string   `json:"last_run"`
	LastStatus string   `json:"last_status"`
	NextRun    string   `json:"next_run"`
}

type regionStatus struct {
	regionTask
	ResultCount int  `json:"result_count"`
	Running     bool `json:"running"`
}

type countryEntry struct {
	Code  string   `json:"code"`
	Name  string   `json:"name"`
	Colos []string `json:"colos"`
}

// 常用国家/地区 → Cloudflare 机房（IATA 码）默认映射。
// 仅作为 Web 表单的预设值，任务里的机房列表可任意修改。
var countryColos = []countryEntry{
	{"KR", "韩国", []string{"ICN"}},
	{"JP", "日本", []string{"NRT", "KIX"}},
	{"HK", "香港", []string{"HKG"}},
	{"TW", "台湾", []string{"TPE"}},
	{"SG", "新加坡", []string{"SIN"}},
	{"MY", "马来西亚", []string{"KUL"}},
	{"TH", "泰国", []string{"BKK"}},
	{"VN", "越南", []string{"HAN", "SGN"}},
	{"PH", "菲律宾", []string{"MNL"}},
	{"ID", "印度尼西亚", []string{"CGK"}},
	{"IN", "印度", []string{"BOM", "DEL", "MAA"}},
	{"AE", "阿联酋", []string{"DXB"}},
	{"SA", "沙特阿拉伯", []string{"RUH"}},
	{"TR", "土耳其", []string{"IST"}},
	{"IL", "以色列", []string{"TLV"}},
	{"DE", "德国", []string{"FRA", "MUC", "DUS"}},
	{"GB", "英国", []string{"LHR", "MAN"}},
	{"FR", "法国", []string{"CDG", "MRS"}},
	{"NL", "荷兰", []string{"AMS"}},
	{"ES", "西班牙", []string{"MAD", "BCN"}},
	{"IT", "意大利", []string{"MXP", "FCO"}},
	{"CH", "瑞士", []string{"ZRH"}},
	{"AT", "奥地利", []string{"VIE"}},
	{"SE", "瑞典", []string{"ARN"}},
	{"NO", "挪威", []string{"OSL"}},
	{"FI", "芬兰", []string{"HEL"}},
	{"DK", "丹麦", []string{"CPH"}},
	{"PL", "波兰", []string{"WAW"}},
	{"CZ", "捷克", []string{"PRG"}},
	{"RO", "罗马尼亚", []string{"OTP"}},
	{"PT", "葡萄牙", []string{"LIS"}},
	{"IE", "爱尔兰", []string{"DUB"}},
	{"RU", "俄罗斯", []string{"DME"}},
	{"US", "美国", []string{"SEA", "SJC", "LAX", "DFW", "ORD", "IAD", "EWR", "BOS", "MIA", "ATL"}},
	{"CA", "加拿大", []string{"YYZ", "YVR"}},
	{"BR", "巴西", []string{"GRU"}},
	{"MX", "墨西哥", []string{"MEX"}},
	{"AR", "阿根廷", []string{"EZE"}},
	{"AU", "澳大利亚", []string{"SYD", "MEL", "PER"}},
	{"NZ", "新西兰", []string{"AKL"}},
	{"ZA", "南非", []string{"JNB"}},
	{"EG", "埃及", []string{"CAI"}},
	{"NG", "尼日利亚", []string{"LOS"}},
	{"KE", "肯尼亚", []string{"NBO"}},
}

var (
	regionCodeRe = regexp.MustCompile(`^[A-Z0-9_-]{1,16}$`)
	coloCodeRe   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ---------- 存储 ----------

func (s *server) regionsFile() string {
	return filepath.Join(s.resultsDir, "regions.json")
}

// loadRegions 从挂载卷读取地区任务；next_run 基于"现在"重算（容器重启期间错过的触发不补跑）
func (s *server) loadRegions() {
	s.regions = map[string]*regionTask{}
	s.nextRun = map[string]time.Time{}
	data, err := os.ReadFile(s.regionsFile())
	if err != nil {
		return // 首次运行无配置文件
	}
	var f struct {
		Regions []regionTask `json:"regions"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("regions.json 解析失败，忽略: %v", err)
		return
	}
	now := time.Now()
	for i := range f.Regions {
		t := &f.Regions[i]
		t.Region = strings.ToUpper(t.Region)
		if !regionCodeRe.MatchString(t.Region) || len(t.Colos) == 0 {
			continue
		}
		if t.TopN <= 0 {
			t.TopN = 10
		}
		if t.SpeedMin == "" {
			t.SpeedMin = "1"
		}
		s.regions[t.Region] = t
		if t.Enabled {
			if expr, err := parseCron(t.Cron); err == nil {
				nr := expr.Next(now)
				s.nextRun[t.Region] = nr
				t.NextRun = nr.Format(time.DateTime)
			}
		}
	}
	log.Printf("已加载 %d 个地区任务", len(s.regions))
}

// saveRegionsLocked 持久化任务列表（调用方需持有 regionMu）
func (s *server) saveRegionsLocked() {
	f := struct {
		Regions []regionTask `json:"regions"`
	}{}
	for _, t := range s.regions {
		f.Regions = append(f.Regions, *t)
	}
	sort.Slice(f.Regions, func(i, j int) bool { return f.Regions[i].Region < f.Regions[j].Region })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.regionsFile(), data, 0o644); err != nil {
		log.Printf("regions.json 写入失败: %v", err)
	}
}

// regionColos 查询地区对应的机房列表：优先已配置任务，其次国家预设
func (s *server) regionColos(region string) []string {
	region = strings.ToUpper(region)
	s.regionMu.Lock()
	defer s.regionMu.Unlock()
	if t, ok := s.regions[region]; ok && len(t.Colos) > 0 {
		return append([]string{}, t.Colos...)
	}
	for _, c := range countryColos {
		if c.Code == region {
			return append([]string{}, c.Colos...)
		}
	}
	return nil
}

// collectRows 汇总多个机房的扫描结果
func (s *server) collectRows(colos []string) []resultRow {
	var rows []resultRow
	for _, colo := range colos {
		rows = append(rows, parseCSV(filepath.Join(s.resultsDir, strings.ToLower(colo)+".csv"))...)
	}
	return rows
}

// ---------- 调度器（进程内，仅 Web 模式生效） ----------

// startScheduler 每 15 秒检查一次到点的地区任务
func (s *server) startScheduler() {
	go func() {
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for range tk.C {
			s.fireDueRegions()
		}
	}()
}

func (s *server) fireDueRegions() {
	now := time.Now()
	s.regionMu.Lock()
	var due []string
	for region, t := range s.regions {
		if !t.Enabled {
			continue
		}
		if nr, ok := s.nextRun[region]; ok && !nr.After(now) {
			due = append(due, region)
		}
	}
	s.regionMu.Unlock()
	for _, region := range due {
		s.fireRegion(region)
	}
}

func (s *server) fireRegion(region string) {
	s.regionMu.Lock()
	t, ok := s.regions[region]
	if !ok || !t.Enabled {
		s.regionMu.Unlock()
		return
	}
	dcList := strings.Join(t.Colos, ",")
	topN, speedMin, cron := t.TopN, t.SpeedMin, t.Cron
	s.regionMu.Unlock()

	// busy 时返回错误：nextRun 保持过期状态，下一轮继续重试（相当于排队）
	if err := s.runScanAsync(dcList, topN, speedMin, "region:"+region); err != nil {
		return
	}

	// 触发成功：推进下一次运行时间并持久化
	s.regionMu.Lock()
	if expr, err := parseCron(cron); err == nil {
		nr := expr.Next(time.Now())
		s.nextRun[region] = nr
		if t, ok := s.regions[region]; ok {
			t.NextRun = nr.Format(time.DateTime)
		}
		s.saveRegionsLocked()
	}
	s.regionMu.Unlock()
	log.Printf("地区任务 %s 已触发（机房: %s）", region, dcList)
}

// onScanDone 扫描结束后更新地区任务的运行记录（由 runScanAsync 的完成回调调用）
func (s *server) onScanDone(trigger string, runErr error) {
	if !strings.HasPrefix(trigger, "region:") {
		return
	}
	region := strings.TrimPrefix(trigger, "region:")
	s.regionMu.Lock()
	defer s.regionMu.Unlock()
	t, ok := s.regions[region]
	if !ok {
		return
	}
	t.LastRun = time.Now().Format(time.DateTime)
	if runErr != nil {
		t.LastStatus = "failed"
	} else {
		t.LastStatus = "ok"
	}
	s.saveRegionsLocked()
}

// ---------- HTTP handlers ----------

// GET /api/regions 任务列表（含运行状态与结果数）
func (s *server) handleRegionsList(w http.ResponseWriter, r *http.Request) {
	s.regionMu.Lock()
	out := make([]regionStatus, 0, len(s.regions))
	running, trigger := s.scanState()
	for _, t := range s.regions {
		out = append(out, regionStatus{
			regionTask:  *t,
			ResultCount: len(s.collectRows(t.Colos)),
			Running:     running && trigger == "region:"+t.Region,
		})
	}
	s.regionMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Region < out[j].Region })
	writeJSON(w, http.StatusOK, out)
}

// POST /api/regions 创建或更新任务（以 region 为键 upsert）
func (s *server) handleRegionSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req regionTask
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	req.Region = strings.ToUpper(strings.TrimSpace(req.Region))
	if !regionCodeRe.MatchString(req.Region) {
		httpError(w, http.StatusBadRequest, "地区码仅允许 1-16 位字母/数字/中划线/下划线")
		return
	}
	var colos []string
	seen := map[string]bool{}
	for _, c := range req.Colos {
		c = strings.ToUpper(strings.TrimSpace(c))
		if !coloCodeRe.MatchString(c) {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("机房码 %q 无效（应为 3 字母 IATA 码）", c))
			return
		}
		if !seen[c] {
			seen[c] = true
			colos = append(colos, c)
		}
	}
	if len(colos) == 0 {
		httpError(w, http.StatusBadRequest, "至少提供一个机房（IATA 码）")
		return
	}
	if len(colos) > 10 {
		httpError(w, http.StatusBadRequest, "单个任务最多 10 个机房")
		return
	}
	req.Cron = strings.TrimSpace(req.Cron)
	if _, err := parseCron(req.Cron); err != nil {
		httpError(w, http.StatusBadRequest, "cron 表达式无效: "+err.Error())
		return
	}
	topN := req.TopN
	if topN < 1 || topN > 100 {
		topN = 10
	}
	speedMin := strings.TrimSpace(req.SpeedMin)
	if speedMin == "" {
		speedMin = "1"
	}
	if _, err := strconv.ParseFloat(speedMin, 64); err != nil {
		httpError(w, http.StatusBadRequest, "speed_min 必须是数字")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = req.Region
	}

	s.regionMu.Lock()
	t, ok := s.regions[req.Region]
	if !ok {
		t = &regionTask{}
		s.regions[req.Region] = t
	}
	t.Region, t.Name, t.Colos = req.Region, req.Name, colos
	t.Cron, t.TopN, t.SpeedMin, t.Enabled = req.Cron, topN, speedMin, req.Enabled
	if t.Enabled {
		if expr, err := parseCron(t.Cron); err == nil {
			nr := expr.Next(time.Now())
			s.nextRun[t.Region] = nr
			t.NextRun = nr.Format(time.DateTime)
		}
	} else {
		delete(s.nextRun, t.Region)
		t.NextRun = ""
	}
	s.saveRegionsLocked()
	s.regionMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "region": req.Region})
}

// DELETE /api/regions/{region} 删除任务（不影响已生成的结果文件）
func (s *server) handleRegionDelete(w http.ResponseWriter, r *http.Request) {
	region := strings.ToUpper(r.PathValue("region"))
	s.regionMu.Lock()
	if _, ok := s.regions[region]; !ok {
		s.regionMu.Unlock()
		httpError(w, http.StatusNotFound, "地区任务不存在")
		return
	}
	delete(s.regions, region)
	delete(s.nextRun, region)
	s.saveRegionsLocked()
	s.regionMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "region": region})
}

// POST /api/regions/{region}/scan 立即触发该地区扫描
func (s *server) handleRegionScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	region := strings.ToUpper(r.PathValue("region"))
	s.regionMu.Lock()
	t, ok := s.regions[region]
	if !ok {
		s.regionMu.Unlock()
		httpError(w, http.StatusNotFound, "地区任务不存在")
		return
	}
	dcList := strings.Join(t.Colos, ",")
	topN, speedMin := t.TopN, t.SpeedMin
	s.regionMu.Unlock()

	if err := s.runScanAsync(dcList, topN, speedMin, "region:"+region); err != nil {
		httpError(w, http.StatusConflict, "已有扫描任务进行中，请稍候")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "region": region})
}

// GET /random-region/{region}/{n}.txt 随机返回 n 个 IP（bestcf 风格）
func (s *server) handleRandomRegion(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !strings.HasSuffix(file, ".txt") {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(strings.TrimSuffix(file, ".txt"))
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return
	}
	if n > 500 {
		n = 500
	}
	colos := s.regionColos(r.PathValue("region"))
	if len(colos) == 0 {
		httpError(w, http.StatusNotFound, "未知地区（无任务配置，也无国家预设）")
		return
	}
	rows := s.collectRows(colos)
	if len(rows) == 0 {
		httpError(w, http.StatusNotFound, "该地区暂无扫描数据，请先触发对应机房的扫描")
		return
	}
	rand.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	if len(rows) > n {
		rows = rows[:n]
	}
	writeText(w, rowsToText(rows))
}

// GET /region/{region}.txt 该地区按下载速度降序的优选结果（最多 100 条）
func (s *server) handleRegionTop(w http.ResponseWriter, r *http.Request) {
	region := strings.ToUpper(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/region/"), ".txt"))
	if region == "" || strings.Contains(region, "/") {
		http.NotFound(w, r)
		return
	}
	colos := s.regionColos(region)
	if len(colos) == 0 {
		httpError(w, http.StatusNotFound, "未知地区（无任务配置，也无国家预设）")
		return
	}
	rows := s.collectRows(colos)
	if len(rows) == 0 {
		httpError(w, http.StatusNotFound, "该地区暂无扫描数据，请先触发对应机房的扫描")
		return
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].SpeedMB > rows[j].SpeedMB })
	if len(rows) > 100 {
		rows = rows[:100]
	}
	writeText(w, rowsToText(rows))
}

func rowsToText(rows []resultRow) string {
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(row.IPPort)
		b.WriteString("\n")
	}
	return b.String()
}

func writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(body))
}
