package recall

import (
	"reflect"
	"testing"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：缓存副本的**完整性**。
//
// # 为什么要有这条用例
//
// cloneResult 逐字段手抄，而 Go 的结构体字面量**不要求写全字段**
// ⇒ 漏抄一个字段，编译照过、测试照过、运行也照过。
//
// 漏抄的失效形态极阴：缓存里那份的该字段是零值 ⇒
// 「第一次有、第二次没了」（或反过来）——只在「第二次问同一个问题」时显现。
//
// 故这里用**反射逐字段比对**兜底：源与副本的每个字段都必须相等，
// 少一个立刻红。
//
// ⚠️ 注意这**不能**靠「加字段时编译器会指出来」——那句话是错的
// （cloneResult 的注释里已订正）。靠的是这条用例。

// TestCloneResultCopiesEveryField 副本必须**逐字段**等于源。
//
// 判别特征：给每个字段填上**互不相同**的值——
// 若某个字段没被抄，它会是零值，与源不等 ⇒ 红。
// （用同一个值填所有字段不行：漏抄的字段可能恰好相等而假绿。）
func TestCloneResultCopiesEveryField(t *testing.T) {
	src := &Result{
		Snippets: []Snippet{{
			Source: SourceAtom, RefID: "r-1", EntityID: "e-1",
			Role: "记忆", Body: "body-1", Importance: types.ImportanceHigh, Tokens: 11,
		}},
		Dropped:                1,
		DroppedByBudget:        2,
		DroppedByDiversity:     3,
		DroppedByDuplicate:     4,
		Drops:                  map[ports.DropReason]int{ports.DropBudget: 5, ports.DropQuota: 6},
		Buckets:                map[ports.BucketID]int{"atom": 7, "quote": 8},
		FromCache:              true,
		Entities:               []string{"e-1", "e-2"},
		LooseSearchUnavailable: true,
	}

	cp := cloneResult(src)
	if cp == nil {
		t.Fatal("副本不该是 nil")
	}

	v1 := reflect.ValueOf(*src)
	v2 := reflect.ValueOf(*cp)
	typ := v1.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		a := v1.Field(i).Interface()
		b := v2.Field(i).Interface()
		if !reflect.DeepEqual(a, b) {
			t.Errorf("字段 %s 没被复制：源=%v 副本=%v", name, a, b)
		}
	}
}

// TestCloneResultIsDeepCopy 副本的**引用型字段**必须与源脱钩。
//
// 深拷贝不是洁癖：缓存里那份若与返回给调用方的那份共享底层数组，
// 调用方改一下（追加来源标记是常见做法）就会污染缓存——
// 症状是「第二次问同一个问题，内容里多了一截上次没见过的字」。
func TestCloneResultIsDeepCopy(t *testing.T) {
	src := &Result{
		Snippets: []Snippet{{RefID: "r-1", Body: "body-1"}},
		Drops:    map[ports.DropReason]int{ports.DropBudget: 5},
		Buckets:  map[ports.BucketID]int{"atom": 7},
		Entities: []string{"e-1"},
	}
	cp := cloneResult(src)

	cp.Snippets[0].Body = "改了"
	cp.Drops[ports.DropBudget] = 99
	cp.Buckets["atom"] = 99
	cp.Entities[0] = "改了"

	if src.Snippets[0].Body != "body-1" {
		t.Errorf("片段正文被共享了：源=%q", src.Snippets[0].Body)
	}
	if src.Drops[ports.DropBudget] != 5 {
		t.Errorf("丢弃原因表被共享了：源=%v", src.Drops)
	}
	if src.Buckets["atom"] != 7 {
		t.Errorf("分桶表被共享了：源=%v", src.Buckets)
	}
	if src.Entities[0] != "e-1" {
		t.Errorf("实体列表被共享了：源=%v", src.Entities)
	}
}
