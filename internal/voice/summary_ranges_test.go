package voice

import (
	"context"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/summarize"
)

func TestSummaryRelaxedRanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges [][2]int
		valid  bool
	}{
		{"skipped_first", [][2]int{{2, 3}}, true},
		{"middle_gap", [][2]int{{0, 0}, {2, 2}}, true},
		{"overlap", [][2]int{{0, 2}, {2, 3}}, false},
		{"missing_seq", [][2]int{{0, 9}}, false},
		{"missing_interior_endpoint", [][2]int{{1, 2}}, false},
		{"reversed", [][2]int{{2, 1}}, false},
		{"unordered", [][2]int{{2, 3}, {0, 1}}, false},
		{"empty_meaningful", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSummaryFixture(t, 4)
			if tc.name == "missing_interior_endpoint" {
				f.exec(`DELETE FROM voice_transcript_turns WHERE seq=1`)
			}
			f.w.model = fakeSummaryModel(func(context.Context, []summarize.Turn) ([]summarize.Summary, error) {
				result := []summarize.Summary{}
				for _, r := range tc.ranges {
					result = append(result, summarize.Summary{Topic: "topic", Summary: "summary", FirstSeq: r[0], LastSeq: r[1]})
				}
				return result, nil
			})
			f.advance(time.Minute)
			f.step()
			status, reason, attempts, _ := f.state()
			if tc.valid {
				if status != "done" || attempts != 0 {
					t.Fatalf("valid ranges rejected: status=%s reason=%s attempts=%d", status, reason, attempts)
				}
				rows, err := f.w.db.Query(`SELECT first_seq,last_seq FROM voice_call_summaries ORDER BY first_seq`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				i := 0
				for rows.Next() {
					var first, last int
					if err := rows.Scan(&first, &last); err != nil {
						t.Fatal(err)
					}
					if i >= len(tc.ranges) || [2]int{first, last} != tc.ranges[i] {
						t.Fatalf("wrong stored range %d: %d..%d", i, first, last)
					}
					i++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if i != len(tc.ranges) {
					t.Fatalf("stored %d ranges, want %d", i, len(tc.ranges))
				}
			} else if status != "pending" || reason != "invalid_summary" || attempts != 1 || f.rows() != 0 {
				t.Fatalf("invalid ranges accepted: status=%s reason=%s attempts=%d rows=%d", status, reason, attempts, f.rows())
			}
		})
	}
}

func TestSummaryWhitespaceTurnsDoneWithoutRows(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "configured"}[configured], func(t *testing.T) {
			f := newSummaryFixture(t, 2)
			f.exec(`UPDATE voice_transcript_turns SET text=?`, " \n\t\u2003")
			f.w.model = nil
			if configured {
				f.w.model = fakeSummaryModel(func(context.Context, []summarize.Turn) ([]summarize.Summary, error) { f.calls++; return nil, nil })
			}
			f.advance(time.Minute)
			f.step()
			if status, reason, attempts, _ := f.state(); status != "done" || attempts != 0 || f.rows() != 0 || f.calls != 0 {
				t.Fatalf("whitespace not completed: status=%s reason=%s attempts=%d rows=%d calls=%d", status, reason, attempts, f.rows(), f.calls)
			}
		})
	}
}
