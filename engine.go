package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stateUnprobed = "unprobed"
	stateOnline   = "online"
	stateOffline  = "offline"
	stateOut      = "out"

	// dialWait 登录受理成功后、确认探测前的固定拨号窗口（实测锚定 3s）。
	dialWait = 3 * time.Second
)

func stateName(s string) string {
	switch s {
	case stateOnline:
		return "已认证"
	case stateOffline:
		return "未认证"
	case stateOut:
		return "不在校园网"
	case stateUnprobed:
		return "未探测"
	}
	return s
}

func modeName(m string) string {
	if m == "auto" {
		return "自动模式"
	}
	return "手动模式"
}

// ---------- Manager：配置的运行时持有者，管理全部网口 worker ----------

type Manager struct {
	mu      sync.Mutex
	cfg     *Config
	cfgPath string
	baseDir string // 二进制所在目录（日志相对路径基准）
	log     *Logger
	workers map[string]*Worker
}

func NewManager(cfg *Config, cfgPath, baseDir string, log *Logger) *Manager {
	return &Manager{cfg: cfg, cfgPath: cfgPath, baseDir: baseDir, log: log, workers: map[string]*Worker{}}
}

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildLocked()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workers {
		w.Stop()
	}
	m.workers = map[string]*Worker{}
}

// rebuildLocked 重建全部网口 worker（配置即时生效的实现方式）。
func (m *Manager) rebuildLocked() {
	for _, w := range m.workers {
		w.Stop()
	}
	m.workers = make(map[string]*Worker, len(m.cfg.Interfaces))
	interval := time.Duration(m.cfg.CheckInterval * float64(time.Second))
	for _, ic := range m.cfg.Interfaces {
		w := newWorker(m, ic)
		m.workers[ic.Key()] = w
		go w.loop(interval)
		m.log.Info(ic.Key(), "网口协程已启动（%s，探测周期 %v）", modeName(ic.Mode), interval)
	}
}

// WithWorker 取 worker 执行回调；网口不存在返回 false。
func (m *Manager) WithWorker(key string, fn func(*Worker)) bool {
	m.mu.Lock()
	w := m.workers[key]
	m.mu.Unlock()
	if w == nil {
		return false
	}
	fn(w)
	return true
}

func (m *Manager) Worker(key string) *Worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[key]
}

// Status 按配置顺序输出全部网口状态快照（/api/status）。
func (m *Manager) Status() []IfaceStatus {
	m.mu.Lock()
	ifaces := make([]IfaceConfig, len(m.cfg.Interfaces))
	copy(ifaces, m.cfg.Interfaces)
	workers := make(map[string]*Worker, len(m.workers))
	for k, w := range m.workers {
		workers[k] = w
	}
	m.mu.Unlock()
	out := make([]IfaceStatus, 0, len(ifaces))
	for _, ic := range ifaces {
		if w := workers[ic.Key()]; w != nil {
			out = append(out, w.Status())
		}
	}
	return out
}

// ToggleMode 模式切换：改配置 → 落盘 → 热更新 worker（不重建，状态保留）。
// 切到 auto 时 probe 落盘为 true（auto 模式强制视为 true）。
func (m *Manager) ToggleMode(key string) (string, error) {
	m.mu.Lock()
	w := m.workers[key]
	idx := -1
	for i := range m.cfg.Interfaces {
		if m.cfg.Interfaces[i].Key() == key {
			idx = i
			break
		}
	}
	if w == nil || idx < 0 {
		m.mu.Unlock()
		return "", fmt.Errorf("网口不存在：%s", key)
	}
	ic := &m.cfg.Interfaces[idx]
	if ic.Mode == "auto" {
		ic.Mode = "manual"
	} else {
		ic.Mode = "auto"
		ic.Probe = true
	}
	newMode := ic.Mode
	saveErr := SaveConfig(m.cfgPath, m.cfg)
	m.mu.Unlock()

	w.SetMode(newMode, ic.EffectiveProbe())
	if saveErr != nil {
		m.log.Error(key, "模式切换后配置落盘失败：%v", saveErr)
	}
	m.log.Info(key, "运行模式已切换为%s", modeName(newMode))
	return newMode, nil
}

