// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type Pool struct {
	mu sync.RWMutex
	// byKey 主索引：池内账号主键（auth.AccountKey，UUIDv4）→ entry。账号身份的唯一来源。
	byKey map[auth.AccountKey]*entry
	// byUID 过渡期兼容镜像：上游 uid → entry。现有 uid-based API 与测试走它；
	// uid 不是身份，跨 provider 撞号时后进者覆盖（upsertLocked 打 WARN）。
	// 待调用点全部迁到 AccountKey 后删除。
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// costExploreInterval costTier 条件探索窗口（issue #136 方案 a′，SetCostExploreInterval
	// 注入；默认 defaultCostExploreInterval 30m）。tier 0 垄断 + tier 1 存在且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only（探索=搭车改道，零新增上游请求）。
	// 0 = 关停（完全回到现状行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 多至 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 只在探索事件时写入（tier 1 枯竭期间停走，陈旧无害）；不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（/status 透出；pick 写锁内 ++，无需 atomic）。
	costExploreEvents int64
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// creditFloor 积分保底（SetCreditFloor 注入；0 = 关闭，缺省即现状）。
	// 账号 credits < floor 时不再参与选号——防止收费请求把余额打穿、连免费模型都
	// 402 冷却到次日签到。签到回血（SetCreditsDetailed）越过 floor 即自动恢复。
	// 全池触底且无免费模型可接时选号返回 nil（硬语义：宁 503 不打穿）。
	// 收费与否的判据见 floorBlockedForModel：本地实测台账优先，缺失时用上游目录
	// 倍率（modelRateOf 注入）兜底，避免「无观测的高价新模型」绕过保底。
	creditFloor int64
	// modelRateOf 按 (realm, 模型) 查上游目录积分倍率（"0.79" / "" = 未知）。
	// 由 main 用 upstream.Client.ModelRate 注入——pool 不依赖 upstream 包（避免
	// 循环依赖与分层破坏），nil 时倍率兜底不生效（退化为仅本地台账判定）。
	// 仅在持 p.mu 时由 floorBlockedForModel 调用；回调不得反向调用 Pool 方法。
	modelRateOf func(realm, model string) string
	// 加权路由的闲置补偿调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// preferExpiring 最早到期优先路由开关（默认 true）。开启且快过期窗口内存在有效
	// 批次时，选号在成本层内先按最早到期排序；关闭后只使用普通加权路由。
	preferExpiring bool
	// crossDomainFallback 同 provider 内跨域回退开关（默认 false = 零回归，issue #135）。
	// 仅由选号调用方（server handler）读取：开启后，未显式写 realm 的请求
	// （如 wb:glm-5.2）在首选域选不到可用号时改用**同一 provider** 的另一域再选一次；
	// 显式 wb:cn: / wb:global: 前缀不受影响，也绝不跨 provider 借用账号。
	crossDomainFallback bool
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// pickSeq 单调递增的选号序号：每次 pick 选中账号时自增并记到 entry.usedSeq，
	// 为 LRU 兜底/防惊群提供与 time.Now() 精度无关的严格全序（Windows ~0.5ms 精度下
	// lastUsed 墙钟会全等）。仅 pick 写锁路径读写，无需 atomic。
	pickSeq uint64
	// strategies 按地区独立的调度策略（cn=domestic / global=international）。
	// 空串等价 weighted_random（现状零回归），由 SetScheduling 注入并热更新。
	strategies RegionStrategies
	// accountOrder 按地区的 UID 优先顺序（轮询与顺序耗尽用）。
	accountOrder RegionAccountOrder
	// scheduling 顺序耗尽/轮询的游标，键 = provider\x1frealm\x1fmodel（见 schedulingKey）。
	// 持久化进 state.json 的 scheduling 段，重启后按既有快照机制恢复。
	scheduling map[string]scheduleCursor
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byKey:              map[auth.AccountKey]*entry{},
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		preferExpiring:     true,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口（issue #136）。用户经
		// config 显式 "0" 关停（SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
		scheduling:          map[string]scheduleCursor{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// Close 停止后台落盘 goroutine 并做最后一次落盘（幂等）。
// 进程退出前调用，消除 startFlusher 的 goroutine 泄漏；不调用也不影响正确性
// （进程退出即回收），仅是生命周期卫生。
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	p.Flush()
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用，
// issue #136）。0 = 关停（完全回到现状行为）；正值覆盖默认 30m。
// 注意：与 SetSoftRateMax「非正值保留默认」不同，0 在这里是**合法值**（关停开关，
// 与 config 的 "0" 关停语义对齐）——不设 0 语义就无法关停探索。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // 负值非法，保留现值
	}
	p.costExploreInterval = d
}

// CostExploreStatus 透出探索台账（/status 用）：累计探索事件数 + 各 (域, 模型)
// 的最近探索时刻（键内 \x1f 分隔符输出为 "|"，与 model_costs 行对照即可读出
// 「探索→毕业」全链路）。RLock 只读遍历；map 大小受「服务过的 (域, 模型)」集合
// 约束（与 modelCost 同界，天然有界）。
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// 键 realm+"\x1f"+model → 输出 "|"（JSON 安全可读；\x1f 不可打印）。
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetWeights 注入加权路由的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetPreferExpiring 注入最早到期优先路由开关（main 从 config 解析后调用）。
func (p *Pool) SetPreferExpiring(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preferExpiring = enabled
}

