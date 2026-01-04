package check

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/beck-8/subs-check/check/platform"
	"github.com/beck-8/subs-check/config"
	proxyutils "github.com/beck-8/subs-check/proxy"
)

// calculateHardTimeout 计算硬超时时间（秒）
// 基础公式：Timeout/1000 + 1
// 下限约束：结果 < 1 时，使用 1
// 上限约束：结果 > 6 时，使用 6
func calculateHardTimeout() int {
	hardTimeoutSec := config.GlobalConfig.Timeout/1000 + 1
	if hardTimeoutSec < 1 {
		hardTimeoutSec = 1
	}
	if hardTimeoutSec > 6 {
		hardTimeoutSec = 6
	}
	return hardTimeoutSec
}

// checkAliveWithHardTimeout 带硬超时的连通性检测
// 使用 goroutine + select 模式强制终止不遵守 HTTP 超时的代理连接
func checkAliveWithHardTimeout(httpClient *http.Client, hardTimeoutSec int) (bool, error) {
	type result struct {
		alive bool
		err   error
	}

	resultCh := make(chan result, 1)

	go func() {
		alive, err := platform.CheckAlive(httpClient)
		resultCh <- result{alive: alive, err: err}
	}()

	select {
	case res := <-resultCh:
		return res.alive, res.err
	case <-time.After(time.Duration(hardTimeoutSec) * time.Second):
		return false, fmt.Errorf("hard timeout after %ds", hardTimeoutSec)
	}
}

// TwoStageChecker 两阶段检测器
type TwoStageChecker struct {
	phase1Threads          int
	effectivePhase2Threads int
	proxies                []map[string]any
}

// NewTwoStageChecker 创建两阶段检测器
func NewTwoStageChecker(proxies []map[string]any) *TwoStageChecker {
	return &TwoStageChecker{
		phase1Threads:          config.GlobalConfig.Phase1Threads(),
		effectivePhase2Threads: config.GlobalConfig.EffectivePhase2Threads(),
		proxies:                proxies,
	}
}

// Run 执行两阶段检测
func (tc *TwoStageChecker) Run() ([]Result, error) {
	if tc.effectivePhase2Threads == 0 {
		return tc.runDegradedMode()
	}
	return tc.runFullTwoStage()
}

// runDegradedMode 退化模式（仅连通性检测）
// 当 effectivePhase2Threads == 0 时执行
// 不测速、不添加速度标签，但支持平台检测和重命名
func (tc *TwoStageChecker) runDegradedMode() ([]Result, error) {
	proxyCount := len(tc.proxies)
	threadCount := tc.phase1Threads
	if proxyCount < threadCount {
		threadCount = proxyCount
	}

	startTime := time.Now()
	slog.Info("退化模式开始", "总节点数", proxyCount, "并发数", threadCount)

	// 创建 ProxyChecker，允许早停
	pc := &ProxyChecker{
		results:          make([]Result, 0),
		proxyCount:       proxyCount,
		threadCount:      threadCount,
		resultChan:       make(chan Result),
		tasks:            make(chan map[string]any, 1),
		disableEarlyStop: false, // 退化模式允许早停
		stageName:        "退化模式-连通性检测",
	}

	ProxyCount.Store(uint32(proxyCount))
	Progress.Store(0)
	Available.Store(0)

	done := make(chan bool)
	if config.GlobalConfig.PrintProgress {
		go pc.showProgress(done)
	}

	var wg sync.WaitGroup
	for i := 0; i < threadCount; i++ {
		wg.Add(1)
		go tc.degradedWorker(pc, &wg)
	}

	go pc.distributeProxies(tc.proxies)

	var collectWg sync.WaitGroup
	collectWg.Add(1)
	go func() {
		pc.collectResults()
		collectWg.Done()
	}()

	wg.Wait()
	close(pc.resultChan)
	collectWg.Wait()
	time.Sleep(100 * time.Millisecond)

	if config.GlobalConfig.PrintProgress {
		done <- true
	}

	elapsed := time.Since(startTime)
	slog.Info("退化模式完成", "耗时", formatDuration(elapsed), "可用节点数", len(pc.results))

	// 检查订阅成功率
	pc.checkSubscriptionSuccessRate(tc.proxies)

	// 应用过滤规则
	return FilterResults(pc.results), nil
}

// degradedWorker 退化模式工作线程
// 仅检测连通性，不测速，不添加速度标签
func (tc *TwoStageChecker) degradedWorker(pc *ProxyChecker, wg *sync.WaitGroup) {
	defer wg.Done()
	hardTimeoutSec := calculateHardTimeout()

	for proxy := range pc.tasks {
		result := tc.checkProxyDegraded(proxy, hardTimeoutSec)
		if result != nil {
			pc.resultChan <- *result
			pc.incrementAvailable()
		}
		pc.incrementProgress()
	}
}

