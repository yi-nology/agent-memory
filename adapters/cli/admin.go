package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/yi-nology/agent-memory/lifecycle"
	"github.com/yi-nology/agent-memory/pipeline/entity"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件是 CLI 的**库管理面**：生命周期动作、实体归一的修正、情节查看。
//
// # 为什么要有这一面
//
// 内核里这些能力**实现了却没有入口**时，它们既不会被用，也不会被验证——
// 而「部件造好了、链没连上」是本仓最贵的一种病：它让人以为这些能力可用。
//
// 触发面的分工：
//
//   - 库管理动作（回收 / 合并 / 保留期 / 物理整理）→ CLI 就是它们的天然入口；
//   - 实体归一的修正（合并 / 登记别名）→ 人工处置面，CLI 是它的入口
//     （归一错了极难归因，必须有一条人走的修路）；
//   - 情节/摘要与页面重生成**需要模型** → 宿主触发。
//     CLI 刻意不注入模型（它是管理口，看与搬，不是生成口）。

// cmdGC 回收原始事件。
//
// 按龄清理是记忆库唯一的有界增长点：原始事件是**素材**面，
// 而事实卡只软退役不硬删（被推翻的事实曾经是真的）——
// 所以没有这一步，素材面会无界增长。
func cmdGC(ctx context.Context, opt Options, stdout, stderr io.Writer, before time.Duration) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	cutoff := time.Now().UTC().Add(-before)
	res, err := lifecycle.GC(ctx, svc.Store(), store.Namespace(opt.Namespace), cutoff)
	if err != nil {
		fmt.Fprintln(stderr, "回收失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已回收原始事件 %d 条，顺带退役 %d 张事实卡（早于 %s）\n",
		res.RawDeleted, res.AtomsRetired, cutoff.Format(time.RFC3339))
	return 0
}

// cmdConsolidate 合并同签名事实。
func cmdConsolidate(ctx context.Context, opt Options, stdout, stderr io.Writer) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	res, err := lifecycle.Consolidate(ctx, svc.Store(), store.Namespace(opt.Namespace))
	if err != nil {
		fmt.Fprintln(stderr, "合并失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已合并 %d 张事实卡，迁移证据 %d 条\n", res.Merged, res.EvidenceMoved)
	return 0
}

// cmdRetention 退役「证据全部来自运行期错误」的事实。
func cmdRetention(ctx context.Context, opt Options, stdout, stderr io.Writer) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	res, err := lifecycle.Retention(ctx, svc.Store(), store.Namespace(opt.Namespace))
	if err != nil {
		fmt.Fprintln(stderr, "保留期处理失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已退役 %d 张事实卡、%d 条候选\n", res.Retired, res.CandidatesRetired)
	return 0
}

// cmdVacuum 物理整理（后端专属能力）。
func cmdVacuum(ctx context.Context, opt Options, stdout, stderr io.Writer) int {
	_, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	if err := lifecycle.Vacuum(ctx, st); err != nil {
		fmt.Fprintln(stderr, "物理整理失败:", err)
		return 1
	}
	fmt.Fprintln(stdout, "已整理")
	return 0
}

// cmdEntityList 列出实体。
func cmdEntityList(ctx context.Context, opt Options, stdout, stderr io.Writer, limit int) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	ents, err := svc.Store().ListEntities(ctx, store.Namespace(opt.Namespace), "", limit)
	if err != nil {
		fmt.Fprintln(stderr, "列实体失败:", err)
		return 1
	}
	for _, e := range ents {
		fmt.Fprintf(stdout, "- %s  %s（%s）事实卡 %d 条\n", e.ID, e.CanonicalName, e.EntityType, e.AtomCount)
	}
	fmt.Fprintf(stderr, "共 %d 个实体\n", len(ents))
	return 0
}

// cmdEntityMerge 合并两个实体。
//
// 归一错了极难归因（症状是「总是答非所问」），所以必须有一条人走的修路：
// 把确实是同一个对象的两个实体合并，事实卡与别名一起迁过去。
func cmdEntityMerge(ctx context.Context, opt Options, stdout, stderr io.Writer, toID, fromID string) int {
	if toID == "" || fromID == "" {
		fmt.Fprintln(stderr, "entity merge 需要 --to 与 --from")
		return 2
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	r, err := entity.New(svc.Store())
	if err != nil {
		fmt.Fprintln(stderr, "装配失败:", err)
		return 1
	}
	res, err := r.Merge(ctx, store.Namespace(opt.Namespace), toID, fromID)
	if err != nil {
		fmt.Fprintln(stderr, "合并失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已把 %s 并入 %s：迁移事实卡 %d 张\n",
		res.Merged.ID, res.Target.ID, res.MovedAtoms)
	return 0
}

// cmdEntityAlias 给实体登记一个别名。
func cmdEntityAlias(ctx context.Context, opt Options, stdout, stderr io.Writer,
	entityID, alias, entityType string) int {
	if entityID == "" || alias == "" {
		fmt.Fprintln(stderr, "entity alias 需要 --entity 与 --alias")
		return 2
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	r, err := entity.New(svc.Store())
	if err != nil {
		fmt.Fprintln(stderr, "装配失败:", err)
		return 1
	}
	e, err := r.Register(ctx, store.Namespace(opt.Namespace), entityID, types.EntityType(entityType),
		alias, types.DecidedUser)
	if err != nil {
		// 冲突是「两个对象抢同一个名字」——内核判不出谁对，交给人。
		fmt.Fprintln(stderr, "登记失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已登记别名 %s → %s\n", alias, e.ID)
	return 0
}

// cmdEpisodeList 列出情节。
func cmdEpisodeList(ctx context.Context, opt Options, stdout, stderr io.Writer,
	session string, limit int) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	eps, err := svc.Store().ListEpisodes(ctx, store.Namespace(opt.Namespace), session, limit)
	if err != nil {
		fmt.Fprintln(stderr, "列情节失败:", err)
		return 1
	}
	for _, e := range eps {
		fmt.Fprintf(stdout, "- %s  %s：%s\n", e.ID, e.Title, e.Summary)
	}
	fmt.Fprintf(stderr, "共 %d 条情节\n", len(eps))
	return 0
}