// SetCrossDomainFallback 注入同 provider 内跨域回退开关（issue #135；false = 零回归）。
// 与其它 pool.* 参数一致，面板保存配置后经同一条热应用路径立即生效。
func (p *Pool) SetCrossDomainFallback(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.crossDomainFallback = enabled
}

// CrossDomainFallback 报告跨域回退开关当前状态（选号侧只读，热改安全）。
func (p *Pool) CrossDomainFallback() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.crossDomainFallback
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// CreditFloor 透出生效的积分保底值（/status 用）。0 = 关闭。
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// SetCreditFloor 注入积分保底线（main 从 config 解析后调用）。
// 0 = 关闭（缺省即现状，零回归）；负值非法保留原值（0）。
// 语义见 Pool.creditFloor 字段注释。
func (p *Pool) SetCreditFloor(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.creditFloor = n
	}
}

// SetModelRateOf 注入上游目录积分倍率查表（main 用 upstream.Client.ModelRate 装配）。
// 供积分保底兜底判定「未实测过的模型是否收费」——本地台账无观测时，不能因为
// 「没学过」就放行，否则高价新模型会把触底号一次性打穿（kimi-k3-1 实案：
// 全池无观测 → 保底全部放行 → 两笔扣 111 分打穿到 0 并硬冷却到次日 04:00）。
// fn 可为 nil（清注入）；回调只在持 p.mu 时被调用，不得反向调用 Pool 方法。
func (p *Pool) SetModelRateOf(fn func(realm, model string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelRateOf = fn
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
// entryByKeyLocked 主索引查找（调用方须持 p.mu）；空键返回 nil。
func (p *Pool) entryByKeyLocked(k auth.AccountKey) *entry {
	if k == "" {
		return nil
	}
	return p.byKey[k]
}

// entryByUIDLocked 是过渡期兼容入口：按上游 uid 查镜像索引（调用方须持 p.mu）。
// uid 只是索引、不是身份；跨 provider 撞号时镜像只保留后进者。新代码一律用 AccountKey。
func (p *Pool) entryByUIDLocked(uid string) *entry {
	if uid == "" {
		return nil
	}
	return p.byUID[uid]
}

// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e := p.entryByUIDLocked(uid)
	if e == nil {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e := p.entryByUIDLocked(uid)
	p.mu.RUnlock()
	if e == nil {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[auth.AccountKey]bool, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		if key, _, err := a.EnsureKey(); err == nil && key != "" {
			seen[key] = true
		}
		p.upsertLocked(a)
	}
	changed := false
	for key, e := range p.byKey {
		if seen[key] {
			continue
		}
		// 只有 uid 索引仍指向本 key 时才清理，避免误删撞号后的新映射。
		if e != nil && e.a != nil {
			if cur := p.byUID[e.a.UID]; cur == e {
				delete(p.byUID, e.a.UID)
			}
		}
		delete(p.byKey, key)
		changed = true
	}
	if changed {
		p.saveLocked()
	}
}

// Remove 从池中移除账号并立即落盘（管理面板用）。返回被移除账号的凭证
// （含 FilePath，供调用方删除 auth 文件）；uid 不存在返回 nil。
// 在途请求的 Release 对已删条目是 no-op，无需等待。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryByUIDLocked(uid)
	if e == nil {
		return nil
	}
	delete(p.byUID, uid)
	if e.key != "" {
		delete(p.byKey, e.key)
	}
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if a == nil {
		return
	}
	key, generated, err := a.EnsureKey()
	if err != nil || key == "" {
		log.Printf("WARN: pool: account %s ensure key failed: %v", a.UID, err)
		return
	}
	// 调用方给的是没有持久化主键的新对象（测试构造 / 重新登录），而池里已有同一账号的
	// 既有状态（state.json 恢复出的 placeholder：同 provider、无 token）时，以既有主键为准，
	// 否则新生成的随机键会让历史 credits/冷却失配（重载场景）。
	// 注意条件含 provider + placeholder：跨 provider 撞 uid 的真实账号不会被并进来。
	if generated {
		if prev := p.byUID[a.UID]; prev != nil && prev.key != "" &&
			prev.a != nil && prev.a.AccessTokenValue() == "" && prev.a.Provider() == a.Provider() {
			key = prev.key
			a.SetKey(key)
		}
	}
	e, ok := p.byKey[key]
	if !ok {
		// 迁移：state.json 旧条目以 uid 为键，这里把它归位到 AccountKey，
		// 保留 credits / 冷却 / 熔断等历史状态（placeholder 凭证换成真实凭证）。
		legacyKey := auth.AccountKey(a.UID)
		if a.UID != "" && legacyKey != key {
			if legacy := p.byKey[legacyKey]; legacy != nil {
				delete(p.byKey, legacyKey)
				legacy.key = key
				legacy.a = a
				p.byKey[key] = legacy
				e, ok = legacy, true
			}
		}
	}
	if ok {
		e.a = a // 保留 credits/cooling 状态
	} else {
		e = &entry{a: a, key: key}
		p.byKey[key] = e
	}
	// uid 镜像：撞号时后进者覆盖并 WARN；同时清掉指向本 entry 的旧 uid。
	if prev := p.byUID[a.UID]; prev != nil && prev != e && prev.key != "" && prev.key != key {
		log.Printf("WARN: pool: uid %s 已映射到 key %s，现被 %s 覆盖（跨 provider 撞号？）",
			a.UID, prev.key, key)
	}
	for uid, cur := range p.byUID {
		if cur == e && uid != a.UID {
			delete(p.byUID, uid)
		}
	}
	p.byUID[a.UID] = e
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
