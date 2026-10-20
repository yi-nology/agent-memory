package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

const candidateCols = `id, raw_event_ids, candidate_type, status, title, assertion,
	verbatim_quote, quote_event_id, subject_name, subject_entity_type, target_entity_id,
	confidence, importance, recommended_action, promotion_reason, extractor_version,
	session_id, scope, created_at, decided_at, decided_by, payload`

func (s *candidateStore) scanCandidate(rows *sql.Rows) (*types.Candidate, error) {
	var (
		c       types.Candidate
		rawIDs  string
		created sql.NullTime
		decided sql.NullTime
		payload string
	)
	if err := rows.Scan(&c.ID, &rawIDs, &c.Type, &c.Status, &c.Title, &c.Assertion,
		&c.VerbatimQuote, &c.QuoteEventID, &c.SubjectName, &c.SubjectEntityType,
		&c.TargetEntityID, &c.Confidence, &c.Importance, &c.RecommendedAction,
		&c.PromotionReason, &c.ExtractorVersion, &c.SessionID, &c.Scope, &created, &decided,
		&c.DecidedBy, &payload); err != nil {
		return nil, err
	}
	c.RawEventIDs = parseStrings(rawIDs)
	c.CreatedAt = fromTSPtr(created)
	c.DecidedAt = fromTSPtr(decided)
	c.Payload = parseMap(payload)
	return &c, nil
}

// candidateSignature 候选的重复检测键。
//
// 用「主体名 + 断言」而不是完整签名（types.AtomCard.Signature 带 EntityID）：
// 候选阶段实体还没归一，EntityID 此时是空的，用它会让所有未归一候选撞成同一签名。
// candidateSignature 候选签名（规则在 types 那一处，勿在这里另写一份）。
func candidateSignature(c *types.Candidate) string {
	return types.CandidateSignature(c.SubjectName, c.Scope, c.Assertion)
}

func (s *candidateStore) SaveCandidate(ctx context.Context, ns store.Namespace, c *types.Candidate) error {
	if err := c.Valid(); err != nil {
		return err
	}
	body := ftsBody(c.Title, c.Assertion, c.SubjectName)
	_, err := s.exec(ctx, `INSERT INTO candidates
		(namespace, id, raw_event_ids, candidate_type, status, title, assertion,
		 verbatim_quote, quote_event_id, subject_name, subject_entity_type, target_entity_id,
		 confidence, importance, recommended_action, promotion_reason, extractor_version,
		 session_id, scope, created_at, decided_at, decided_by, payload, signature, body, body_tsv)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,
		        to_tsvector('simple', $25))
		ON CONFLICT(namespace, id) DO UPDATE SET
			raw_event_ids = excluded.raw_event_ids, candidate_type = excluded.candidate_type,
			status = excluded.status, title = excluded.title, assertion = excluded.assertion,
			verbatim_quote = excluded.verbatim_quote, quote_event_id = excluded.quote_event_id,
			subject_name = excluded.subject_name, subject_entity_type = excluded.subject_entity_type,
			target_entity_id = excluded.target_entity_id, confidence = excluded.confidence,
			importance = excluded.importance, recommended_action = excluded.recommended_action,
			promotion_reason = excluded.promotion_reason, extractor_version = excluded.extractor_version,
			session_id = excluded.session_id, scope = excluded.scope, created_at = excluded.created_at,
			decided_at = excluded.decided_at, decided_by = excluded.decided_by,
			payload = excluded.payload, signature = excluded.signature,
			body = excluded.body, body_tsv = excluded.body_tsv`,
		nsOf(ns), c.ID, stringsJSON(c.RawEventIDs), string(c.Type), string(c.Status), c.Title,
		c.Assertion, c.VerbatimQuote, c.QuoteEventID, c.SubjectName, string(c.SubjectEntityType),
		c.TargetEntityID, string(c.Confidence), string(c.Importance), string(c.RecommendedAction),
		c.PromotionReason, c.ExtractorVersion, c.SessionID, string(c.Scope), ts(c.CreatedAt),
		tsPtr(c.DecidedAt), string(c.DecidedBy), mapJSON(c.Payload), candidateSignature(c), body)
	return err
}

