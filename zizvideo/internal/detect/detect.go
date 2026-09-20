// Package detect 是剧场剧集识别的唯一内核：单剧场 / 批量一键 / 扫描后后台补齐
// 三方共用同一份解析与同一个写入口，禁止第二份（ITERATION-2 A.2）。
package detect

import (
	"context"
	"path/filepath"

	"github.com/zizdog/zizvideo/internal/domain"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// Change is one derived (season, episode) that a confirm run would write.
type Change struct {
	MediaID    string
	Title      string
	Filename   string
	OldSeason  *int
	OldEpisode *int
	OldLabel   string
	NewSeason  *int
	NewEpisode *int
	NewLabel   string
}

// Result is the read-only outcome for one 剧场.
type Result struct {
	SeriesID      string
	Title         string
	Total         int
	Changes       []Change
	Assigns       []storage.EpisodeAssignment
	ManualSkipped int
	// Unidentified 是"文件名不匹配任何模式、保持未识别"的成员数（不许猜）。
	Unidentified int
}

// MediaName is the filename the parser works on (Title is the fallback for rows
// whose path was cleared). 唯一实现，api/detect 都调它。
func MediaName(m domain.Media) string {
	name := m.Path
	if name == "" {
		name = m.Title
	}
	return filepath.Base(name)
}

// EpisodesFor derives the nullable numbers for one media row (加入剧场时复用）。
func EpisodesFor(m domain.Media) (season, episode *int, ok bool) {
	numbers, ok := media.ParseEpisode(MediaName(m))
	if !ok {
		return nil, nil, false
	}
	season, episode = media.EpisodePointers(numbers)
	return season, episode, true
}

// Series re-derives every member's number from its filename. It only reads:
// manual rows are skipped and counted, unrecognized names are left 未识别.
func Series(ctx context.Context, db *storage.DB, scope domain.LibraryScope, seriesID string) (Result, error) {
	out := Result{SeriesID: seriesID, Changes: []Change{}, Assigns: []storage.EpisodeAssignment{}}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	series, err := db.GetSeries(seriesID)
	if err != nil {
		return out, err
	}
	out.Title = series.Title
	eps, medias, err := db.ListSeriesEpisodes(scope, seriesID)
	if err != nil {
		return out, err
	}
	out.Total = len(eps)
	for i := range eps {
		if eps[i].EpisodeSource == domain.EpisodeSourceManual {
			out.ManualSkipped++
			continue
		}
		season, episode, ok := EpisodesFor(medias[i])
		if !ok {
			out.Unidentified++
			continue // 识别不到就不写、不改，绝不用猜的值填库
		}
		if intPtrEqual(season, eps[i].Season) && intPtrEqual(episode, eps[i].Episode) &&
			eps[i].EpisodeSource == domain.EpisodeSourceFilename {
			continue
		}
		out.Changes = append(out.Changes, Change{
			MediaID: medias[i].ID, Title: medias[i].Title, Filename: MediaName(medias[i]),
			OldSeason: eps[i].Season, OldEpisode: eps[i].Episode,
			OldLabel:  media.EpisodeLabel(eps[i].Season, eps[i].Episode),
			NewSeason: season, NewEpisode: episode,
			NewLabel: media.EpisodeLabel(season, episode),
		})
		out.Assigns = append(out.Assigns, storage.EpisodeAssignment{
			MediaID: medias[i].ID, Season: season, Episode: episode,
			Source: domain.EpisodeSourceFilename,
		})
	}
	return out, nil
}

// Apply is the only write path: it forwards to storage.ApplySeriesEpisodes,
// whose SQL carries `<> 'manual'`（series.go）—— 自动识别永不覆盖 manual。
func Apply(ctx context.Context, db *storage.DB, seriesID string, assigns []storage.EpisodeAssignment) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return db.ApplySeriesEpisodes(seriesID, assigns)
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