// UpdateConfig 网页保存配置：校验 → 落盘 → 日志重配置 + 重建全部 worker。
// 仅 web.listen 变更需重启进程（以 notes 提示）。
func (m *Manager) UpdateConfig(data []byte) (notes []string, err error) {
	newCfg, perr := ParseConfig(data)
	if perr != nil {
		return nil, perr
	}
	m.mu.Lock()
	old := m.cfg
	if newCfg.Web.Listen != old.Web.Listen {
		notes = append(notes, "web.listen 变更需重启进程后生效")
	}
	if newCfg.Web.Password != old.Web.Password {
		notes = append(notes, "管理密码已即时更新")
	}
	m.cfg = newCfg
	saveErr := SaveConfig(m.cfgPath, newCfg)
	m.log.Reconfigure(newCfg.Log.Level, newCfg.Log.File, newCfg.Log.MaxMB, m.baseDir)
	m.rebuildLocked()
	m.mu.Unlock()

	if saveErr != nil {
		m.log.Error("系统", "配置落盘失败：%v", saveErr)
		notes = append(notes, "警告：配置落盘失败，重启后可能回退："+saveErr.Error())
	}
	m.log.Info("系统", "配置已更新并即时生效（网口协程已重建）")
	return notes, nil
}

func (m *Manager) ConfigJSON() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.MarshalIndent(m.cfg, "", "  ")
}

func (m *Manager) Password() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Web.Password
}

// loginParams 自动登录序列参数。
func (m *Manager) loginParams() (retries int, retryInterval, cooldown time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.LoginRetries,
		time.Duration(m.cfg.RetryInterval * float64(time.Second)),
		time.Duration(m.cfg.RejectCooldown * float64(time.Second))
}

// ---------- Worker：单网口状态机 + 全部动作 ----------

type IfaceStatus struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	Mac           string `json:"mac"`
	Mode          string `json:"mode"`
	Probe         bool   `json:"probe"`
	User          string `json:"user"`
	State         string `json:"state"`
	Rejected      bool   `json:"rejected"`
	RejectMessage string `json:"reject_message"`
	CooldownUntil int64  `json:"cooldown_until"`
	IP            string `json:"ip"`
	HWMac         string `json:"hw_mac"`
	LastAction    string `json:"last_action"`
	LastProbeAt   int64  `json:"last_probe_at"`
}

type ActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type Worker struct {
	mgr *Manager
	cfg IfaceConfig // Name/Mac/User/Password 静态；Mode/Probe 走原子字段

	mode      atomic.Value // string
	probeFlag atomic.Bool

	sem      chan struct{} // 操作互斥：周期探测与网页动作、自动登录序列互斥
	stop     chan struct{}
	stopOnce sync.Once

	mu            sync.Mutex
	state         string
	ip, hwmac     string
	lastAction    string
	lastProbeAt   time.Time
	rejectMsg     string
	cooldownUntil time.Time
	lastProbe     *ProbeResult
}

func newWorker(mgr *Manager, ic IfaceConfig) *Worker {
	w := &Worker{
		mgr:   mgr,
		cfg:   ic,
		sem:   make(chan struct{}, 1),
		stop:  make(chan struct{}),
		state: stateUnprobed,
	}
	w.mode.Store(ic.Mode)
	w.probeFlag.Store(ic.EffectiveProbe())
	return w
}

func (w *Worker) Key() string     { return w.cfg.Key() }
func (w *Worker) Mode() string    { return w.mode.Load().(string) }
func (w *Worker) ProbeFlag() bool { return w.probeFlag.Load() }
func (w *Worker) SetMode(mode string, probe bool) {
	w.mode.Store(mode)
	w.probeFlag.Store(probe)
}
func (w *Worker) Stop() { w.stopOnce.Do(func() { close(w.stop) }) }

