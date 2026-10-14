// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Provider 上游服务提供商。空值语义 = ProviderWorkBuddy（旧凭证文件零回归）。
type Provider string

const (
	ProviderWorkBuddy Provider = "workbuddy"
	ProviderTrae      Provider = "trae"
)

// AccountKey 池内账号主键：本项目自生成的 UUIDv4，与上游 uid 解耦。
// 形态为 8-4-4-4-12 的十六进制串（无 "/"），可安全用作面板路由参数与 map 键。
// 随 auth 文件持久化（account_key 键），保证重启/重载后稳定。
type AccountKey string

// NewAccountKey 生成一个新的 UUIDv4 主键（crypto/rand，无第三方依赖）。
func NewAccountKey() (AccountKey, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("account key rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return AccountKey(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])), nil
}

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	// provider 上游 provider（"workbuddy" / "trae"），落盘于顶层 provider 键。
	// 空 = 缺省：Provider() 回落 ProviderWorkBuddy。
	// 与 realm 同构（私有字段 + 导出访问器，Go 不允许字段与方法同名），
	// 区别是 provider 加载后不可变，访问器无需加锁。
	provider     Provider
	// key 池内主键（UUIDv4，落盘于顶层 account_key 键）。空 = 尚未生成，
	// 由 LoadDir / 登录导入路径生成并落盘；业务判定用 Key()。
	key          AccountKey
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// Trae 专属字段（provider==ProviderTrae 时有效）。字段名与落盘位置对照
	// trae2api/internal/auth/auth.go：都放在嵌套形 auth 对象内。
	//   ApiHost   ExchangeToken/GetUserInfo 的 host（如 https://api.trae.com.cn）
	//   MachineID x-machine-id 头
	//   DeviceID  x-device-id 头（签到要求每账号唯一 16 位数字）
	ApiHost   string
	MachineID string
	DeviceID  string
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// Provider 返回账号所属上游；空值回落 ProviderWorkBuddy（旧文件零回归）。
// provider 加载后不可变，故不加锁（realm 需要锁是因为 BackfillRealm/RefreshToken 会改写它）。
func (a *Auth) Provider() Provider {
	if a == nil || a.provider == "" {
		return ProviderWorkBuddy
	}
	return a.provider
}

// ProviderStored 直读持久化的 provider（可能为空 = 未 backfill 的旧文件）。
// 供 LoadDir 判断是否需要迁移；业务判定一律用 Provider()。
func (a *Auth) ProviderStored() Provider { return a.provider }

// Key 返回池内主键（UUIDv4）。尚未生成时返回空串——生成由 EnsureKey / LoadDir /
// 登录导入路径负责，Parse 保持只读。
// key 会被 EnsureKey 写入，故加锁（与 Realm() 同理）。
func (a *Auth) Key() AccountKey {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.key
}

// EnsureKey 保证账号有池内主键：缺则生成并写入内存字段，返回 (key, 是否新生成, error)。
// 只改内存，不落盘；持久化由调用方（LoadDir / 登录导入路径）SaveAtomic 负责。
// 池在账号入池时调用它，保证 entry 的 map 键恒非空。
func (a *Auth) EnsureKey() (AccountKey, bool, error) {
	if a == nil {
		return "", false, fmt.Errorf("nil auth")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.key != "" {
		return a.key, false, nil
	}
	k, err := NewAccountKey()
	if err != nil {
		return "", false, err
	}
	a.key = k
	return k, true, nil
}

// SetKey 显式写入池内主键（池在「沿用已有账号主键」时调用：调用方给的是没有
// 持久化主键的新对象，而池里已有同一账号的既有状态，必须以既有主键为准）。
// 空键忽略；只改内存，不落盘。
func (a *Auth) SetKey(k AccountKey) {
	if a == nil || k == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.key = k
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// 直读字段（EnterpriseID 自 Parse 后不再改写，与 upstream 读取方式一致），
// 无需加锁——Realm() 加锁是因为 realm/Domain 会被 RefreshToken 在锁内改写。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
				ApiHost      string `json:"apiHost"`
				MachineID    string `json:"machineId"`
				DeviceID     string `json:"deviceId"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
			// Provider 顶层 provider（本项目自有元数据，非上游输出）；缺失 = 旧文件。
			Provider string `json:"provider"`
			// AccountKey 顶层 account_key（本项目自生成的池内主键，UUIDv4）。
			AccountKey string `json:"account_key"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
			provider:     Provider(strings.TrimSpace(n.Provider)),
			key:          AccountKey(strings.TrimSpace(n.AccountKey)),
			ApiHost:      n.Auth.ApiHost,
			MachineID:    n.Auth.MachineID,
			DeviceID:     n.Auth.DeviceID,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
			Provider     string `json:"provider"`
			AccountKey   string `json:"account_key"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
			provider:     Provider(strings.TrimSpace(f.Provider)),
			key:          AccountKey(strings.TrimSpace(f.AccountKey)),
			ApiHost:      f.ApiHost,
			MachineID:    f.MachineID,
			DeviceID:     f.DeviceID,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// Trae 专属字段同样非空才写回（放在 auth 对象内，与 trae2api 输出一致）：
	// WorkBuddy 凭证不会被塞进空键。
	if authDoc, ok := doc["auth"].(map[string]any); ok {
		if a.ApiHost != "" {
			authDoc["apiHost"] = a.ApiHost
		}
		if a.MachineID != "" {
			authDoc["machineId"] = a.MachineID
		}
		if a.DeviceID != "" {
			authDoc["deviceId"] = a.DeviceID
		}
	}
	// provider 总是写回（空值归一化为 workbuddy）：把 provider 维度显式化，
	// 让 LoadDir 的 backfill 一次收敛，之后不再依赖文件名前缀推断。
	prov := a.provider
	if prov == "" {
		prov = ProviderWorkBuddy
	}
	doc["provider"] = string(prov)
	// account_key 非空才写回：键由 LoadDir / 登录导入路径生成，Parse 保持只读，
	// 不在这里凭空造键（否则未挂 FilePath 的临时对象也会被写脏）。
	if a.key != "" {
		doc["account_key"] = string(a.key)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// providerFromFile 从文件名前缀推断 provider，仅在文件内容缺 provider 键时使用。
// 文件名只作为存量迁移的提示，内容始终优先（写完 backfill 后不再依赖它）。
func providerFromFile(path string) Provider {
	if strings.HasPrefix(filepath.Base(path), "trae-") {
		return ProviderTrae
	}
	return ProviderWorkBuddy
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json 与 trae*.json；解析失败的文件静默跳过
// （启动日志由调用方统计）。provider 以文件内容的 provider 键为准，缺键时按文件名
// 前缀回落（trae-* → Trae，其余 → WorkBuddy）。
//
// 顺带做存量迁移：缺 provider / 空 realm 的 auth 自动 backfill（provider 按文件名、
// realm 按原始 domain 推断）并 SaveAtomic 落盘，一次性把旧文件补齐。provider 与 realm
// 合并成一次写。单个文件写失败不阻断启动（log WARN 继续），避免历史 auth 目录
// 个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	var files []string
	for _, pat := range []string{"workbuddy*.json", "trae*.json"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	// 稳定顺序：重复 UID 的「后载入者胜出」行为依赖它（见下方 seenUID WARN）。
	sort.Strings(files)
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f

		// provider 存量迁移：内容缺 provider 键时按文件名前缀回落。
		providerChanged := false
		if a.ProviderStored() == "" {
			a.provider = providerFromFile(f)
			providerChanged = true
		}
		// realm 存量迁移：空 realm 按原始 domain 推断（不变更已有标识）。
		realmChanged := false
		realmMsg := ""
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				realmChanged = true
				if r == "global" {
					realmMsg = fmt.Sprintf("auth %s 存量迁移: 补 realm=global（domain=%s）",
						logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		// 主键存量迁移：缺 account_key 时生成 UUIDv4。生成后必须落盘，否则重启会换键、
		// state.json 与 Redis 里的引用全部失配。
		keyChanged := false
		if _, changed, err := a.EnsureKey(); err != nil {
			log.Printf("WARN: auth %s account key gen: %v", logfmt.Label(a.UID, a.Nickname), err)
		} else {
			keyChanged = changed
		}
		if providerChanged || realmChanged || keyChanged {
			if err := a.SaveAtomic(); err != nil {
				log.Printf("WARN: auth %s backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
			} else if realmMsg != "" {
				log.Printf("%s", realmMsg)
			}
		}
		out = append(out, a)
	}
	return out, nil
}
