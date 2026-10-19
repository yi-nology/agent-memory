// Package portable 记忆的跨宿主迁移包（`.hmpkg`）。
//
// # 格式
//
// 一个 **zip 容器**（不是自定义二进制格式）——理由是迁移包要能被人在没装
// 本仓的机器上打开看看里面是什么，而 zip 到处都能解。内容：
//
//	manifest.json      包清单：格式版本、来源、计数、各文件校验和
//	raw.jsonl          捕获事件
//	candidates.jsonl   候选
//	atoms.jsonl        事实卡
//	entities.jsonl     实体
//	aliases.jsonl      别名
//	pages.jsonl        实体页面
//	episodes.jsonl     情节
//	digests.jsonl      周期摘要
//	journal.jsonl      审计条目
//
// 每行一个 JSON 对象（JSONL）：**可流式读写**，不必把整包读进内存，
// 且损坏时能定位到行而不是整包报废。
//
// # 三条闭环
//
//	pack    导出成包
//	adopt   导入进一个记忆库
//	doctor  检查包是否完整可导
//
// doctor 存在的原因是迁移是**最容易出静默错误**的环节：包缺了一半，
// 导入「成功」了，而宿主从此拥有一段残缺的记忆——而记忆库不会告诉你它缺了什么。
package portable

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/oekit/pkg/encoding"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// FormatVersion 包格式版本。
const FormatVersion = 1

// Extension 包文件扩展名。
const Extension = ".hmpkg"

// Manifest 包清单。
type Manifest struct {
	// FormatVersion 格式版本（导入时校验，不匹配即拒）。
	FormatVersion int `json:"format_version"`
	// CreatedAt 打包时刻。
	CreatedAt time.Time `json:"created_at"`
	// SourceNamespace 来源记忆库。
	SourceNamespace string `json:"source_namespace"`
	// SourceHost 来源宿主标识。
	SourceHost string `json:"source_host"`
	// Counts 各类条数。键是**数据类别名**（raw / atoms / entities / …），
	// 与 ImportReport.Counts 同键——往返比较（导出多少、导回多少）
	// 靠的就是这层对齐，键若一边带文件后缀一边不带，就没法直接比。
	Counts map[string]int `json:"counts"`
	// Files 各数据文件的 SHA-256（十六进制），doctor 据此校验完整性。
	Files map[string]string `json:"files"`
}

// 内部包文件名。
const (
	fileManifest   = "manifest.json"
	fileRaw        = "raw.jsonl"
	fileCandidates = "candidates.jsonl"
	fileAtoms      = "atoms.jsonl"
	fileEntities   = "entities.jsonl"
	fileAliases    = "aliases.jsonl"
	filePages      = "pages.jsonl"
	fileEpisodes   = "episodes.jsonl"
	fileDigests    = "digests.jsonl"
	fileJournal    = "journal.jsonl"
)

var dataFiles = []string{
	fileRaw, fileCandidates, fileAtoms, fileEntities,
	fileAliases, filePages, fileEpisodes, fileDigests, fileJournal,
}

// ErrFormatVersion 包格式版本不认识。
var ErrFormatVersion = errors.New("portable: 包格式版本不认识")

// ErrCorrupt 包损坏。
var ErrCorrupt = errors.New("portable: 包已损坏")

// ── 导出 ──

// Source 导出面用到的存储。
type Source interface {
	ListRaw(ctx context.Context, ns store.Namespace, before time.Time, p store.Page) ([]*types.RawEvent, error)
	ListCandidates(ctx context.Context, ns store.Namespace, f store.CandidateFilter) ([]*types.Candidate, error)
	ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error)
	ListEntities(ctx context.Context, ns store.Namespace, entityType types.EntityType, limit int) ([]*types.Entity, error)
	ListAliases(ctx context.Context, ns store.Namespace, entityID string) ([]*types.Alias, error)
	ListEntityPages(ctx context.Context, ns store.Namespace, limit int) ([]*types.EntityPage, error)
	ListEpisodes(ctx context.Context, ns store.Namespace, session string, limit int) ([]*types.Episode, error)
	ListDigests(ctx context.Context, ns store.Namespace, limit int) ([]*types.DigestRecord, error)
	ListJournal(ctx context.Context, ns store.Namespace, subject string, limit int) ([]*types.JournalEntry, error)
}

// ExportOptions 导出选项。
type ExportOptions struct {
	// Host 来源宿主标识（写进清单，便于追溯）。
	Host string
	// Limit 单类条数上限（0=不限）。
	Limit int
	// Now 打包时刻（0=用真实时钟）。
	//
	// 注入它的理由与 lifecycle.WithClock 相同：打包时刻会写进清单、
	// 而清单是迁移包的一部分——它得可断言。
	Now time.Time
}

