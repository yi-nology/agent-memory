package types

import "time"

// MemoryNodeLevel 树节点层级。
type MemoryNodeLevel string

const (
	// LevelRoot 根节点（一个 namespace 一棵）。
	LevelRoot MemoryNodeLevel = "root"
	// LevelBranch 分支节点（按主题或实体聚拢）。
	LevelBranch MemoryNodeLevel = "branch"
	// LevelLeaf 叶节点（**只持 AtomID，不复制内容**）。
	LevelLeaf MemoryNodeLevel = "leaf"
)

// MemoryNode 记忆树上的一格。
//
// # 它不是第二份事实
//
// leaf 持有 AtomID，正文由 AtomCard 提供。这是刻意的：一旦 leaf 复制正文，
// 「事实被推翻」就只改了 atoms 表、树上的旧正文还在，于是召回可能捞到已退役的
// 事实——而这种 bug 极难查（数据看起来是对的，只是过时了）。
//
// 组织视图存在的意义是**给人看**（控制台浏览）与**给召回做粗筛**（先定位分支
// 再取事实），不是给存储再造一份。
type MemoryNode struct {
	// ID 节点标识。
	ID string
	// ParentID 父节点标识（根为 None）。
	ParentID *string
	// Level 层级。
	Level MemoryNodeLevel
	// Content 本节点自身的文本（分支=标题，根=namespace 名）。
	//
	// **叶节点忽略此字段**：leaf 的内容由 AtomID 投影得到，两者不一致时以事实卡为准。
	Content string
	// Topic 主题标识（叶子可为空）。
	Topic *string
	// ConversationID 关联会话。
	ConversationID *string
	// AtomID 引用的事实卡（**仅叶节点非空**）。
	AtomID *string
	// CreatedAt 创建时刻。
	CreatedAt time.Time
	// UpdatedAt 更新时刻。
	UpdatedAt time.Time
	// Metadata 附加载荷。
	Metadata map[string]any
}

// Leaf 是否为叶节点。
func (n *MemoryNode) Leaf() bool { return n.Level == LevelLeaf }

// Valid 节点形状校验。
//
// 拒绝的两类形状都是**会把 bug 藏起来**的：叶节点没有 AtomID（召回时投影成空），
// 非叶节点却有 AtomID（事实挂在不该在的地方，且没有任何视图能发现）。
func (n *MemoryNode) Valid() error {
	if n.ID == "" {
		return ErrNodeNoID
	}
	switch n.Level {
	case LevelLeaf:
		if n.AtomID == nil || *n.AtomID == "" {
			return ErrLeafWithoutAtom
		}
	case LevelRoot, LevelBranch:
		if n.AtomID != nil && *n.AtomID != "" {
			return ErrNonLeafWithAtom
		}
	default:
		return ErrNodeBadLevel
	}
	return nil
}
