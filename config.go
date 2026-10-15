package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// 默认值
const (
	defaultListen         = "0.0.0.0:10086"
	defaultLogLevel       = "info"
	defaultLogFile        = "campus-auth.log"
	defaultLogMaxMB       = 4
	defaultCheckInterval  = 15
	defaultLoginRetries   = 3
	defaultRetryInterval  = 10
	defaultRejectCooldown = 600
)

type WebConfig struct {
	Listen   string `json:"listen"`
	Password string `json:"password"`
}

type LogConfig struct {
	Level string `json:"level"`
	File  string `json:"file"`
	MaxMB int    `json:"max_mb"`
}

type IfaceConfig struct {
	Name     string `json:"name,omitempty"`
	Mac      string `json:"mac,omitempty"`
	Mode     string `json:"mode"`
	Probe    bool   `json:"probe"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// Key 网口唯一标识：name 优先，其次 mac（同时用于 WebUI API 路径）。
func (c IfaceConfig) Key() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Mac
}

func (c IfaceConfig) Locator() string {
	if c.Name != "" {
		return "name=" + c.Name
	}
	return "mac=" + c.Mac
}

// EffectiveProbe probe 仅在 manual 模式生效；auto 模式强制视为 true。
func (c IfaceConfig) EffectiveProbe() bool {
	return c.Mode == "auto" || c.Probe
}

type Config struct {
	Web            WebConfig     `json:"web"`
	Log            LogConfig     `json:"log"`
	CheckInterval  float64       `json:"check_interval"`
	LoginRetries   int           `json:"login_retries"`
	RetryInterval  float64       `json:"retry_interval"`
	RejectCooldown float64       `json:"reject_cooldown"`
	Interfaces     []IfaceConfig `json:"interfaces"`
}

// ---------- 解析与校验（启动加载与网页保存共用） ----------

// 指针影子结构：区分「字段缺失」与「字段为零值」，缺失走默认，显式非法值拒绝。
type rawConfig struct {
	Web *struct {
		Listen   *string `json:"listen"`
		Password *string `json:"password"`
	} `json:"web"`
	Log *struct {
		Level *string `json:"level"`
		File  *string `json:"file"`
		MaxMB *int    `json:"max_mb"`
	} `json:"log"`
	CheckInterval  *float64    `json:"check_interval"`
	LoginRetries   *int        `json:"login_retries"`
	RetryInterval  *float64    `json:"retry_interval"`
	RejectCooldown *float64    `json:"reject_cooldown"`
	Interfaces     *[]rawIface `json:"interfaces"`
}

type rawIface struct {
	Name     *string `json:"name"`
	Mac      *string `json:"mac"`
	Mode     *string `json:"mode"`
	Probe    *bool   `json:"probe"`
	User     *string `json:"user"`
	Password *string `json:"password"`
}

func ParseConfig(data []byte) (*Config, error) {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("JSON 解析失败：%v", err)
	}
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	cfg := &Config{
		CheckInterval:  defaultCheckInterval,
		LoginRetries:   defaultLoginRetries,
		RetryInterval:  defaultRetryInterval,
		RejectCooldown: defaultRejectCooldown,
	}

	if raw.Web == nil {
		add("缺少 web 配置")
	} else {
		if raw.Web.Listen != nil {
			cfg.Web.Listen = strings.TrimSpace(*raw.Web.Listen)
			if cfg.Web.Listen == "" {
				add("web.listen 不能为空")
			}
		} else {
			cfg.Web.Listen = defaultListen
		}
		if raw.Web.Password != nil {
			cfg.Web.Password = *raw.Web.Password // 密码不 trim，允许任意字符
			if cfg.Web.Password == "" {
				add("web.password 不能为空")
			}
		} else {
			add("web.password 不能为空")
		}
	}

	if raw.Log == nil {
		cfg.Log = LogConfig{Level: defaultLogLevel, File: defaultLogFile, MaxMB: defaultLogMaxMB}
	} else {
		cfg.Log.Level = defaultLogLevel
		if raw.Log.Level != nil {
			cfg.Log.Level = *raw.Log.Level
		}
		switch cfg.Log.Level {
		case "debug", "info", "error":
		default:
			add("log.level 必须为 debug/info/error")
		}
		cfg.Log.File = defaultLogFile
		if raw.Log.File != nil {
			if f := strings.TrimSpace(*raw.Log.File); f != "" {
				cfg.Log.File = f
			}
		}
		cfg.Log.MaxMB = defaultLogMaxMB
		if raw.Log.MaxMB != nil {
			cfg.Log.MaxMB = *raw.Log.MaxMB
			if cfg.Log.MaxMB <= 0 {
				add("log.max_mb 必须大于 0")
			}
		}
	}

	if raw.CheckInterval != nil {
		cfg.CheckInterval = *raw.CheckInterval
	}
	if !(cfg.CheckInterval > 0) {
		add("check_interval 必须大于 0")
	}
	if raw.LoginRetries != nil {
		cfg.LoginRetries = *raw.LoginRetries
	}
	if cfg.LoginRetries < 0 {
		add("login_retries 必须为不小于 0 的整数")
	}
	if raw.RetryInterval != nil {
		cfg.RetryInterval = *raw.RetryInterval
	}
	if !(cfg.RetryInterval > 0) {
		add("retry_interval 必须大于 0")
	}
	if raw.RejectCooldown != nil {
		cfg.RejectCooldown = *raw.RejectCooldown
	}
	if cfg.RejectCooldown < 0 {
		add("reject_cooldown 不能小于 0")
	}

	if raw.Interfaces == nil || len(*raw.Interfaces) == 0 {
		add("interfaces 必须为非空数组")
	} else {
		seen := map[string]bool{}
		for i, ri := range *raw.Interfaces {
			ic := IfaceConfig{Mode: "manual", Probe: true}
			if ri.Name != nil {
				ic.Name = strings.TrimSpace(*ri.Name)
			}
			if ri.Mac != nil {
				ic.Mac = strings.TrimSpace(*ri.Mac)
			}
			if ic.Name == "" && ic.Mac == "" {
				add("interfaces[%d]：name / mac 需至少填写一项（定位出口网口）", i)
			}
			if ri.Mode != nil && *ri.Mode != "" {
				ic.Mode = *ri.Mode
			}
			switch ic.Mode {
			case "manual", "auto":
			default:
				add("interfaces[%d].mode 必须为 manual 或 auto", i)
			}
			if ri.Probe != nil {
				ic.Probe = *ri.Probe
			}
			if ri.User != nil {
				ic.User = strings.TrimSpace(*ri.User)
			}
			if ic.User == "" {
				add("interfaces[%d].user 不能为空", i)
			}
			if ri.Password != nil {
				ic.Password = *ri.Password
			}
			if ic.Password == "" {
				add("interfaces[%d].password 不能为空", i)
			}
			key := ic.Key()
			if seen[key] {
				add("interfaces[%d]：网口定位重复（%s）", i, key)
			}
			seen[key] = true
			cfg.Interfaces = append(cfg.Interfaces, ic)
		}
	}

	if len(errs) > 0 {
		return nil, errors.New("配置校验失败：" + strings.Join(errs, "；"))
	}
	return cfg, nil
}

// ---------- 落盘 ----------

func SaveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// defaultConfig 首次运行自动生成的默认配置。
func defaultConfig() *Config {
	return &Config{
		Web:            WebConfig{Listen: defaultListen, Password: "password"},
		Log:            LogConfig{Level: defaultLogLevel, File: defaultLogFile, MaxMB: defaultLogMaxMB},
		CheckInterval:  defaultCheckInterval,
		LoginRetries:   defaultLoginRetries,
		RetryInterval:  defaultRetryInterval,
		RejectCooldown: defaultRejectCooldown,
		Interfaces: []IfaceConfig{
			{Name: "以太网", Mode: "manual", Probe: true, User: "admin", Password: "password"},
			{Name: "wlan0", Mode: "auto", Probe: true, User: "admin", Password: "password"},
		},
	}
}

// loadOrCreateConfig 加载配置；文件不存在时生成默认配置并继续运行。
func loadOrCreateConfig(path string) (*Config, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		cfg, perr := ParseConfig(data)
		if perr != nil {
			return nil, false, fmt.Errorf("%s：%v", path, perr)
		}
		return cfg, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("读取配置失败：%v", err)
	}
	cfg := defaultConfig()
	if err := SaveConfig(path, cfg); err != nil {
		return nil, false, fmt.Errorf("默认配置写入 %s 失败：%v", path, err)
	}
	return cfg, true, nil
}
