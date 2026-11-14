package sqlite

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
)

// 本文件：向量索引（ports.VectorIndex）的后端实现。
//
// 从 misc.go 拆出（archcheck 的 max_file_lines=800 是拆分信号不是风格偏好）：
// 向量是**派生物**——真源是事实卡（向量是事实的函数），
// 索引丢了可以从事实卡重算，事实卡丢了索引救不回来。
// 故这一面只有「落向量与读向量」，没有任何改写事实的入口。

// ── 向量索引 ──
//
// 向量是**派生物**：真源是事实卡（向量是事实的函数），
// 索引丢了可以从事实卡重算，事实卡丢了索引救不回来。
// 故这一面只有「落向量与读向量」，没有任何改写事实的入口。

// vectorStore 向量索引存取（**派生物**）。
//
// 刻意单列一个窄结构而不是平铺在 Store 上：
// 仓里「每张表一个内嵌窄结构」那条纪律的兑现——
// 平铺会让 Store 的方法数往上走，而门禁的阈值就是按「一个类型几个方法」判的。
type vectorStore struct{ *Store }

// Upsert 写入或更新一条向量（scope 是这条向量的归属）。
func (s *vectorStore) Upsert(ctx context.Context, namespace, id, scope string, vec []float32) error {
	if id == "" {
		return errors.New("向量标识不能为空")
	}
	if len(vec) == 0 {
		return errors.New("向量不能为空")
	}
	// 同一个命名空间只许一种维度：维度不同的两条向量没法比余弦，
	// 而「比不出来」若被静默吞掉，症状是「语义召回像是没生效」。
	if err := s.checkVecDim(ctx, namespace, len(vec)); err != nil {
		return err
	}
	_, err := s.exec(ctx, `INSERT INTO atom_vectors (namespace, id, scope, dim, vec, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(namespace, id) DO UPDATE SET
			scope = excluded.scope, dim = excluded.dim, vec = excluded.vec,
			updated_at = excluded.updated_at`,
		namespace, id, scope, len(vec), encodeVec(vec), ts(time.Now().UTC()))
	return err
}

// checkVecDim 校验命名空间里的维度与给定维度一致。
//
// 维度不一致时报 ports.ErrDimensionMismatch 而不是各算各的：
// 静默「比不出来」的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
//
// 拒绝路径**带操作指引**：这个错是**预期防护**而非故障（换 embedding 模型
// 后必然出现一次），但宿主侧症状（写向量索引失败 + 语义召回全空）看起来
// 像坏了。文案把「清 atom_vectors」这个标准恢复动作直接给出来——
// 索引是派生物（事实卡是真源），清了可安全重建；
// 没有这行指引，宿主只能读源码悟（argus 实测踩过）。
func (s *vectorStore) checkVecDim(ctx context.Context, namespace string, dim int) error {
	rows, err := s.query(ctx, `SELECT DISTINCT dim FROM atom_vectors WHERE namespace = ?`, namespace)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			return err
		}
		if d != dim {
			return fmt.Errorf("%w：命名空间 %q 已存在 dim=%d 的向量，与本次 dim=%d 不一致。"+
				"这是预期防护（同命名空间混两种维度没法比余弦），不是故障。"+
				"换 embedding 模型的标准动作：确认旧向量不再需要后，"+
				"清空该命名空间的向量索引（DELETE FROM atom_vectors WHERE namespace = '%s'；"+
				"索引是派生物、事实卡是真源，可安全重建），再用新模型重建向量",
				ports.ErrDimensionMismatch, namespace, d, dim, namespace)
		}
	}
	return rows.Err()
}