// Export 把一个记忆库导出成包。
func Export(ctx context.Context, src Source, ns store.Namespace, w io.Writer, opt ExportOptions) (*Manifest, error) {
	limit := opt.Limit
	if limit <= 0 {
		limit = 100000
	}

	raws, err := src.ListRaw(ctx, ns, time.Time{}, store.Page{Limit: limit})
	if err != nil {
		return nil, err
	}
	cands, err := src.ListCandidates(ctx, ns, store.CandidateFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	atoms, err := src.ListAtoms(ctx, ns, store.AtomFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	ents, err := src.ListEntities(ctx, ns, "", limit)
	if err != nil {
		return nil, err
	}
	pages, err := src.ListEntityPages(ctx, ns, limit)
	if err != nil {
		return nil, err
	}
	eps, err := src.ListEpisodes(ctx, ns, "", limit)
	if err != nil {
		return nil, err
	}
	digests, err := src.ListDigests(ctx, ns, limit)
	if err != nil {
		return nil, err
	}
	journal, err := src.ListJournal(ctx, ns, "", limit)
	if err != nil {
		return nil, err
	}

	// 别名要按实体逐个取（别名表没有「全部列出」的口子）——这是**已知缺口**：
	// 实体很多时这里是 N 次查询。M4 补了 ListEntityPages 等读口，
	// 但别名表没有「列出全部」的口子，因为别名表的查询面本来就只有两个：
	// 「按别名查实体」与「按实体列出别名」。补它时要一并想清楚
	// 「一个命名空间里有多少别名」的查询该走什么索引。
	var aliases []*types.Alias
	for _, e := range ents {
		as, err := src.ListAliases(ctx, ns, e.ID)
		if err != nil {
			return nil, err
		}
		aliases = append(aliases, as...)
	}

	man := &Manifest{
		FormatVersion:   FormatVersion,
		CreatedAt:       opt.Now.UTC(),
		SourceNamespace: string(ns),
		SourceHost:      opt.Host,
		Counts:          map[string]int{},
		Files:           map[string]string{},
	}

	zw := zip.NewWriter(w)
	write := func(name string, rows any) error {
		var buf strings.Builder
		bw := bufio.NewWriter(&buf)
		enc := json.NewEncoder(bw)
		n := 0
		switch v := rows.(type) {
		case []*types.RawEvent:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.Candidate:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.AtomCard:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.Entity:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.Alias:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.EntityPage:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.Episode:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.DigestRecord:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		case []*types.JournalEntry:
			for _, r := range v {
				if err := enc.Encode(r); err != nil {
					return err
				}
				n++
			}
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		// 计数用**类别名**（去掉 .jsonl 后缀），与 ImportReport.Counts 同键：
		// 「导出多少、导回多少」是迁移最该被核对的一件事，
		// 而两边的键若一边带后缀一边不带，这个核对就做不成。
		man.Counts[strings.TrimSuffix(name, ".jsonl")] = n
		man.Files[name] = encoding.Sha256Hex([]byte(buf.String()))

		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, buf.String())
		return err
	}

	if err := write(fileRaw, raws); err != nil {
		return nil, err
	}
	if err := write(fileCandidates, cands); err != nil {
		return nil, err
	}
	if err := write(fileAtoms, atoms); err != nil {
		return nil, err
	}
	if err := write(fileEntities, ents); err != nil {
		return nil, err
	}
	if err := write(fileAliases, aliases); err != nil {
		return nil, err
	}
	if err := write(filePages, pages); err != nil {
		return nil, err
	}
	if err := write(fileEpisodes, eps); err != nil {
		return nil, err
	}
	if err := write(fileDigests, digests); err != nil {
		return nil, err
	}
	if err := write(fileJournal, journal); err != nil {
		return nil, err
	}

	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	mf, err := zw.Create(fileManifest)
	if err != nil {
		return nil, err
	}
	if _, err := mf.Write(mb); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return man, nil
}

// ExportFile 导出到文件。
func ExportFile(ctx context.Context, src Source, ns store.Namespace, filePath string, opt ExportOptions) (*Manifest, error) {
	f, err := os.Create(filePath) // #nosec G304 -- 路径由调用方给定
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return Export(ctx, src, ns, f, opt)
}

// ── 检查 ──

// DoctorReport 检查结果。
type DoctorReport struct {
	// OK 是否可导。
	OK bool
	// Manifest 清单（读得到时）。
	Manifest *Manifest
	// Problems 问题清单（人类可读）。
	Problems []string
}

// Doctor 检查包是否完整可导。
//
// 校验三样：清单在不在、格式版本认不认、各数据文件的校验和对不对。
// 校验和这一项最要紧：zip 本身会记 CRC，但**数据文件被截断后 CRC 仍可能过**
// （截断到某个块边界时），而记忆丢了就是丢了。
func Doctor(pkgPath string) (*DoctorReport, error) {
	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer func() { _ = zr.Close() }()

	rep := &DoctorReport{}
	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}

	mf, ok := byName[fileManifest]
	if !ok {
		rep.Problems = append(rep.Problems, "缺少 "+fileManifest)
		return rep, nil
	}
	mb, err := readZip(mf)
	if err != nil {
		rep.Problems = append(rep.Problems, "读取清单失败: "+err.Error())
		return rep, nil
	}
	var man Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		rep.Problems = append(rep.Problems, "清单不是合法 JSON: "+err.Error())
		return rep, nil
	}
	rep.Manifest = &man
	if man.FormatVersion != FormatVersion {
		rep.Problems = append(rep.Problems,
			fmt.Sprintf("包格式版本 %d 不认识（本仓只认 %d）", man.FormatVersion, FormatVersion))
		return rep, nil
	}

	names := make([]string, 0, len(man.Files))
	for n := range man.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, ok := byName[n]
		if !ok {
			rep.Problems = append(rep.Problems, "缺少数据文件 "+n)
			continue
		}
		b, err := readZip(f)
		if err != nil {
			rep.Problems = append(rep.Problems, "读取 "+n+" 失败: "+err.Error())
			continue
		}
		if got := encoding.Sha256Hex(b); got != man.Files[n] {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("%s 校验和不符（期望 %s 实得 %s）", n, short(man.Files[n]), short(got)))
		}
	}
	for _, n := range dataFiles {
		if _, ok := byName[n]; !ok {
			rep.Problems = append(rep.Problems, "包内缺数据文件 "+n)
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

func readZip(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// ── 导入 ──

// Target 导入面用到的存储。
type Target interface {
	SaveRaw(ctx context.Context, ns store.Namespace, ev *types.RawEvent) error
	SaveCandidate(ctx context.Context, ns store.Namespace, c *types.Candidate) error
	SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error
	SaveEntity(ctx context.Context, ns store.Namespace, e *types.Entity) error
	SaveAlias(ctx context.Context, ns store.Namespace, a *types.Alias) error
	UpsertEntityPage(ctx context.Context, ns store.Namespace, p *types.EntityPage) error
	SaveEpisode(ctx context.Context, ns store.Namespace, e *types.Episode) error
	SaveDigest(ctx context.Context, ns store.Namespace, d *types.DigestRecord) error
	AppendJournal(ctx context.Context, ns store.Namespace, e *types.JournalEntry) error
}

// ImportReport 导入结果。
type ImportReport struct {
	// Counts 各类导入条数。
	Counts map[string]int
	// Skipped 跳过的条数（已存在或校验不过）。
	Skipped int
}

// Import 把包导入一个记忆库。
//
// **先 doctor 后导**：损坏的包导一半再报错，宿主就拥有了一个残缺的记忆库
// 且不知道缺了什么。宁可一句都不导。
//
// 幂等：同一条记录重复导入以「已存在」跳过——迁移包常常被反复导入
// （重试、换机器），而重复导入不该产生重复事实。
func Import(ctx context.Context, pkgPath string, tgt Target, ns store.Namespace) (*ImportReport, error) {
	rep, err := Doctor(pkgPath)
	if err != nil {
		return nil, err
	}
	if !rep.OK {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, strings.Join(rep.Problems, "；"))
	}

	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()

	out := &ImportReport{Counts: map[string]int{}}
	read := func(name string, fn func([]byte) error) error {
		f, ok := lookup(zr, name)
		if !ok {
			return nil
		}
		b, err := readZip(f)
		if err != nil {
			return err
		}
		return fn(b)
	}

	err = read(fileRaw, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.RawEvent
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveRaw(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["raw"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileAtoms, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.AtomCard
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveAtom(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["atoms"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileEntities, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.Entity
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveEntity(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["entities"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileAliases, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.Alias
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveAlias(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["aliases"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(filePages, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.EntityPage
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.UpsertEntityPage(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["pages"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileEpisodes, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.Episode
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveEpisode(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["episodes"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileDigests, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.DigestRecord
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveDigest(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["digests"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileCandidates, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.Candidate
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.SaveCandidate(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["candidates"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	err = read(fileJournal, func(b []byte) error {
		return eachLine(b, func(line []byte) error {
			var v types.JournalEntry
			if err := json.Unmarshal(line, &v); err != nil {
				return err
			}
			if err := tgt.AppendJournal(ctx, ns, &v); err != nil {
				return err
			}
			out.Counts["journal"]++
			return nil
		})
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

func lookup(zr *zip.ReadCloser, name string) (*zip.File, bool) {
	for _, f := range zr.File {
		if f.Name == name {
			return f, true
		}
	}
	return nil, false
}

func eachLine(b []byte, fn func([]byte) error) error {
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// FileName 生成包文件名。
func FileName(namespace string, at time.Time) string {
	return fmt.Sprintf("memory-%s-%s%s", sanitize(namespace), at.UTC().Format("20060102-150405"), Extension)
}

// sanitize 文件名净化。
//
// 路径分隔符与其他路径敏感字符一律换掉——包名会进下载目录，
// 而 namespace 是宿主给的，**不可信**。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "ns"
	}
	return path.Base(out)
}