// checkProxyDegraded 退化模式检测单个代理
func (tc *TwoStageChecker) checkProxyDegraded(proxy map[string]any, hardTimeoutSec int) *Result {
	res := &Result{
		Proxy: proxy,
	}

	httpClient := CreateClient(proxy)
	if httpClient == nil {
		slog.Debug(fmt.Sprintf("创建代理Client失败: %v", proxy["name"]))
		return nil
	}
	defer httpClient.Close()

	// 使用硬超时检测连通性
	alive, err := checkAliveWithHardTimeout(httpClient.Client, hardTimeoutSec)
	if err != nil || !alive {
		return nil
	}

	// 平台检测（可选）
	if config.GlobalConfig.MediaCheck {
		tc.performMediaCheck(res, httpClient)
	}

	// 重命名（可选），不添加速度标签
	pc := &ProxyChecker{}
	pc.updateProxyNameEx(res, httpClient, 0, false) // includeSpeedTag = false

	return res
}

// performMediaCheck 执行平台检测
func (tc *TwoStageChecker) performMediaCheck(res *Result, httpClient *ProxyClient) {
	mediaTimeout := config.GlobalConfig.MediaCheckTimeout
	if mediaTimeout <= 0 {
		mediaTimeout = 10
	}
	mediaClient := &http.Client{
		Transport: httpClient.Client.Transport,
		Timeout:   time.Duration(mediaTimeout) * time.Second,
	}

	for _, plat := range config.GlobalConfig.Platforms {
		switch plat {
		case "openai":
			cookiesOK, clientOK := platform.CheckOpenAI(mediaClient)
			if clientOK && cookiesOK {
				res.Openai = true
			} else if cookiesOK || clientOK {
				res.OpenaiWeb = true
			}
		case "youtube":
			if region, _ := platform.CheckYoutube(mediaClient); region != "" {
				res.Youtube = region
			}
		case "netflix":
			if ok, _ := platform.CheckNetflix(mediaClient); ok {
				res.Netflix = true
			}
		case "disney":
			if ok, _ := platform.CheckDisney(mediaClient); ok {
				res.Disney = true
			}
		case "gemini":
			if ok, _ := platform.CheckGemini(mediaClient); ok {
				res.Gemini = true
			}
		case "iprisk":
			country, ip := proxyutils.GetProxyCountry(mediaClient)
			if ip == "" {
				break
			}
			res.IP = ip
			res.Country = country
			risk, err := platform.CheckIPRisk(mediaClient, ip)
			if err == nil {
				res.IPRisk = risk
			}
		case "tiktok":
			if region, _ := platform.CheckTikTok(mediaClient); region != "" {
				res.TikTok = region
			}
		}
	}
}

// runFullTwoStage 完整两阶段模式
// 阶段1：高并发连通性筛选
// 阶段2：低并发全量测速
func (tc *TwoStageChecker) runFullTwoStage() ([]Result, error) {
	totalStartTime := time.Now()

	// 阶段1：连通性筛选
	candidates, phase1Duration := tc.runPhase1()
	if len(candidates) == 0 {
		slog.Warn("阶段1完成", "耗时", formatDuration(phase1Duration), "候选节点数", 0)
		return []Result{}, nil
	}

	slog.Info("阶段1完成", "耗时", formatDuration(phase1Duration), "候选节点数", len(candidates))

	// 阶段2：全量测速
	stage2Results, phase2Duration := tc.runPhase2(candidates)

	slog.Info("阶段2完成", "耗时", formatDuration(phase2Duration), "通过速度测试节点数", len(stage2Results))

	// 最终处理
	finalResults := tc.finalizeResults(candidates, stage2Results)

	slog.Info("可用节点数量", "数量", len(finalResults))
	slog.Info("测试总消耗流量", "流量", fmt.Sprintf("%.3fGB", float64(TotalBytes.Load())/1024/1024/1024))

	totalDuration := time.Since(totalStartTime)
	slog.Info("检测完成", "总耗时", formatDuration(totalDuration))

	return finalResults, nil
}

// formatDuration 格式化时间间隔为易读格式
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f秒", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%d分%d秒", minutes, seconds)
}

// runPhase1 执行阶段1连通性筛选
// 高并发检测，禁用早停，使用硬超时
func (tc *TwoStageChecker) runPhase1() ([]map[string]any, time.Duration) {
	proxyCount := len(tc.proxies)
	threadCount := tc.phase1Threads
	if proxyCount < threadCount {
		threadCount = proxyCount
	}

	startTime := time.Now()
	slog.Info("阶段1开始", "总节点数", proxyCount, "并发数", threadCount)

	// 创建 ProxyChecker，禁用早停
	pc := &ProxyChecker{
		results:          make([]Result, 0),
		proxyCount:       proxyCount,
		threadCount:      threadCount,
		resultChan:       make(chan Result),
		tasks:            make(chan map[string]any, 1),
		disableEarlyStop: true, // 阶段1禁用早停
		stageName:        "阶段1-连通性检测",
	}

	ProxyCount.Store(uint32(proxyCount))
	Progress.Store(0)
	Available.Store(0)

	done := make(chan bool)
	if config.GlobalConfig.PrintProgress {
		go pc.showProgress(done)
	}

	var candidates []map[string]any
	var candidatesMu sync.Mutex

	var wg sync.WaitGroup
	hardTimeoutSec := calculateHardTimeout()

	for i := 0; i < threadCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxy := range pc.tasks {
				httpClient := CreateClient(proxy)
				if httpClient == nil {
					pc.incrementProgress()
					continue
				}

				alive, err := checkAliveWithHardTimeout(httpClient.Client, hardTimeoutSec)
				httpClient.Close()

				if err == nil && alive {
					candidatesMu.Lock()
					candidates = append(candidates, proxy)
					candidatesMu.Unlock()
					pc.incrementAvailable()
				}
				pc.incrementProgress()
			}
		}()
	}

	go pc.distributeProxies(tc.proxies)

	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	if config.GlobalConfig.PrintProgress {
		done <- true
	}

	return candidates, time.Since(startTime)
}