// Search 返回最相似的若干条。
//
// # 归属过滤**进 SQL**
//
// scope 非空时取「全局的 + 同归属的」，别家的一条都不进候选池。
// 放在调用方后置的话，被别家占掉的 topK 槽位已经花掉了——
// 于是「该给我的那条」根本没进候选池，而症状只是「语义召回像是没生效」。
// scope 为空 = **不隔离**（与 store.AtomFilter.Scope 同一条语义）。
//
// # 暴力扫描的适用面
//
// 这是「把该命名空间的向量取回来、在 Go 里算余弦」的暴力实现：
// 它是**小库够用**的那一档——零外部依赖、跟着记忆库一起活（跨进程可用）。
// 大规模部署的宿主应注入自己的向量库（Chroma / Qdrant / PG 的向量扩展……），
// 本仓不替宿主选向量技术（核心不引向量库 SDK，见 architecture-policy.yaml）。
func (s *vectorStore) Search(ctx context.Context, namespace, scope string, vec []float32,
	topK int) ([]ports.VectorHit, error) {
	return s.searchVectors(ctx, namespace, func(q string, args []any) (string, []any) {
		if scope != "" {
			q += ` AND (scope = '' OR scope = ?)`
			args = append(args, scope)
		}
		return q, args
	}, vec, topK)
}

// SearchGlobal 以全局读者身份检索：只返回**归属为空**（全局）的向量。
//
// ports.GlobalVectorSearcher 的实现（issue #62）：Search 的空 scope 是
// 「不隔离」（冻结语义），全局读者不能共用那个空值——否则 TopK 配额
// 先被别家的向量占满，再被后置滤除，全局的那条根本没进候选池。
// 过滤同样在本层生效（与 TopK 同一次求值）。
func (s *vectorStore) SearchGlobal(ctx context.Context, namespace string, vec []float32,
	topK int) ([]ports.VectorHit, error) {
	return s.searchVectors(ctx, namespace, func(q string, args []any) (string, []any) {
		q += ` AND scope = ''`
		return q, args
	}, vec, topK)
}

// searchVectors 向量检索的公共骨架：scopeCond 决定哪一行**进候选池**。
//
// 把「谁配进池」单独抽出来，是因为空 scope 在冻结面上只能表达「不隔离」，
// 而全局读者要「只取全局」——两种判据共用同一套取数与算分。
func (s *vectorStore) searchVectors(ctx context.Context, namespace string,
	scopeCond func(q string, args []any) (string, []any), vec []float32,
	topK int) ([]ports.VectorHit, error) {
	if len(vec) == 0 {
		return nil, errors.New("查询向量不能为空")
	}
	// 维度先对齐：库里是 3 维、查询给 4 维时**必须报错**，
	// 静默返回空集的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
	if err := s.checkVecDim(ctx, namespace, len(vec)); err != nil {
		return nil, err
	}
	q := `SELECT id, vec FROM atom_vectors WHERE namespace = ?`
	args := []any{namespace}
	q, args = scopeCond(q, args)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hits []ports.VectorHit
	for rows.Next() {
		var (
			id  string
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		score, ok := cosineScore(decodeVec(raw), vec)
		if !ok {
			// 零向量没有方向，无从比较（跳过，不当命中）。
			continue
		}
		hits = append(hits, ports.VectorHit{ID: id, Score: float32(score)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortHits(hits)
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// sortHits 按分数倒序排；同分按标识升序。
//
// 同分时的标识序是**可复现性**的一部分：没有它，同样的查询每次返回的
// 次序可能不同，而融合层按名次算分——于是「召回结果不可复现」。
func sortHits(hits []ports.VectorHit) {
	slices.SortFunc(hits, func(a, b ports.VectorHit) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// Delete 删一条向量。
//
// 删不到不报错：删索引是清理动作，同一条被清两次是常态
// （事实卡被推翻两次、清理流程重跑），报错只会让清理路径变脆。
func (s *vectorStore) Delete(ctx context.Context, namespace, id string) error {
	_, err := s.exec(ctx, `DELETE FROM atom_vectors WHERE namespace = ? AND id = ?`,
		namespace, id)
	return err
}