func (w *Worker) logDebug(f string, a ...any) { w.mgr.log.Debug(w.Key(), f, a...) }
func (w *Worker) logInfo(f string, a ...any)  { w.mgr.log.Info(w.Key(), f, a...) }
func (w *Worker) logError(f string, a ...any) { w.mgr.log.Error(w.Key(), f, a...) }

func (w *Worker) sleep(d time.Duration) bool { // false = 已停止
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-w.stop:
		return false
	case <-t.C:
		return true
	}
}

func (w *Worker) TryAcquire() bool {
	select {
	case w.sem <- struct{}{}:
		return true
	default:
		return false
	}
}
func (w *Worker) Release() { <-w.sem }

// loop 探测循环。manual + probe=false 不探测（界面保持「未探测」）；操作进行中跳过本周期
// （自动登录序列期间暂停探测循环）。
func (w *Worker) loop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			if !w.TryAcquire() {
				w.logDebug("上一操作仍在进行，跳过本周期探测")
				continue
			}
			w.tick()
			w.Release()
		}
	}
}

func (w *Worker) tick() {
	mode := w.Mode()
	if mode != "auto" && !w.ProbeFlag() {
		return // manual + probe=false：不探测
	}
	res, err := w.probeOnce()
	if err != nil {
		w.markOut(err)
		return
	}
	w.applyProbe(res)
	if mode == "auto" && res.State == stateOffline {
		w.mu.Lock()
		cooling := time.Now().Before(w.cooldownUntil)
		w.mu.Unlock()
		if cooling {
			w.logDebug("拒绝冷却中，跳过自动登录")
			return
		}
		w.autoLoginSequence()
	}
}

// probeOnce 每次操作前重新解析出口网口 IPv4（网口不存在/无 IP 即「不在校园网」，周期自愈）。
func (w *Worker) probeOnce() (*ProbeResult, error) {
	ip, err := resolveSourceIP(w.cfg)
	if err != nil {
		return nil, err
	}
	return PortalProbe(ip)
}

func (w *Worker) markOut(err error) {
	w.mu.Lock()
	prev := w.state
	w.state = stateOut
	w.lastProbe = nil
	w.lastAction = "探测失败：" + err.Error()
	w.mu.Unlock()
	if prev != stateOut {
		w.logInfo("状态变化：%s → 不在校园网（%v）", stateName(prev), err)
	} else {
		w.logDebug("探测失败：%v", err)
	}
}

func (w *Worker) applyProbe(res *ProbeResult) {
	var action string
	if res.State == stateOnline {
		action = "探测：已认证（302 含 act=LOGINSUCC，已记录新鲜下线凭据）"
		if res.ErrMsg != "" {
			action += "；门户提示：" + res.ErrMsg
		}
	} else {
		action = "探测：未认证（302 无 act，已解析六参数备用）"
	}
	w.mu.Lock()
	prev := w.state
	w.state = res.State
	w.lastProbe = res
	w.lastProbeAt = time.Now()
	w.ip = res.Values.Get("wlanuserip")
	w.hwmac = res.Values.Get("mac")
	if res.State == stateOnline {
		w.rejectMsg = ""
	}
	w.lastAction = action
	w.mu.Unlock()
	if prev != res.State {
		w.logInfo("状态变化：%s → %s", stateName(prev), stateName(res.State))
	} else {
		w.logDebug("%s", action)
	}
}

func (w *Worker) setActionf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	w.mu.Lock()
	w.lastAction = msg
	w.mu.Unlock()
}

func (w *Worker) lastActionText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastAction
}

