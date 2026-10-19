package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

func (s *entityStore) SaveEntity(ctx context.Context, ns store.Namespace, e *types.Entity) error {
	if e.ID == "" {
		return errors.New("实体 ID 不能为空")
	}
	_, err := s.exec(ctx, `INSERT INTO entities
		(namespace, id, entity_type, canonical_name, atom_count, created_at, last_promoted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(namespace, id) DO UPDATE SET
			entity_type = excluded.entity_type,
			canonical_name = excluded.canonical_name`,
		nsOf(ns), e.ID, string(e.EntityType), e.CanonicalName, e.AtomCount,
		ts(e.CreatedAt), tsPtr(e.LastPromotedAt))
	return err
}

func (s *entityStore) GetEntity(ctx context.Context, ns store.Namespace, id string) (*types.Entity, error) {
	rows, err := s.query(ctx, `SELECT id, entity_type, canonical_name, atom_count,
		created_at, last_promoted_at FROM entities WHERE namespace = $1 AND id = $2`, nsOf(ns), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return scanEntity(rows)
}

func scanEntity(rows *sql.Rows) (*types.Entity, error) {
	var (
		e       types.Entity
		last    sql.NullTime
		created sql.NullTime
	)
	if err := rows.Scan(&e.ID, &e.EntityType, &e.CanonicalName, &e.AtomCount, &created, &last); err != nil {
		return nil, err
	}
	e.CreatedAt = fromTSPtr(created)
	e.LastPromotedAt = fromTSPtr(last)
	return &e, nil
}

// FindEntityByName 规范名与别名**两条路径都走**。
//
// 漏掉任一条的后果不一样：漏规范名 → 新建的同一实体裂成两个；
// 漏别名 → 用户换个说法提问就召回不到，而用户完全不知道自己「换了个说法」。
func (s *entityStore) FindEntityByName(ctx context.Context, ns store.Namespace, name string) (*types.Entity, error) {
	n := types.NormalizeAlias(name)
	if n == "" {
		return nil, store.ErrNotFound
	}
	ent, err := s.queryEntityOne(ctx,
		`SELECT e.id, e.entity_type, e.canonical_name, e.atom_count, e.created_at, e.last_promoted_at
		 FROM entities e
		 WHERE e.namespace = $1 AND lower(trim(e.canonical_name)) = $2 LIMIT 1`,
		nsOf(ns), n)
	if err == nil {
		return s.withAliases(ctx, ns, ent)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// 规范名没命中再走别名。缺这条路径的后果是「用户换个说法就召回不到」，
	// 而用户完全不知道自己换了个说法。
	ent, err = s.queryEntityOne(ctx,
		`SELECT e.id, e.entity_type, e.canonical_name, e.atom_count, e.created_at, e.last_promoted_at
		 FROM aliases a JOIN entities e ON e.namespace = a.namespace AND e.id = a.entity_id
		 WHERE a.namespace = $1 AND a.alias = $2 LIMIT 1`,
		nsOf(ns), n)
	if err != nil {
		return nil, err
	}
	return s.withAliases(ctx, ns, ent)
}

// queryEntityOne 单行查询取实体：扫完立刻关游标再返回。
//
// 单独抽出来是因为「关游标」这件事分散在各处就一定会漏一处，而漏掉的那一处
// 在连接池宽裕时完全正常、只在池宽为 1 时挂起——那种 bug 上线后可能几个月才出现
// 一次，且每次都指向不同的调用点。
func (s *entityStore) queryEntityOne(ctx context.Context, q string, args ...any) (*types.Entity, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ent *types.Entity
	if rows.Next() {
		ent, err = scanEntity(rows)
	}
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if ent == nil {
		return nil, store.ErrNotFound
	}
	return ent, nil
}

// withAliases 把别名表投影成 Entity.Aliases（字符串面）。
//
// 存储返回带来源与创建者的完整别名记录（审计要看谁建的），
// 而 Entity 上只挂字符串面供渲染——两者的信息量不同，故分两个方法而非一个。
func (s *entityStore) withAliases(ctx context.Context, ns store.Namespace, e *types.Entity) (*types.Entity, error) {
	as, err := s.ListAliases(ctx, ns, e.ID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(as))
	for _, a := range as {
		names = append(names, a.Alias)
	}
	e.Aliases = names
	return e, nil
}

func (s *entityStore) ListEntities(ctx context.Context, ns store.Namespace, entityType types.EntityType, limit int) ([]*types.Entity, error) {
	q := `SELECT id, entity_type, canonical_name, atom_count, created_at, last_promoted_at
		FROM entities WHERE namespace = $1`
	args := []any{nsOf(ns)}
	if entityType != "" {
		q += ` AND entity_type = $2`
		args = append(args, string(entityType))
	}
	q += ` ORDER BY atom_count DESC, canonical_name ASC LIMIT $` + itoa(len(args)+1)
	args = append(args, limitOf(limit, 50))

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// BumpEntityAtomCount 事实卡增减时同步计数。
//
// 计数允许为负而不报错：退役一条事实时 delta 是 -1，而该实体的计数可能因
// 历史脏数据已经是 0。此时钳到 0 是对的（计数只用于排序与页重生成触发，
// 负数会让这两个行为都失真），但**报错**会让正常退役流程失败。
func (s *entityStore) BumpEntityAtomCount(ctx context.Context, ns store.Namespace, entityID string, delta int) error {
	if entityID == "" || delta == 0 {
		return nil
	}
	_, err := s.exec(ctx, `UPDATE entities SET atom_count = GREATEST(0, atom_count + $1)
		WHERE namespace = $2 AND id = $3`, delta, nsOf(ns), entityID)
	return err
}

func (s *entityStore) SaveAlias(ctx context.Context, ns store.Namespace, a *types.Alias) error {
	if a.Alias == "" || a.EntityID == "" {
		return errEmptyAlias
	}
	_, err := s.exec(ctx, `INSERT INTO aliases
		(namespace, alias, entity_id, entity_type, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT(namespace, alias) DO UPDATE SET
			entity_id = excluded.entity_id,
			entity_type = excluded.entity_type,
			created_by = excluded.created_by`,
		nsOf(ns), types.NormalizeAlias(a.Alias), a.EntityID, string(a.EntityType),
		string(a.CreatedBy), ts(a.CreatedAt))
	return err
}

func (s *entityStore) ListAliases(ctx context.Context, ns store.Namespace, entityID string) ([]*types.Alias, error) {
	rows, err := s.query(ctx,
		`SELECT alias, entity_id, entity_type, created_by, created_at FROM aliases
		 WHERE namespace = $1 AND entity_id = $2 ORDER BY alias`,
		nsOf(ns), entityID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.Alias
	for rows.Next() {
		var (
			a       types.Alias
			created sql.NullTime
		)
		if err := rows.Scan(&a.Alias, &a.EntityID, &a.EntityType, &a.CreatedBy, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = fromTSPtr(created)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ── 活跃实体栈 ──

func (s *entityStore) UpsertActiveEntity(ctx context.Context, ns store.Namespace, a *types.ActiveEntity) error {
	if a.ThreadID == "" || a.EntityID == "" {
		return errEmptyActiveEntity
	}
	_, err := s.exec(ctx, `INSERT INTO thread_active_entities
		(namespace, thread_id, entity_id, canonical_name, last_seen, pinned, pin_time)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(namespace, thread_id, entity_id) DO UPDATE SET
			canonical_name = excluded.canonical_name,
			last_seen = excluded.last_seen,
			pinned = excluded.pinned,
			pin_time = COALESCE(excluded.pin_time, thread_active_entities.pin_time)`,
		nsOf(ns), a.ThreadID, a.EntityID, a.CanonicalName, ts(a.LastSeen),
		a.Pinned, tsPtr(a.PinTime))
	return err
}

func (s *entityStore) ListActiveEntities(ctx context.Context, ns store.Namespace, threadID string, limit int) ([]*types.ActiveEntity, error) {
	rows, err := s.query(ctx,
		`SELECT entity_id, canonical_name, last_seen, pinned, pin_time
		 FROM thread_active_entities WHERE namespace = $1 AND thread_id = $2
		 ORDER BY pinned DESC, last_seen DESC LIMIT $3`,
		nsOf(ns), threadID, limitOf(limit, 10))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.ActiveEntity
	for rows.Next() {
		var (
			a    types.ActiveEntity
			seen sql.NullTime
			pin  sql.NullTime
		)
		if err := rows.Scan(&a.EntityID, &a.CanonicalName, &seen, &a.Pinned, &pin); err != nil {
			return nil, err
		}
		a.ThreadID = threadID
		a.LastSeen = fromTSPtr(seen)
		a.PinTime = fromTSPtr(pin)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// EvictActiveEntities 淘汰到只剩 keep 条，返回被淘汰的实体。
//
// 钉住项（pinned）**不参与淘汰**：用户刚确认过的对象滑出栈，会让下一句「那个」
// 立刻指错人——而用户完全看不出发生了什么。
func (s *entityStore) EvictActiveEntities(ctx context.Context, ns store.Namespace, threadID string, keep int) ([]types.ActiveEntity, error) {
	if keep < 0 {
		keep = 0
	}
	rows, err := s.query(ctx,
		`SELECT entity_id, canonical_name, last_seen, pin_time FROM thread_active_entities
		 WHERE namespace = $1 AND thread_id = $2 AND NOT pinned
		 ORDER BY last_seen DESC OFFSET $3`,
		nsOf(ns), threadID, keep)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var victims []*types.ActiveEntity
	for rows.Next() {
		var (
			a    types.ActiveEntity
			seen sql.NullTime
			pin  sql.NullTime
		)
		if err := rows.Scan(&a.EntityID, &a.CanonicalName, &seen, &pin); err != nil {
			return nil, err
		}
		a.ThreadID = threadID
		a.LastSeen = fromTSPtr(seen)
		a.PinTime = fromTSPtr(pin)
		victims = append(victims, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 逐条删除前先关游标：占着连接时删除语句会等死。
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]types.ActiveEntity, 0, len(victims))
	for _, v := range victims {
		if _, err := s.exec(ctx,
			`DELETE FROM thread_active_entities WHERE namespace = $1 AND thread_id = $2 AND entity_id = $3`,
			nsOf(ns), threadID, v.EntityID); err != nil {
			return out, err
		}
		out = append(out, *v)
	}
	return out, nil
}
