// credit.go — WorkBuddy + Trae 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//	./credit -pretty           # 人类可读日报
//
// 输出结构:
//
//	{"service":"workbuddy|trae|workbuddy+trae","ts":N,"providers":{"workbuddy":N,"trae":N},
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","provider","nickname","remain","used","size","packages","ok","error?"}]}
//
// WorkBuddy：POST codebuddy.cn/v2/billing/meter/get-user-resource，聚合所有 package 的
// Cycle* 字段，TotalDosage 作 size 下限。
// Trae：internal/upstream.TraeClient.CreditPackages（企业用量包 CreditPackages）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const billingBaseCN = "https://www.codebuddy.cn"

// billingBaseGlobal 国际版计费域。global 账号打 CN 域会得到 401：
// www.codebuddy.cn 不认 workbuddy.ai 的 token（实测 401，workbuddy.ai 同 token 为 code=0）。
const billingBaseGlobal = "https://www.workbuddy.ai"

// billingBaseFor 按账号 realm 选择计费域。
//
// 判定口径与 internal/auth.Realm() 一致：显式 realm=global 或 domain 落在
// workbuddy.ai 家族，都按国际版处理。cmd/credit 早先对所有账号硬编码 CN 域，
// 导致 global 账号余额查询恒返回 401（面板显示的是池内缓存值，不是实时的）。
func billingBaseFor(a *auth.Auth) string {
	if strings.EqualFold(strings.TrimSpace(a.Realm()), "global") {
		return billingBaseGlobal
	}
	d := strings.ToLower(strings.TrimSpace(a.DomainValue()))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return billingBaseGlobal
	}
	return billingBaseCN
}

type accountResult struct {
	UID      string `json:"uid"`
	Provider string `json:"provider"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type resourcePackage struct {
	CapacityRemain      int64 `json:"CapacityRemain"`
	CapacityUsed        int64 `json:"CapacityUsed"`
	CapacitySize        int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
}

// packageRemainUsed 与 billing.go:203-258 一致
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

func fetchUserResource(a *auth.Auth) (remain, used, size int64, packs int, err error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	base := billingBaseFor(a)
	// global 域无 /v2 前缀（与 internal/upstream 的 billingMeterPaths 同口径）。
	path := "/v2/billing/meter/get-user-resource"
	if base == billingBaseGlobal {
		path = "/billing/meter/get-user-resource"
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64             `json:"TotalDosage"`
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	for _, p := range env.Data.Response.Data.Accounts {
		r, u, s := packageRemainUsed(p)
		remain += r
		used += u
		size += s
	}
	packs = len(env.Data.Response.Data.Accounts)
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := env.Data.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	list, _ := auth.LoadDir(authDir)

	accounts := make([]accountResult, 0, len(list))
	for _, a := range list {
		res := accountResult{UID: a.UID, Nickname: a.Nickname, Provider: string(a.Provider())}
		if res.Provider == "" {
			res.Provider = string(auth.ProviderWorkBuddy)
		}

		// ---------- Trae 分支：走企业用量包 CreditPackages ----------
		if a.Provider() == auth.ProviderTrae {
			packs, remain, total, err := upstream.NewTraeClient().CreditPackages(context.Background(), a)
			if err != nil {
				res.Error = err.Error()
			} else {
				used := total - remain
				if used < 0 {
					used = 0
				}
				res.Remain, res.Used, res.Size = &remain, &used, &total
				res.Packages = len(packs)
				res.OK = true
			}
			accounts = append(accounts, res)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		// ---------- WorkBuddy 分支（原逻辑） ----------
		if a.AccessTokenValue() == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, used, size, packs, err := fetchUserResource(a)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain, res.Used, res.Size, res.Packages, res.OK = &remain, &used, &size, packs, true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	providers := map[string]int{}
	for _, a := range accounts {
		providers[a.Provider]++
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	service := "workbuddy"
	if providers[string(auth.ProviderTrae)] > 0 {
		if providers[string(auth.ProviderWorkBuddy)] > 0 {
			service = "workbuddy+trae"
		} else {
			service = "trae"
		}
	}
	out := map[string]any{
		"service":   service,
		"ts":        time.Now().Unix(),
		"providers": providers,
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报：四行汇总 + 失败明细。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, "["+a.Provider+"] "+name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 积分日报（WorkBuddy + Trae）\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
