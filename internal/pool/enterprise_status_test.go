package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestStatusExposesEnterpriseFlag 面板需要 per-account 的「是否企业版」来决定
// 渲染哪些按钮（企业号隐藏签到/任务）。该标志是 auth.EnterpriseID 的计算值，
// 不落盘——状态接口与 List() 必须都能透出。
func TestStatusExposesEnterpriseFlag(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "ent", Nickname: "企业号", EnterpriseID: "e1"})
	p.Add(&auth.Auth{UID: "per", Nickname: "个人号"})
	// Trae 登录把 TenantID 记入 EnterpriseID，但「企业版」是 WorkBuddy 私有规则，
	// Trae 恒可签到：不得因此被标成企业版（否则面板隐藏签到按钮）。
	traeAuth := &auth.Auth{UID: "trae1", Nickname: "Trae号", EnterpriseID: "tenant-1"}
	auth.SetProvider(traeAuth, auth.ProviderTrae)
	p.Add(traeAuth)

	ent, ok := p.Status("ent")
	if !ok {
		t.Fatal("ent status missing")
	}
	if !ent.Enterprise {
		t.Error("企业号 Status.Enterprise 应为 true")
	}
	per, ok := p.Status("per")
	if !ok {
		t.Fatal("per status missing")
	}
	if per.Enterprise {
		t.Error("个人号 Status.Enterprise 应为 false")
	}
	tr, ok := p.Status("trae1")
	if !ok {
		t.Fatal("trae1 status missing")
	}
	if tr.Enterprise {
		t.Error("Trae 号即使有 TenantID 也不应标企业版（会误藏签到按钮）")
	}

	// List() 是面板 overview 的数据源，同样要带标志。
	for _, st := range p.List() {
		switch st.UID {
		case "ent":
			if !st.Enterprise {
				t.Error("List() 未透出企业号标志")
			}
		case "per":
			if st.Enterprise {
				t.Error("List() 误标个人号为企业版")
			}
		case "trae1":
			if st.Enterprise {
				t.Error("List() 误标 Trae 号为企业版")
			}
		}
	}
}
