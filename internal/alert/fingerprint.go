package alert

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// DynamicLabelKeys 是不参与指纹计算的动态标签。
//
// 这些标签的值随实例/容器重建而变化。若纳入指纹，同一故障在实例重建后会被
// 当成一条全新告警，去重与后续关联全部失效。取值依据 IIP-V1.0 技术设计文档 §4.3。
var DynamicLabelKeys = []string{"instance", "pod", "container", "node"}

// dynamicLabelSet 供 O(1) 查询用；包级只构造一次。
var dynamicLabelSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(DynamicLabelKeys))
	for _, k := range DynamicLabelKeys {
		set[k] = struct{}{}
	}
	return set
}()

// GenerateFingerprint 基于稳定标签集合生成 SHA256 指纹（IIP-V1.0 §4.3）。
//
// 规则：
//  1. 剔除 DynamicLabelKeys 中的动态标签；
//  2. 剩余键**排序后**以 "k=v," 拼接；
//  3. SHA256 后 hex 编码——恒为 64 字符，列宽 VARCHAR(128) 有余。
//
// 有意不纳入 annotations：注解值（如 value: 1250ms）每次评估都会变化，
// 纳入即等于永远无法重复，去重形同虚设。
//
// 键顺序之所以必须排序：Go 的 map 遍历顺序随机，不排序会产生不稳定指纹。
func GenerateFingerprint(labels map[string]string) string {
	stable := make([]string, 0, len(labels))
	for k := range labels {
		if _, dynamic := dynamicLabelSet[k]; dynamic {
			continue
		}
		stable = append(stable, k)
	}
	sort.Strings(stable)

	var buf bytes.Buffer
	for _, k := range stable {
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(labels[k])
		buf.WriteByte(',')
	}

	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}