// markRejected 明确拒绝（密码错/欠费/停机等，success=false）：状态=拒绝(原文)，进入冷却。
func (w *Worker) markRejected(msg string) {
	_, _, cooldown := w.mgr.loginParams()
	until := time.Now().Add(cooldown)
	w.mu.Lock()
	w.rejectMsg = msg
	w.cooldownUntil = until
	w.state = stateOffline
	w.lastAction = "认证被拒绝：\"" + msg + "\""
	w.mu.Unlock()
	w.logError("认证被拒绝：\"%s\"，自动登录冷却至 %s", msg, until.Format("15:04:05"))
}

// autoLoginSequence：最多 1+retries 次提交；明确拒绝 → 冷却终止；
// 受理成功 → 等 3s 拨号窗口 → 探测确认；重试耗尽 → 等下一探测周期（不加额外冷却）。
func (w *Worker) autoLoginSequence() {
	retries, retryInterval, _ := w.mgr.loginParams()

	// 网页触发时可能没有近期探测，先确保手上有认证参数
	w.mu.Lock()
	hasParams := w.lastProbe != nil && w.lastProbe.HasParams()
	w.mu.Unlock()
	if !hasParams {
		res, err := w.probeOnce()
		if err != nil {
			w.markOut(err)
			w.setActionf("自动登录中止：探测失败（%v）", err)
			return
		}
		w.applyProbe(res)
		if !res.HasParams() {
			w.setActionf("自动登录中止：探测结果无法提供认证参数")
			return
		}
	}

	for attempt := 0; attempt <= retries; attempt++ {
		select {
		case <-w.stop:
			return
		default:
		}
		w.logInfo("自动登录：第 %d/%d 次提交", attempt+1, retries+1)

		rep, err := w.submitWebauth()
		if err != nil {
			// 动作型请求失败（超时/黑洞）：按本轮失败处理，走既有重试节奏
			w.setActionf("第 %d 次提交失败：%v", attempt+1, err)
			w.logInfo("第 %d 次提交失败：%v", attempt+1, err)
		} else if !rep.Success {
			w.markRejected(rep.Message) // 明确拒绝（含未知文案）
			return
		} else {
			w.setActionf("受理成功：\"%s\"，等待 %d 秒拨号窗口后确认", rep.Message, int(dialWait.Seconds()))
			if !w.sleep(dialWait) {
				return
			}
			res, perr := w.probeOnce()
			if perr != nil {
				w.markOut(perr)
				w.setActionf("第 %d 次提交后确认探测失败：%v", attempt+1, perr)
				w.logInfo("第 %d 次提交后确认探测失败：%v", attempt+1, perr)
			} else {
				w.applyProbe(res)
				if res.State == stateOnline {
					w.logInfo("自动登录：探测确认已认证，序列完成")
					w.setActionf("自动登录成功：探测确认已认证")
					return
				}
				w.setActionf("第 %d 次提交受理但探测确认仍未认证", attempt+1)
				w.logInfo("第 %d 次提交受理但探测确认仍未认证", attempt+1)
			}
		}
		if attempt < retries {
			if !w.sleep(retryInterval) {
				return
			}
		}
	}
	w.setActionf("自动登录重试耗尽，仍未确认认证，等待下一探测周期")
	w.logInfo("自动登录：重试耗尽仍未确认")
}

// submitWebauth 用最近一次探测的六参数提交（序列期间不重复探测）。
func (w *Worker) submitWebauth() (*PortalReply, error) {
	w.mu.Lock()
	lp := w.lastProbe
	w.mu.Unlock()
	if lp == nil || !lp.HasParams() {
		return nil, errors.New("无可用认证参数")
	}
	ip, err := resolveSourceIP(w.cfg)
	if err != nil {
		return nil, err
	}
	return PortalWebauth(ip, lp.SixParams(), w.cfg.User, w.cfg.Password)
}

// ---------- 网页触发的动作（WebUI 按钮） ----------

