package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 协议常量：取自实测值，写死、不可配置。
const (
	probeURL           = "http://5.5.5.5/"
	portalBase         = "https://10.0.128.3"
	portalReferer      = portalBase + "/portal/webauth"
	uaString           = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
	browserFingerprint = "14bcc4ceac99a31f5bf799cc6b0e6360"

	probeTimeout = 5 * time.Second  // 探测超时
	postTimeout  = 10 * time.Second // webauth / webdisconn 超时
)

// sixParamKeys webauth 六参数 = 最近一次探测 302 Location 的前六个字段。
var sixParamKeys = []string{"wlanacip", "wlanacname", "wlanuserip", "mac", "vlan", "url"}

// ProbeResult 5.5.5.5 探测结果。「不在校园网」状态不走此结构（直接返回 error）。
type ProbeResult struct {
	State    string     // "online" | "offline"
	Values   url.Values // Location 全部参数（URL decode 后）
	Distoken string     // 在线态免费携带的新鲜下线凭据
	ErrMsg   string     // 门户附带文案（如顶号提示），仅展示不参与逻辑
}

// SixParams 提取 webauth 所需六参数（mac 保持 302 下发的原样小写冒号格式）。
func (p *ProbeResult) SixParams() url.Values {
	v := url.Values{}
	for _, k := range sixParamKeys {
		v.Set(k, p.Values.Get(k))
	}
	return v
}

// HasParams 五个核心参数齐备即可发起 webauth（url 允许为空）。
func (p *ProbeResult) HasParams() bool {
	for _, k := range sixParamKeys[:5] {
		if p.Values.Get(k) == "" {
			return false
		}
	}
	return true
}

// newHTTPClient 每次操作独立构建：绑定出口网口源 IP（Dialer.LocalAddr），
// Portal 为自签证书 → InsecureSkipVerify（仅访问 10.0.128.3 / 5.5.5.5 两个地址），
// 全部请求禁跟随重定向（探测靠读 Location 判态）。
func newHTTPClient(localIP net.IP, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: timeout}
	if localIP != nil {
		d.LocalAddr = &net.TCPAddr{IP: localIP}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:       d.DialContext,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// PortalProbe 三态判定唯一依据：GET http://5.5.5.5/（禁跟随重定向，超时 5s）。
//
//	302 且 Location 含 act=LOGINSUCC → 已认证
//	302 且无 act（Location 指向 /portal/webauth）→ 未认证（顺带取六参数与 distoken）
//	超时 / 连接失败 / 非 302 / Location 不含 /portal/webauth → 不在校园网（error）
func PortalProbe(localIP net.IP) (*ProbeResult, error) {
	req, err := http.NewRequest(http.MethodGet, probeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", uaString)
	client := newHTTPClient(localIP, probeTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("探测请求失败（%v）", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode != http.StatusFound {
		return nil, fmt.Errorf("探测响应非 302（HTTP %d）", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, errors.New("探测 302 缺少 Location")
	}
	if !strings.Contains(loc, "/portal/webauth") {
		return nil, fmt.Errorf("302 Location 非门户认证页（%s）", truncate(loc, 120))
	}
	u, err := url.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("Location 解析失败（%v）", err)
	}
	q := u.Query()
	res := &ProbeResult{Values: q}
	if q.Get("act") == "LOGINSUCC" {
		res.State = "online"
		res.Distoken = q.Get("distoken")
		res.ErrMsg = q.Get("errorMsg")
	} else {
		res.State = "offline"
	}
	return res, nil
}

// PortalReply 业务应答。HTTP 恒为 200，成败只看 body 的 success。
type PortalReply struct {
	Success bool
	Code    int
	Message string
}

func postForm(localIP net.IP, path string, form url.Values) (*PortalReply, error) {
	req, err := http.NewRequest(http.MethodPost, portalBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", portalReferer)
	req.Header.Set("User-Agent", uaString)
	client := newHTTPClient(localIP, postTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败（%v）", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败（%v）", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var jr struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Code    int    `json:"code"`
		Result  *struct {
			Message string `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return nil, fmt.Errorf("响应 JSON 解析失败（%v，原文：%s）", err, truncate(string(raw), 200))
	}
	msg := jr.Message
	if jr.Result != nil && jr.Result.Message != "" {
		msg = jr.Result.Message
	}
	return &PortalReply{Success: jr.Success, Code: jr.Code, Message: msg}, nil
}

// PortalWebauth 登录：11 字段全部写死来源 —— 前 6 个 = 探测 Location，userId/passwd = 配置，
// 其余 3 个 = 代码常量。mac 保持 302 下发的原样。
func PortalWebauth(localIP net.IP, six url.Values, user, pass string) (*PortalReply, error) {
	form := url.Values{}
	for k, vs := range six {
		form[k] = vs
	}
	form.Set("userId", user)
	form.Set("passwd", pass)
	form.Set("loginMode", "password")
	form.Set("recordTerminalMac", "Y")
	form.Set("browserFingerprint", browserFingerprint)
	return postForm(localIP, "/api/portal/webauth", form)
}

// PortalWebdisconn 下线：distoken 必填且须新鲜（每次下线前先探测现取）；
// mac 提交时把 302 下发的小写转大写；pageid/act/ssid 原样空串。
func PortalWebdisconn(localIP net.IP, six url.Values, distoken string) (*PortalReply, error) {
	form := url.Values{}
	for k, vs := range six {
		form[k] = vs
	}
	form.Set("mac", strings.ToUpper(six.Get("mac")))
	form.Set("distoken", distoken)
	form.Set("pageid", "")
	form.Set("act", "")
	form.Set("ssid", "")
	return postForm(localIP, "/api/portal/webdisconn", form)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
