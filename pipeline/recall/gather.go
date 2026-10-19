package recall

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var (
	errNilStore  = errors.New("recall: 存储未装配")
	errNilBander = errors.New("recall: 必须装配 Bander：召回内容是不可信内容，不裹数据区不得进 prompt")
)

// hasSource 判断来源是否在配置里。
func hasSource(list []Source, want Source) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// gather 按路由决定的面取候选。
//
// 路由规则（缺省口径）：
//   - 恒有 atom（召回主力）与 raw（兜底）；
//   - 宿主解析出实体时才加 page_headline——它是固定成本，收益要话题锁定才兑现。
//
// 关键：**ActiveOnly 恒置位**。漏掉它意味着已退役与已作废的事实照进 prompt，
// 而那种失效在数据上完全看不出来（内容看着是对的，只是过时了）。
func (p *Pipeline) gather(ctx context.Context, req Request, limit int) ([]*candidate, error) {
	var out []*candidate

	if hasSource(p.cfg.Sources, SourceAtom) {
		atoms, err := p.store.ListAtoms(ctx, req.Namespace, store.AtomFilter{
			ActiveOnly: true,
			Limit:      limit,
		})
		if err != nil {
			return nil, err
		}
		for _, a := range atoms {
			c, err := p.snippet(ctx, SourceAtom, a.ID, a.EntityID, "记忆", a.Assertion,
				a.Importance, a.OccurredAt)
			if err != nil {
				return nil, err
			}
			c.bucket = bucketAtom
			c.dedupKey = a.EntityID + "\x00" + a.Signature()
			out = append(out, c)
		}
	}

	if hasSource(p.cfg.Sources, SourceRaw) {
		raws, err := p.store.ListRaw(ctx, req.Namespace, time.Time{},
			store.Page{Limit: p.cfg.MaxRaw})
		if err != nil {
			return nil, err
		}
		for _, e := range raws {
			c, err := p.snippet(ctx, SourceRaw, e.ID, "", "历史", e.Content,
				types.ImportanceLow, e.Timestamp)
			if err != nil {
				return nil, err
			}
			c.bucket = bucketQuote
			c.dedupKey = "raw\x00" + e.ID
			c.rawSession = e.SessionID
			c.rawRuntimeError = e.Provenance.RuntimeError
			out = append(out, c)
		}
	}

	if hasSource(p.cfg.Sources, SourcePageHeadline) && len(req.Entities) > 0 {
		for _, id := range req.Entities {
			page, err := p.store.GetEntityPage(ctx, req.Namespace, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue // 实体还没有页面是常态（晋升先于首次生成），不是错
				}
				return nil, err
			}
			if page.Headline == "" {
				continue
			}
			c, err := p.snippet(ctx, SourcePageHeadline, id, id, "画像", page.Headline,
				types.ImportanceMed, page.UpdatedAt)
			if err != nil {
				return nil, err
			}
			c.bucket = bucketHeadline
			c.dedupKey = "headline\x00" + id
			out = append(out, c)
		}
	}

	return out, nil
}

// snippet 造一个**已包裹**的候选片段。
//
// 包裹在这一层发生、而不是渲染时：片段一旦被造出来就是「可以进 prompt 的形态」，
// 让它以明文形态存在等于给下游留了一条忘记包裹的路。
func (p *Pipeline) snippet(ctx context.Context, src Source, refID, entityID, role, body string,
	imp types.Importance, at time.Time) (*candidate, error) {
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	wrapped := p.bander.WrapMemory(body)
	n, err := p.est.Estimate(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	return &candidate{
		Snippet: Snippet{
			Source: src, RefID: refID, EntityID: entityID, Role: role,
			Body: wrapped, Importance: imp, OccurredAt: at, Tokens: n,
		},
		plain: body,
	}, nil
}

// pushEntities 把本次召回命中的实体推进活跃实体栈。
//
// 两条入栈路径（缺省口径）：
//  1. 召回命中的实体（事实卡带出来的）；
//  2. 宿主已解析出的实体。
//
// 推进发生在**重排之后、取用之前**——用的是排过序的前几条，
// 而不是全量命中。否则一个实体只要沾边就会进栈，栈里全是弱相关对象。
func (p *Pipeline) pushEntities(ctx context.Context, req Request, cands []*candidate) []string {
	if req.ThreadID == "" {
		return nil
	}
	now := p.now()
	seen := map[string]bool{}
	var pushed []string

	push := func(id, name string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		pushed = append(pushed, id)
		_ = p.store.UpsertActiveEntity(ctx, req.Namespace, &types.ActiveEntity{
			ThreadID: req.ThreadID, EntityID: id, CanonicalName: name, LastSeen: now,
		})
	}

	// 名字留空：召回片段本身不携带实体名（那是另一张表的事），硬塞 RefID
	// 会把「事实卡 ID」当成实体名存进栈里。存储层的空名按「沿用旧值」处理。
	for _, c := range cands[:minInt(len(cands), 5)] {
		if c.bucket == bucketAtom {
			push(c.EntityID, "")
		}
	}
	for _, id := range req.Entities {
		push(id, "")
	}

	// 栈有上限，超了按 LRU 淘汰（钉住项不参与，见 store 契约）。
	if _, err := p.store.EvictActiveEntities(ctx, req.Namespace, req.ThreadID, 8); err != nil {
		return pushed
	}
	return pushed
}

// allocate 分桶配额：按优先级逐条接收，装不下就丢弃并计数。
//
// 配额按桶各算：事实桶满了**不占用**画像桶的额度。这样画像这一档是**保留**的，
// 而不是「事实够多就永远挤掉画像」——后者会让页面重生成这个功能形同虚设。
//
// # 已知边界：桶容量小于该桶最小单元时，该桶恒为空
//
// 分桶是**上限**，不是保底。默认口径下画像一句话通常远小于 10% × 1500 = 150
// token，故不成问题；但若宿主把 TotalBudget 压到很小、或画像写得极长，
// 画像会**静默地一次都不出现**。症状是「页面重生成白做了」而不是报错。
// 宿主调小预算时要用 Result.Dropped 盯一眼，别只看它非零。
func (p *Pipeline) allocate(cands []*candidate) ([]Snippet, int) {
	caps := map[bucket]int{
		bucketAtom:     int(float64(p.cfg.TotalBudget) * p.cfg.BucketAtom),
		bucketQuote:    int(float64(p.cfg.TotalBudget) * p.cfg.BucketQuote),
		bucketHeadline: int(float64(p.cfg.TotalBudget) * p.cfg.BucketHeadline),
	}
	used := map[bucket]int{}
	var (
		kept  []Snippet
		total int
	)
	for _, c := range cands {
		if c.Tokens > caps[c.bucket]-used[c.bucket] || total+c.Tokens > p.cfg.TotalBudget {
			continue // 不 break：后面可能有小片段塞得进去
		}
		used[c.bucket] += c.Tokens
		total += c.Tokens
		kept = append(kept, c.Snippet)
	}
	return kept, len(cands) - len(kept)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