// OpProbe 立即探测。
func (w *Worker) OpProbe() ActionResult {
	if !w.TryAcquire() {
		return ActionResult{false, "该网口正忙（探测/登录序列进行中），请稍后再试"}
	}
	defer w.Release()
	res, err := w.probeOnce()
	if err != nil {
		w.markOut(err)
		return ActionResult{false, "探测失败：" + err.Error()}
	}
	w.applyProbe(res)
	return ActionResult{true, w.lastActionText()}
}

// OpLogin 网页触发登录：
//
//	manual：单发一次 POST webauth（用最近一次探测的六参数），不确认、不重试、不管结果；
//	auto  ：触发登录序列（异步执行，结果见状态卡片与日志）。
func (w *Worker) OpLogin() ActionResult {
	if !w.TryAcquire() {
		return ActionResult{false, "该网口正忙（探测/登录序列进行中），请稍后再试"}
	}
	if w.Mode() == "auto" {
		go func() {
			defer w.Release()
			w.autoLoginSequence()
		}()
		return ActionResult{true, "已触发自动登录序列，结果见状态卡片与日志"}
	}
	defer w.Release()

	// 手动单发：probe=false 或手上无参数时，先临时探测一次取参
	six, err := w.ensureParams()
	if err != nil {
		return ActionResult{false, err.Error()}
	}
	ip, err := resolveSourceIP(w.cfg)
	if err != nil {
		return ActionResult{false, err.Error()}
	}
	rep, err := PortalWebauth(ip, six, w.cfg.User, w.cfg.Password)
	if err != nil {
		w.setActionf("手动登录请求失败：%v", err)
		return ActionResult{false, "认证请求失败：" + err.Error()}
	}
	if !rep.Success {
		w.setActionf("手动登录被拒绝：\"%s\"", rep.Message)
		return ActionResult{false, "认证被拒绝：\"" + rep.Message + "\""}
	}
	msg := fmt.Sprintf("手动登录受理成功：\"%s\"（单发一次，状态由后续探测反映）", rep.Message)
	w.setActionf("%s", msg)
	return ActionResult{true, msg}
}

// ensureParams 取六参数：probe 开启且缓存有效时直接用最近一次探测；否则临时探测一次。
func (w *Worker) ensureParams() (url.Values, error) {
	w.mu.Lock()
	lp := w.lastProbe
	useCache := w.ProbeFlag() && lp != nil && lp.HasParams()
	var six url.Values
	if useCache {
		six = lp.SixParams()
	}
	w.mu.Unlock()
	if useCache {
		return six, nil
	}
	res, err := w.probeOnce()
	if err != nil {
		w.markOut(err)
		return nil, fmt.Errorf("临时探测失败：%v", err)
	}
	w.applyProbe(res)
	if !res.HasParams() {
		return nil, fmt.Errorf("探测结果无法提供认证参数（当前%s）", stateName(res.State))
	}
	return res.SixParams(), nil
}

// OpLogout 网页触发下线：每次先探测一次取新鲜 distoken → webdisconn → 完事。
func (w *Worker) OpLogout() ActionResult {
	if !w.TryAcquire() {
		return ActionResult{false, "该网口正忙（探测/登录序列进行中），请稍后再试"}
	}
	defer w.Release()

	res, err := w.probeOnce()
	if err != nil {
		w.markOut(err)
		return ActionResult{false, "探测失败：" + err.Error()}
	}
	w.applyProbe(res)
	if res.State != stateOnline {
		return ActionResult{false, "当前" + stateName(res.State) + "，无下线凭据，无需下线"}
	}
	ip, err := resolveSourceIP(w.cfg)
	if err != nil {
		return ActionResult{false, err.Error()}
	}
	rep, err := PortalWebdisconn(ip, res.SixParams(), res.Distoken)
	if err != nil {
		w.setActionf("下线请求失败：%v", err)
		return ActionResult{false, "下线请求失败：" + err.Error()}
	}
	if !rep.Success {
		w.setActionf("下线失败：\"%s\"", rep.Message)
		return ActionResult{false, "下线失败：\"" + rep.Message + "\""}
	}
	w.mu.Lock()
	w.state = stateOffline // BRAS 已确认下线；下一探测周期自然复核
	w.lastAction = "下线成功（webdisconn 响应：\"" + rep.Message + "\"）"
	w.mu.Unlock()
	w.logInfo("下线成功")
	return ActionResult{true, "下线成功"}
}