func (s *candidateStore) GetCandidate(ctx context.Context, ns store.Namespace, id string) (*types.Candidate, error) {
	rows, err := s.query(ctx, `SELECT `+candidateCols+` FROM candidates WHERE namespace = $1 AND id = $2`,
		nsOf(ns), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanCandidate(rows)
}

// FindDuplicateCandidate 查同签名候选。
//
// 只认**未落定**的候选：一条已晋升的候选不该挡住下一个候选被判「重复」——
// 晋升层真正关心的是「事实卡里有没有这条」，那是 FindAtomBySignature 的事。
func (s *candidateStore) FindDuplicateCandidate(ctx context.Context, ns store.Namespace, signature string) (*types.Candidate, error) {
	rows, err := s.query(ctx, `SELECT `+candidateCols+` FROM candidates
		WHERE namespace = $1 AND signature = $2 AND status = $3 ORDER BY created_at DESC LIMIT 1`,
		nsOf(ns), signature, string(types.CandidatePending))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanCandidate(rows)
}

func (s *candidateStore) ListCandidates(ctx context.Context, ns store.Namespace, f store.CandidateFilter) ([]*types.Candidate, error) {
	var (
		where = []string{"namespace = $1"}
		args  = []any{nsOf(ns)}
	)
	if len(f.Statuses) > 0 {
		// 占位符从 $2 起：第一个位置已被 namespace 占了。
		where = append(where, "status IN ("+numberedPlaceholders(len(args)+1, len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	}
	if f.Session != "" {
		where = append(where, "session_id = $"+fmt.Sprint(len(args)+1))
		args = append(args, f.Session)
	}
	q := `SELECT ` + candidateCols + ` FROM candidates WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limitOf(f.Limit, 50))

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.Candidate
	for rows.Next() {
		c, err := s.scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// numberedPlaceholders 生成从 start 起的连续占位符（$2, $3, ...）。
func numberedPlaceholders(start, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "$%d", start+i)
	}
	return b.String()
}

// UpdateCandidateStatus 落定状态，带乐观锁。
//
// expected 非空时与库内现值不符即 ErrStaleWrite：两个晋升器同时处理同一条候选时，
// 后到的那个必须知道自己撞了，而不是默默覆盖前者的判定——否则「谁判的、为什么
// 判成这个」这条审计链就断了。
func (s *candidateStore) UpdateCandidateStatus(ctx context.Context, ns store.Namespace, id string,
	expected, next types.CandidateStatus, actor types.DecidedBy, reason string) error {
	if id == "" {
		return errors.New("候选 ID 不能为空")
	}
	// 枚举校验单独做：UPDATE 路径拿不到 RawEventIDs，跑整条 Valid() 会因
	// 「无素材来源」误判——那是保存路径的义务，不是状态变更路径的。
	if !isValidCandidateStatus(next) {
		return fmt.Errorf("未知候选状态 %q", next)
	}
	q := `UPDATE candidates SET status = $1, decided_by = $2, decided_at = $3, promotion_reason = $4
	      WHERE namespace = $5 AND id = $6`
	args := []any{string(next), string(actor), ts(time.Now().UTC()), reason, nsOf(ns), id}
	if expected != "" {
		q += ` AND status = $7`
		args = append(args, string(expected))
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// 分不清是「不存在」还是「被抢占」，两者都不该被当成成功。
		if _, gerr := s.GetCandidate(ctx, ns, id); gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				return store.ErrNotFound
			}
			return gerr
		}
		return store.ErrStaleWrite
	}
	return nil
}

func isValidCandidateStatus(st types.CandidateStatus) bool {
	switch st {
	case types.CandidatePending, types.CandidatePromoted, types.CandidateMerged,
		types.CandidateDropped, types.CandidateReview:
		return true
	default:
		return false
	}
}

func (s *candidateStore) SearchCandidates(ctx context.Context, ns store.Namespace, query string, limit int) ([]*types.Candidate, error) {
	ids, err := s.searchFTS(ctx, "candidates", nsOf(ns), query, limit, "")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	out := make([]*types.Candidate, 0, len(ids))
	for _, id := range ids {
		c, err := s.GetCandidate(ctx, ns, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