// runPhase2 执行阶段2全量测速
// 对候选节点进行测速，禁用早停
func (tc *TwoStageChecker) runPhase2(candidates []map[string]any) ([]Result, time.Duration) {
	proxyCount := len(candidates)
	threadCount := tc.effectivePhase2Threads
	if proxyCount < threadCount {
		threadCount = proxyCount
	}

	startTime := time.Now()
	slog.Info("阶段2开始", "候选节点数", proxyCount, "并发数", threadCount)

	// 创建 ProxyChecker，禁用早停
	pc := &ProxyChecker{
		results:          make([]Result, 0),
		proxyCount:       proxyCount,
		threadCount:      threadCount,
		resultChan:       make(chan Result),
		tasks:            make(chan map[string]any, 1),
		disableEarlyStop: true, // 阶段2禁用早停
		stageName:        "阶段2-速度检测",
	}

	ProxyCount.Store(uint32(proxyCount))
	Progress.Store(0)
	Available.Store(0)

	done := make(chan bool)
	if config.GlobalConfig.PrintProgress {
		go pc.showProgress(done)
	}

	var wg sync.WaitGroup
	for i := 0; i < threadCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for proxy := range pc.tasks {
				result := tc.checkProxyPhase2(proxy)
				if result != nil {
					pc.resultChan <- *result
					pc.incrementAvailable()
				}
				pc.incrementProgress()
			}
		}()
	}

	go pc.distributeProxies(candidates)

	var collectWg sync.WaitGroup
	collectWg.Add(1)
	go func() {
		pc.collectResults()
		collectWg.Done()
	}()

	wg.Wait()
	close(pc.resultChan)
	collectWg.Wait()
	time.Sleep(100 * time.Millisecond)

	if config.GlobalConfig.PrintProgress {
		done <- true
	}

	return pc.results, time.Since(startTime)
}

// checkProxyPhase2 阶段2检测单个代理
// 执行测速、min-speed 过滤、平台检测、重命名，添加速度标签
func (tc *TwoStageChecker) checkProxyPhase2(proxy map[string]any) *Result {
	res := &Result{
		Proxy: proxy,
	}

	httpClient := CreateClient(proxy)
	if httpClient == nil {
		slog.Debug(fmt.Sprintf("创建代理Client失败: %v", proxy["name"]))
		return nil
	}
	defer httpClient.Close()

	// 测速
	speed, _, err := platform.CheckSpeed(httpClient.Client, Bucket, httpClient.BytesRead)
	if err != nil || speed < config.GlobalConfig.MinSpeed {
		return nil
	}

	// 记录速度
	res.SpeedKBps = speed

	// 平台检测（可选）
	if config.GlobalConfig.MediaCheck {
		tc.performMediaCheck(res, httpClient)
	}

	// 重命名并添加速度标签
	pc := &ProxyChecker{}
	pc.updateProxyNameEx(res, httpClient, speed, true) // includeSpeedTag = true

	return res
}

// finalizeResults 最终处理（统计、过滤、排序、截断）
// 1. 调用成功率统计
// 2. 应用 FilterResults
// 3. 按 SpeedKBps 降序排序
// 4. TopN 截断（success-limit）
func (tc *TwoStageChecker) finalizeResults(candidates []map[string]any, stage2Results []Result) []Result {
	// 1. 成功率统计（在过滤和截断之前）
	checkSubscriptionSuccessRateWithResults(candidates, stage2Results)

	// 2. 应用过滤规则
	filteredResults := FilterResults(stage2Results)

	// 3. 按 SpeedKBps 降序排序
	sort.Slice(filteredResults, func(i, j int) bool {
		return filteredResults[i].SpeedKBps > filteredResults[j].SpeedKBps
	})

	// 4. TopN 截断
	if config.GlobalConfig.SuccessLimit > 0 && len(filteredResults) > int(config.GlobalConfig.SuccessLimit) {
		filteredResults = filteredResults[:config.GlobalConfig.SuccessLimit]
		slog.Info(fmt.Sprintf("应用 TopN 截断，保留前 %d 个节点", config.GlobalConfig.SuccessLimit))
	}

	return filteredResults
}
