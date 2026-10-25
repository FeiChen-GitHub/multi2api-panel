// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// AvailableKeys 同 AvailableUIDs，但返回池内主键（AccountKey）——会话粘性绑定用主键，
// 因为 uid 在跨 provider 之间可能重复。按主键字符串排序，输出稳定。
func (p *Pool) AvailableKeys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	keys := make([]string, 0, len(p.byKey))
	for key, e := range p.byKey {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	return keys
}

// WeightedAvailableKeysForModelRealm 同 WeightedAvailableUIDsForModelRealm，但返回主键：
// 普通账号出现 1 次，有效快过期账号出现 expiringVirtualSlots 次（新会话温和偏好）。
func (p *Pool) WeightedAvailableKeysForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	entries := make(map[string]*entry, len(p.byKey))
	keys := make([]string, 0, len(p.byKey))
	for key, e := range p.byKey {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		s := string(key)
		entries[s] = e
		keys = append(keys, s)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		slots := 1
		if p.preferExpiring && expiringNow(entries[k], now) {
			slots = expiringVirtualSlots
		}
		for i := 0; i < slots; i++ {
			out = append(out, k)
		}
	}
	return out
}

// AvailableKeysForProviderModelRealm 返回候选账号主键：先按 provider + realm 过滤，
// 再做模型健康判定（healthyForModel，含 6004 模型豁免）与在途占满判定。
// provider=="" 不过滤 provider；realm=="" 不过滤 realm；model=="" 退化为账号级 healthy。
// 结果按主键排序，保证调用方看到稳定顺序。
//
// 这是多 provider 路由的选号入口：调用方（handler / session）拿到主键后，
// 用 AuthByKey / PickByKey 系列取号，不再依赖上游 uid。
func (p *Pool) AvailableKeysForProviderModelRealm(provider auth.Provider, model, realm string) []auth.AccountKey {
	return p.availableKeysForProviderModelRealm(provider, model, realm, ChannelSOLO)
}

// AvailableKeysForProviderModelRealmChannel 同上，再叠一层通道维度（DESIGN §6.6
// 第 ② ③ 点）：Work 请求只统计「Work 出口未冷却」的账号，SOLO 反之。
// 调用方用它判断「某个 provider+realm+通道是否还有号」，避免 Work 请求被 SOLO
// 侧的冷却误判成无号。
func (p *Pool) AvailableKeysForProviderModelRealmChannel(provider auth.Provider, model, realm string, ch Channel) []auth.AccountKey {
	return p.availableKeysForProviderModelRealm(provider, model, realm, ch)
}

func (p *Pool) availableKeysForProviderModelRealm(provider auth.Provider, model, realm string, ch Channel) []auth.AccountKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	keys := make([]auth.AccountKey, 0, len(p.byKey))
	for key, e := range p.byKey {
		if e == nil || e.a == nil {
			continue
		}
		if provider != "" && e.a.Provider() != provider {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		// healthyForChannel 已含模型级维度（model=="" 时该维度不生效），
		// 与改前两分支等价（零回归）。
		if !e.healthyForChannel(now, ch, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// ServableForProviderModelRealmChannel 返回「provider + realm + 通道」这个组合此刻
// **真正能服务该模型**的账号数（跨域回退 issue #135 的判据）。
//
// 与 pick 的候选口径一致（healthyForChannel 的 6004/11102 模型级冷却豁免、在途租约、
// 积分保底都计入），但**不含 pick 的全冷却兜底**——回退要的正是「这个组合此刻有没有
// 健康号」，兜底把冷却号捞回来的乐观语义会掩盖这一点。
//
// 只读：持 RLock 遍历，不改任何状态。provider / realm 为空串时退化为该维不过滤
// （与既有 realm 语义一致）。
func (p *Pool) ServableForProviderModelRealmChannel(provider auth.Provider, model, realm string, ch Channel) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byKey {
		if e == nil || e.a == nil {
			continue
		}
		if provider != "" && e.a.Provider() != provider {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForChannel(now, ch, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		n++
	}
	return n
}
