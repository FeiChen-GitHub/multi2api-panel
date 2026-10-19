// signin 一次性批量签到工具：遍历 ./auths/workbuddy-*.json 与 ./auths/trae-*.json，
// 自动 RefreshToken（过期时），逐个签到，顺手查余额。
//
// WorkBuddy 走 daily-checkin + get-user-resource；
// Trae 走 internal/upstream 的 TraeClient（Checkin / CreditPackages），
// 两家的“已签到”“未开放”都按幂等成功处理，不报错。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type row struct {
	file     string
	uid      string
	provider string
	nick     string
	status   string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail   string
	remain   int64
	hasQuota bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, _ := filepath.Glob(filepath.Join(dir, "workbuddy-*.json"))
	traeFiles, _ := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	files = append(files, traeFiles...)
	sort.Strings(files)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	up := upstream.New()

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		r := row{file: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// provider：优先用文件里存的，缺省按文件名前缀回落（存量文件没有 provider 键）。
		r.provider = string(a.Provider())
		if r.provider == "" {
			if strings.Contains(filepath.Base(f), "trae-") {
				r.provider = string(auth.ProviderTrae)
			} else {
				r.provider = string(auth.ProviderWorkBuddy)
			}
		}

		// ---------- Trae 分支 ----------
		// 注意：Trae 的错误文案里也含 "checkin"，绝不能走 WorkBuddy 的 isAlready()，
		// 否则真正的失败会被误判成“已签到”。
		if a.Provider() == auth.ProviderTrae {
			tc := upstream.NewTraeClient()
			ctx := context.Background()
			if a.NeedsRefresh(2*time.Hour) || a.AccessTokenValue() == "" {
				if err := tc.RefreshToken(ctx, a); err != nil {
					var ue *upstream.Error
					if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
						r.status = "AUTH_INVALID"
					} else {
						r.status = "FAIL"
					}
					r.detail = "refresh: " + short(err.Error())
					rows = append(rows, r)
					failN++
					continue
				}
				// 落盘失败必须暴露，否则重启回旧 token。
				if err := a.SaveAtomic(); err != nil {
					log.Printf("signin %s save: %v", a.UID, err)
				}
			}
			if err := tc.Checkin(ctx, a); err != nil {
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					r.status = "AUTH_INVALID"
				} else {
					r.status = "FAIL"
				}
				r.detail = short(err.Error())
				failN++
			} else {
				r.status = "OK"
				okN++
			}
			// 顺手查余额（失败不影响签到结论）。
			if _, remain, _, qerr := tc.CreditPackages(ctx, a); qerr == nil {
				r.remain, r.hasQuota = remain, true
			}
			rows = append(rows, r)
			continue
		}

		// ---------- WorkBuddy 分支（原逻辑） ----------
		if a.NeedsRefresh(2 * time.Hour) {
			if err := up.RefreshToken(a); err != nil {
				if ue, ok := err.(*upstream.Error); ok && ue.Kind == upstream.ErrSessionDead {
					r.status = "AUTH_INVALID"
				} else {
					r.status = "FAIL"
				}
				r.detail = "refresh: " + short(err.Error())
				rows = append(rows, r)
				failN++
				continue
			}
			// refresh 后写回文件（权限问题已修复）；落盘失败必须暴露，否则重启回旧 token
			if err := a.SaveAtomic(); err != nil {
				log.Printf("signin %s save: %v", a.UID, err)
			}
		}

		err = up.DailyCheckin(a)
		switch {
		case err == nil:
			r.status = "OK"
			okN++
		default:
			// DailyCheckin 已签到返回 code!=0 错误
			if isAlready(err.Error()) {
				r.status = "ALREADY"
				r.detail = short(err.Error())
				alreadyN++
			} else {
				r.status = "FAIL"
				r.detail = short(err.Error())
				failN++
			}
		}
		// 顺手查余额
		if remain, _, qerr := up.UserResource(a); qerr == nil {
			r.remain, r.hasQuota = remain, true
		}
		rows = append(rows, r)
	}

	// 报告
	fmt.Printf("uid                                  | provider   | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-36s | %-10s | %-11s | %-12s | %-6s | %s\n",
			trunc(r.uid, 36), trunc(r.provider, 10), trunc(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

// 已签判定：code 非 0 且含 "已签到"/"already"/"checkin" 等字样
// （只给 WorkBuddy 用；Trae 分支不调用它）
func isAlready(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
