package voice

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
)

// taskCandidate contains task facts, not a model-facing identifier or a semantic match.
type taskCandidate struct {
	Request, Status, CreatedAt, FinishedAt, Question, Result string
	Additions                                                []string
}

type scoredTaskCandidate struct {
	taskCandidate
	id      string
	score   int
	created time.Time
}

const candidateColumns = `t.id,coalesce(t.title,''),t.status,t.created_at,coalesce(t.finished_at,''),
CASE WHEN t.status IN ('waiting','awaiting_user') AND t.interaction_state IN ('awaiting_message','explicit_confirmation') THEN coalesce(t.interaction_question,'') ELSE '' END,
substr(coalesce(t.result,''),1,600)`

// taskCandidates retrieves bounded lexical candidates across calls. Ranking chooses
// membership; the final facts are newest first. It performs no writes or inference.
func (s *Service) taskCandidates(ctx context.Context, q rosterQueryer, workspace, owner, text string, now time.Time, limit int) ([]taskCandidate, error) {
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	read := func(query string, args ...any) ([]scoredTaskCandidate, error) {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []scoredTaskCandidate
		for rows.Next() {
			var c scoredTaskCandidate
			if err := rows.Scan(&c.id, &c.Request, &c.Status, &c.CreatedAt, &c.FinishedAt, &c.Question, &c.Result, &c.score); err != nil {
				return nil, err
			}
			c.created, err = time.Parse(time.RFC3339Nano, c.CreatedAt)
			if err != nil {
				return nil, err
			}
			c.CreatedAt = stamp(c.created)
			if c.FinishedAt != "" {
				finished, err := time.Parse(time.RFC3339Nano, c.FinishedAt)
				if err != nil {
					return nil, err
				}
				c.FinishedAt = stamp(finished)
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}
	selected, err := read(`SELECT `+candidateColumns+`,0 FROM tasks t
JOIN voice_task_links l ON l.task_id=t.id AND l.workspace_id=t.workspace_id
WHERE t.workspace_id=? AND t.owner_subject_id=?
AND l.session_id=(SELECT id FROM voice_sessions WHERE workspace_id=? AND owner_id=? AND julianday(created_at)<=julianday(?) ORDER BY julianday(created_at) DESC,id DESC LIMIT 1)
AND t.status IN ('queued','active','waiting','awaiting_user')
AND julianday(t.created_at) BETWEEN julianday(?) AND julianday(?)
ORDER BY julianday(t.created_at) DESC,t.id DESC LIMIT 5`, workspace, owner, workspace, owner, stamp(now), stamp(now.Add(-time.Hour)), stamp(now))
	if err != nil {
		return nil, err
	}
	if len(selected) > limit {
		selected = selected[:limit]
	}
	terms := candidateTerms(text)
	if len(terms) > 0 && len(selected) < limit {
		patterns := make([]string, 0, len(terms))
		for _, term := range terms {
			patterns = append(patterns, candidateLikePattern(term))
		}
		encodedPatterns, err := json.Marshal(patterns)
		if err != nil {
			return nil, err
		}
		// A term table avoids SQL expression/parameter limits. Score every eligible
		// task, but fetch result snippets only for the bounded top-ranked set.
		// Reserve enough rows to allow for overlap with the always-include set.
		matches, err := read(`WITH scored AS MATERIALIZED (
SELECT t.id,t.created_at,(SELECT sum(
CASE WHEN lower(t.title) LIKE p.value ESCAPE '\' THEN 2 ELSE 0 END +
CASE WHEN lower(coalesce(t.result,'')) LIKE p.value ESCAPE '\' THEN 1 ELSE 0 END)
FROM json_each(?) p) AS score
FROM tasks t WHERE t.workspace_id=? AND t.owner_subject_id=?
AND t.status IN ('queued','active','waiting','awaiting_user','complete')
AND julianday(t.created_at)<=julianday(?)), ranked AS (
SELECT id,score FROM scored WHERE score>0
ORDER BY score DESC,julianday(created_at) DESC,id DESC LIMIT ?)
SELECT `+candidateColumns+`,r.score FROM ranked r JOIN tasks t ON t.id=r.id
ORDER BY r.score DESC,julianday(t.created_at) DESC,t.id DESC`, string(encodedPatterns), workspace, owner, stamp(now), limit+len(selected))
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool)
		for _, c := range selected {
			seen[c.id] = true
		}
		for _, c := range matches {
			if c.score > 0 && !seen[c.id] && len(selected) < limit {
				selected = append(selected, c)
				seen[c.id] = true
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].created.Equal(selected[j].created) {
			return selected[i].id > selected[j].id
		}
		return selected[i].created.After(selected[j].created)
	})
	var out []taskCandidate
	for _, c := range selected {
		rows, err := q.QueryContext(ctx, `SELECT substr(coalesce(content,''),1,300) FROM task_entries
WHERE workspace_id=? AND task_id=? AND kind='user_message'
AND julianday(created_at)>julianday(?) AND julianday(created_at)<=julianday(?)
ORDER BY julianday(created_at) DESC,id DESC LIMIT 3`, workspace, c.id, c.CreatedAt, stamp(now))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var addition string
			if err := rows.Scan(&addition); err != nil {
				rows.Close()
				return nil, err
			}
			c.Additions = append(c.Additions, addition)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		// Present the last three additions in conversational order.
		for i, j := 0, len(c.Additions)-1; i < j; i, j = i+1, j-1 {
			c.Additions[i], c.Additions[j] = c.Additions[j], c.Additions[i]
		}
		out = append(out, c.taskCandidate)
	}
	return out, nil
}

func candidateLikePattern(term string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(term) + "%"
}

// candidateTerms uses Latin words/digits and CJK bigrams, without stemming,
// translation or synonym expansion. Only English function words are discarded.
func candidateTerms(text string) []string {
	stop := map[string]bool{}
	for _, word := range strings.Fields("a an the and or but is are was were be been being to of in on at for from with by this that these those it its my me i you your we our they their do does did how what when where which please about") {
		stop[word] = true
	}
	var terms []string
	seen := map[string]bool{}
	add := func(term string) {
		if !seen[term] && !stop[term] {
			terms = append(terms, term)
			seen[term] = true
		}
	}
	kind := func(r rune) int {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return 2
		}
		if unicode.Is(unicode.Latin, r) || unicode.IsDigit(r) {
			return 1
		}
		return 0
	}
	runes := []rune(strings.ToLower(text))
	for i := 0; i < len(runes); {
		k := kind(runes[i])
		j := i + 1
		for j < len(runes) && kind(runes[j]) == k {
			j++
		}
		if k == 1 && j-i >= 2 || k == 2 && j-i == 1 {
			add(string(runes[i:j]))
		} else if k == 2 {
			for n := i; n+1 < j; n++ {
				add(string(runes[n : n+2]))
			}
		}
		i = j
	}
	return terms
}
