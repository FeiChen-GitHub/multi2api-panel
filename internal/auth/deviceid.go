// deviceid.go — 保证每个 Trae 账号有一个可用于每日签到的 deviceId。
//
// 背景：Trae 签到与积分查询（ug）接口要求 X-Device-Id 每账号互不相同——同一天
// 两个账号共用一个 deviceId，第二个会被「该设备已签到」拦截；空 deviceId 则签到
// 报 9004。真实设备号是 16 位纯数字，实测任意互异的 16 位数字串同样可用。
//
// 因此：新账号落盘前若 DeviceID 为空就补一个随机 16 位数字串并固化进 auth 文件，
// 保证该号始终用同一个 deviceId 签到。已有的（用户导入/真实注册）值一律保留。
package auth

import (
	"crypto/rand"
	"math/big"
)

// NewCheckinDeviceID 生成一个 16 位纯数字的签到 deviceId（首位 1-9，避免前导 0
// 被上游按数值解析掉）。格式与真实注册设备号一致。
func NewCheckinDeviceID() (string, error) {
	digits := make([]byte, 16)
	first, err := rand.Int(rand.Reader, big.NewInt(9))
	if err != nil {
		return "", err
	}
	digits[0] = byte('1' + first.Int64())
	for i := 1; i < 16; i++ {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digits[i] = byte('0' + d.Int64())
	}
	return string(digits), nil
}

// EnsureCheckinDeviceID 仅在 DeviceID 为空时生成并写入（返回是否新生成）。
func (a *Auth) EnsureCheckinDeviceID() (bool, error) {
	if a.DeviceID != "" {
		return false, nil
	}
	id, err := NewCheckinDeviceID()
	if err != nil {
		return false, err
	}
	a.DeviceID = id
	return true, nil
}