// Status 状态快照（WebUI /api/status）。
func (w *Worker) Status() IfaceStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	unixOrZero := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	return IfaceStatus{
		Key:           w.cfg.Key(),
		Name:          w.cfg.Name,
		Mac:           w.cfg.Mac,
		Mode:          w.Mode(),
		Probe:         w.ProbeFlag(),
		User:          w.cfg.User,
		State:         w.state,
		Rejected:      w.rejectMsg != "" && time.Now().Before(w.cooldownUntil),
		RejectMessage: w.rejectMsg,
		CooldownUntil: unixOrZero(w.cooldownUntil),
		IP:            w.ip,
		HWMac:         w.hwmac,
		LastAction:    w.lastAction,
		LastProbeAt:   unixOrZero(w.lastProbeAt),
	}
}

// ---------- 出口网口定位与源 IP 解析 ----------

// resolveSourceIP 按 name（OS 网口名）或 mac（网口自身 MAC）定位出口网口，取其 IPv4 作为源 IP。
// 网口不存在 / 无 IP → error（上层记为「不在校园网」，周期重试，不影响其他网口）。
func resolveSourceIP(cfg IfaceConfig) (net.IP, error) {
	var iface *net.Interface
	if cfg.Name != "" {
		var err error
		iface, err = net.InterfaceByName(cfg.Name)
		if err != nil {
			return nil, fmt.Errorf("找不到网口 %q（%v）", cfg.Name, err)
		}
	} else {
		want := normalizeMac(cfg.Mac)
		if want == "" {
			return nil, errors.New("无网口定位信息")
		}
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, err
		}
		for i := range ifaces {
			if normalizeMac(ifaces[i].HardwareAddr.String()) == want {
				iface = &ifaces[i]
				break
			}
		}
		if iface == nil {
			return nil, fmt.Errorf("找不到 MAC 为 %s 的网口", cfg.Mac)
		}
	}
	ip4 := firstIPv4(iface)
	if ip4 == nil {
		return nil, fmt.Errorf("网口 %q 无 IPv4 地址", iface.Name)
	}
	return ip4, nil
}

func normalizeMac(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, ".", "")
	return s
}

// firstIPv4 取网口首个 IPv4 地址，无则返回 nil（取址失败按无地址处理）。
// resolveSourceIP 与 ListNetifs 共用。
func firstIPv4(iface *net.Interface) net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}

// ---------- 本机网口清单（WebUI 设置页「从本机选择」数据源） ----------

// NetifInfo 单个本机网口的识别信息。
type NetifInfo struct {
	Name string `json:"name"` // 系统网口名
	Mac  string `json:"mac"`  // 物理地址（小写冒号格式，可能为空）
	IP   string `json:"ip"`   // 首个 IPv4（可能为空）
	Up   bool   `json:"up"`   // 网口是否启用
}

// ListNetifs 枚举本机全部网口（名称 / MAC / 首个 IPv4 / 启用状态），
// 供设置页在「系统网口名 / 网口物理地址」定位值上直接选择，免手动抄写。
func ListNetifs() []NetifInfo {
	ifaces, err := net.Interfaces()
	if err != nil {
		return []NetifInfo{}
	}
	out := make([]NetifInfo, 0, len(ifaces))
	for i := range ifaces {
		ni := NetifInfo{
			Name: ifaces[i].Name,
			Mac:  ifaces[i].HardwareAddr.String(),
			Up:   ifaces[i].Flags&net.FlagUp != 0,
		}
		if ip4 := firstIPv4(&ifaces[i]); ip4 != nil {
			ni.IP = ip4.String()
		}
		out = append(out, ni)
	}
	return out
}
