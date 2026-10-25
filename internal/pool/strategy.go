package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	WeightedRandom    = "weighted_random"
	RoundRobin        = "round_robin"
	SequentialExhaust = "sequential_exhaust"
)

// RegionStrategies 使用配置层的地区名；路由层仍使用现有 cn/global realm。
type RegionStrategies struct {
	Domestic      string `json:"domestic"`
	International string `json:"international"`
}

func (s RegionStrategies) Normalize() (RegionStrategies, error) {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"domestic", &s.Domestic}, {"international", &s.International},
	} {
		*field.value = strings.ToLower(strings.TrimSpace(*field.value))
		if *field.value == "" {
			*field.value = WeightedRandom
		}
		switch *field.value {
		case WeightedRandom, RoundRobin, SequentialExhaust:
		default:
			return s, fmt.Errorf("%s: %q 不是合法策略（weighted_random / round_robin / sequential_exhaust）", field.name, *field.value)
		}
	}
	return s, nil
}

// RegionAccountOrder 指定 UID 优先顺序；未列出的账号按 UID 排序追加。
type RegionAccountOrder struct {
	Domestic      []string `json:"domestic,omitempty"`
	International []string `json:"international,omitempty"`
}

type scheduleCursor struct {
	Strategy string `json:"strategy"`
	UID      string `json:"uid"`
}

// SetScheduling 原子热更新两域策略。状态带策略标签，切换算法后忽略旧指针。
func (p *Pool) SetScheduling(s RegionStrategies, order RegionAccountOrder) error {
	s, err := s.Normalize()
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategies = s
	p.accountOrder = RegionAccountOrder{
		Domestic:      append([]string(nil), order.Domestic...),
		International: append([]string(nil), order.International...),
	}
	for key, cursor := range p.scheduling {
		// 键可能是两段（老格式 realm\x1fmodel）或三段（provider\x1frealm\x1fmodel）。
		// realm 在两种格式里分别是第 0 / 第 1 段。
		parts := strings.Split(key, "\x1f")
		realm := ""
		switch len(parts) {
		case 2:
			realm = parts[0]
		case 3:
			realm = parts[1]
		}
		if cursor.Strategy != p.strategyForRealmLocked(realm) {
			delete(p.scheduling, key)
			p.dirty.Store(true)
		}
	}
	return nil
}

func (p *Pool) strategyForRealmLocked(realm string) string {
	s := ""
	switch realm {
	case "cn":
		s = p.strategies.Domestic
	case "global":
		s = p.strategies.International
	}
	if s == "" {
		return WeightedRandom
	} // 无 realm 的旧入口不混用地区策略。
	return s
}

// StrategyForRealm 供会话入口决定新会话是否应交由账号池分配。
func (p *Pool) StrategyForRealm(realm string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategyForRealmLocked(realm)
}

func (p *Pool) restoreSchedulingLocked(cursors map[string]scheduleCursor) {
	p.scheduling = make(map[string]scheduleCursor, len(cursors))
	for key, cursor := range cursors {
		if cursor.Strategy != RoundRobin && cursor.Strategy != SequentialExhaust {
			continue
		}
		// 键有两种形态：新格式 provider\x1frealm\x1fmodel（三段），
		// 老格式 realm\x1fmodel（两段，main 分支写的，无 provider 维度）。
		// 老键原样保留：provider 为空的老入口仍会用两段键查它。
		parts := strings.Split(key, "\x1f")
		if len(parts) == 2 {
			if parts[0] == "cn" || parts[0] == "global" {
				p.scheduling[key] = cursor
			}
			continue
		}
		if len(parts) == 3 && (parts[1] == "cn" || parts[1] == "global") {
			p.scheduling[key] = cursor
		}
	}
}

// pickOrderedLocked 接收已完成健康、保底、在途与成本层过滤的全量候选。
// 临时 tried/在途占满/成本探索可借用其他账号，但不推进顺序耗尽的主账号。
// 调用方持 p.mu；指针按 (provider, realm, model) 隔离，并通过既有快照机制持久化。
//
// provider 维度是本仓相对 main 分支的扩展：同一个 realm 下 WorkBuddy 与 Trae 的
// 账号是两套独立的池子，顺序耗尽的「主账号」不能跨 provider 继承——否则 Trae 请求
// 会把 WorkBuddy 的游标推进到某个号上，两边互相干扰。
func (p *Pool) pickOrderedLocked(cands []*entry, tried map[auth.AccountKey]bool, model, realm string, provider auth.Provider, strategy string, explored bool, now time.Time) *entry {
	order := p.accountOrder.Domestic
	if realm == "global" {
		order = p.accountOrder.International
	}
	rank := make(map[string]int, len(order))
	for _, uid := range order {
		if _, exists := rank[uid]; !exists {
			rank[uid] = len(rank)
		}
	}
	lessUID := func(a, b string) bool {
		ar, aok := rank[a]
		br, bok := rank[b]
		if aok != bok {
			return aok
		}
		if aok {
			return ar < br
		}
		return a < b
	}
	sort.Slice(cands, func(i, j int) bool { return lessUID(cands[i].a.UID, cands[j].a.UID) })
	key := schedulingKey(provider, realm, model)
	cursor := p.scheduling[key]
	if cursor.Strategy != strategy {
		cursor = scheduleCursor{Strategy: strategy}
	}
	var selected *entry
	if strategy == SequentialExhaust {
		for _, e := range cands {
			if e.a.UID == cursor.UID {
				selected = e
				break
			}
		}
	}
	if selected == nil {
		selected = cands[0]
		if cursor.UID != "" {
			for _, e := range cands {
				if lessUID(cursor.UID, e.a.UID) {
					selected = e
					break
				}
			}
		}
	}
	advance := true
	if strategy == SequentialExhaust && cursor.UID != "" {
		current := p.byUID[cursor.UID]
		if current != nil && current.a.Realm() == realm && current.healthyForModel(now, model) &&
			!p.floorBlockedForRealmModel(current, model, realm, now) &&
			(explored || tried[current.key] || p.inFlightFull(current)) {
			advance = false
		}
	}
	if advance && cursor.UID != selected.a.UID {
		p.scheduling[key] = scheduleCursor{Strategy: strategy, UID: selected.a.UID}
		p.dirty.Store(true)
	}
	return selected
}

// schedulingKey 构造调度游标的键：provider + realm + model 三段。
// 用 \x1f（unit separator）分隔，与成本探索 key 的既有约定一致；
// provider 为空（老入口）时退化为两段键，与 main 分支的格式兼容。
func schedulingKey(provider auth.Provider, realm, model string) string {
	if provider == "" {
		return realm + "\x1f" + model
	}
	return string(provider) + "\x1f" + realm + "\x1f" + model
}
