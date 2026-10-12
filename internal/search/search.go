// Package search は検索語から bleve のクエリを組み立て、結果を API 用の形に整える。
//
// クエリの方針:
//   - 空白区切りの各語をすべて含むドキュメントを返す（AND）
//   - 日本語など非 ASCII を含む語は bigram のフレーズ一致（＝部分文字列一致）
//   - ASCII だけの語（数字・記号を含んでもよい）は前方一致（入力途中を許容）。
//     そのうち英数字だけで 4 文字以上の語は、編集距離 1 の fuzzy も効かせる（typo を許容）
//   - タイトル・ファイル名の一致は本文より高く評価する
package search

import (
	"strings"
	"time"
	"unicode"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/abetd/notereader/internal/indexer"
)

// Sort は並び順。
type Sort string

const (
	SortScore Sort = "score"
	SortMtime Sort = "mtime"
	// DefaultSort は sort を指定しなかったときの並び順。
	DefaultSort = SortMtime
)

const (
	DefaultLimit = 20
	MaxLimit     = 100

	titleBoost    = 3.0
	filenameBoost = 2.0
)

// Searcher は検索を実行できるもの（indexer.Index が満たす）。
type Searcher interface {
	Search(req *bleve.SearchRequest) (*bleve.SearchResult, error)
}

// Archive はアーカイブの判定（*config.Config が満たす）。
type Archive interface {
	// ArchiveRegexp はアーカイブ内のファイルの rel_path 全体に一致する正規表現。空ならアーカイブは無い。
	ArchiveRegexp() string
	// Archived は相対パスのファイルがアーカイブ内にあるか。
	Archived(rel string) bool
}

// Request は検索条件。
type Request struct {
	Query string
	Sort  Sort
	Limit int
	// Root が空でなければ、そのルートに属するドキュメントだけを対象にする。
	Root string
	// Archive が nil でなければ、結果にアーカイブかどうかを付け、
	// IncludeArchive が false ならアーカイブ内のファイルを結果から除く。
	// Archive が nil なら IncludeArchive によらず何も除かない。
	Archive        Archive
	IncludeArchive bool
}

// Hit は検索結果 1 件。
type Hit struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Type は "html" / "markdown"。
	Type    string    `json:"type"`
	Root    string    `json:"root"`
	RelPath string    `json:"rel_path"`
	Path    string    `json:"path"`
	Mtime   time.Time `json:"mtime"`
	Score   float64   `json:"score"`
	// Snippet は一致箇所を <mark> で囲んだ HTML。<mark> 以外はすべて HTML エスケープ済み（画面は innerHTML に入れる）。
	Snippet string `json:"snippet"`
	URL     string `json:"url"`
	// Archived はアーカイブ内のファイルか。
	Archived bool `json:"archived"`
}

// Response は検索結果。
type Response struct {
	Total uint64 `json:"total"`
	Hits  []Hit  `json:"hits"`
	Took  string `json:"took"`
}

// Do は検索を実行する。
func Do(s Searcher, r Request) (*Response, error) {
	if r.Limit <= 0 {
		r.Limit = DefaultLimit
	}
	r.Limit = min(r.Limit, MaxLimit)

	q, hasTerms := buildQuery(r.Query)
	if r.Root != "" {
		root := bleve.NewTermQuery(r.Root)
		root.SetField(indexer.FieldRoot)
		q = bleve.NewConjunctionQuery(q, root)
	}
	if r.Archive != nil && !r.IncludeArchive {
		if expr := r.Archive.ArchiveRegexp(); expr != "" {
			archived := bleve.NewRegexpQuery(expr)
			archived.SetField(indexer.FieldRelPath)
			b := bleve.NewBooleanQuery()
			b.AddMust(q)
			b.AddMustNot(archived)
			q = b
		}
	}
	req := bleve.NewSearchRequestOptions(q, r.Limit, 0, false)
	req.Fields = []string{
		indexer.FieldTitle, indexer.FieldType, indexer.FieldRoot, indexer.FieldRelPath,
		indexer.FieldPath, indexer.FieldMtime, indexer.FieldSummary,
	}
	// 検索語が無いときはスコアが付かないので、指定に関わらず新しい順にする
	if r.Sort == SortScore && hasTerms {
		req.SortBy([]string{"-_score", "-" + indexer.FieldMtime})
	} else {
		req.SortBy([]string{"-" + indexer.FieldMtime})
	}
	if hasTerms {
		req.Highlight = bleve.NewHighlightWithStyle("html")
		req.Highlight.AddField(indexer.FieldBody)
	}

	res, err := s.Search(req)
	if err != nil {
		return nil, err
	}

	out := &Response{Total: res.Total, Hits: make([]Hit, 0, len(res.Hits)), Took: res.Took.String()}
	for _, h := range res.Hits {
		hit := Hit{
			ID:    h.ID,
			Score: h.Score,
			URL:   "/view/" + h.ID,
		}
		hit.Title, _ = h.Fields[indexer.FieldTitle].(string)
		hit.Type, _ = h.Fields[indexer.FieldType].(string)
		hit.Root, _ = h.Fields[indexer.FieldRoot].(string)
		hit.RelPath, _ = h.Fields[indexer.FieldRelPath].(string)
		hit.Path, _ = h.Fields[indexer.FieldPath].(string)
		if r.Archive != nil {
			hit.Archived = r.Archive.Archived(hit.RelPath)
		}
		if s, ok := h.Fields[indexer.FieldMtime].(string); ok {
			hit.Mtime, _ = time.Parse(time.RFC3339, s)
		}
		if frags := h.Fragments[indexer.FieldBody]; len(frags) > 0 {
			hit.Snippet = frags[0]
		} else if s, ok := h.Fields[indexer.FieldSummary].(string); ok {
			hit.Snippet = escape(s)
		}
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

// buildQuery は検索語からクエリを作る。語が 1 つも無ければ全件クエリと false を返す。
func buildQuery(q string) (query.Query, bool) {
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return bleve.NewMatchAllQuery(), false
	}
	conj := make([]query.Query, 0, len(terms))
	for _, t := range terms {
		conj = append(conj, termQuery(t))
	}
	return bleve.NewConjunctionQuery(conj...), true
}

func termQuery(term string) query.Query {
	fields := []struct {
		name  string
		boost float64
	}{
		{indexer.FieldTitle, titleBoost},
		{indexer.FieldFilename, filenameBoost},
		{indexer.FieldBody, 1},
	}
	var disj []query.Query
	for _, f := range fields {
		if isASCII(term) {
			m := bleve.NewMatchQuery(term)
			m.SetField(f.name)
			m.SetBoost(f.boost)
			// "b.html" のように記号で分割される語は、分割後の全トークンを要求する
			m.SetOperator(query.MatchQueryOperatorAnd)
			// fuzzy は分割後の各トークンに効くため、1 文字トークン（"b" → "a" 等）が
			// 誤一致しないよう、記号を含まない 4 文字以上の語に限る
			if len(term) >= 4 && isAlnum(term) {
				m.SetFuzziness(1)
			}
			p := bleve.NewPrefixQuery(strings.ToLower(term))
			p.SetField(f.name)
			p.SetBoost(f.boost * 0.5)
			disj = append(disj, m, p)
			continue
		}
		p := bleve.NewMatchPhraseQuery(term)
		p.SetField(f.name)
		p.SetBoost(f.boost)
		disj = append(disj, p)
	}
	return bleve.NewDisjunctionQuery(disj...)
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")

func escape(s string) string { return escaper.Replace(s) }
