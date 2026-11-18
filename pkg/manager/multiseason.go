package manager

import (
	"crypto/md5"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	debridTypes "github.com/Trifocals3537/tessarr/pkg/debrid/types"
	"github.com/Trifocals3537/tessarr/pkg/storage"
)

// Multi-season detection patterns
var (
	// Pre-compiled patterns for multi-season replacement
	multiSeasonReplacements = []multiSeasonPattern{
		// S01-08 -> S01 (or whatever target season)
		{regexp.MustCompile(`(?i)S(\d{1,2})-\d{1,2}`), "S%02d"},

		// S01-S08 -> S01
		{regexp.MustCompile(`(?i)S(\d{1,2})-S\d{1,2}`), "S%02d"},

		// Season 1-8 -> Season 1
		{regexp.MustCompile(`(?i)Season\.?\s*\d{1,2}-\d{1,2}`), "Season %02d"},

		// Seasons 1-8 -> Season 1
		{regexp.MustCompile(`(?i)Seasons\.?\s*\d{1,2}-\d{1,2}`), "Season %02d"},

		// Complete Series -> Season X
		{regexp.MustCompile(`(?i)Complete[.\s]*Series`), "Season %02d"},

		// All Seasons -> Season X
		{regexp.MustCompile(`(?i)All[.\s]*Seasons?`), "Season %02d"},
	}

	// Also pre-compile other patterns
	seasonPattern     = regexp.MustCompile(`(?i)(?:season\.?\s*|s)(\d{1,2})`)
	episodePattern    = regexp.MustCompile(`(?i)S\d{1,2}E\d{1,3}(?:E\d{1,3})*`)
	qualityIndicators = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|BluRay|WEB-DL|HDTV|x264|x265|HEVC)`)

	multiSeasonIndicators = []*regexp.Regexp{
		regexp.MustCompile(`(?i)complete[.\s]*series`),
		regexp.MustCompile(`(?i)all[.\s]*seasons?`),
		regexp.MustCompile(`(?i)season\.?\s*\d+\s*-\s*\d+`),
		regexp.MustCompile(`(?i)s\d+\s*-\s*s?\d+`),
		regexp.MustCompile(`(?i)seasons?\s*\d+\s*-\s*\d+`),
	}
)

type multiSeasonPattern struct {
	pattern     *regexp.Regexp
	replacement string
}

// SeasonInfo represents information about a season in a multi-season torrent
type SeasonInfo struct {
	SeasonNumber int
	Files        []*storage.File
	InfoHash     string
	Name         string
}

// convertToMultiSeason converts a normal torrent to a multi-season torrents
func convertToMultiSeason(torrent *storage.Entry, seasons []SeasonInfo) []*storage.Entry {
	seasonResults := make([]*storage.Entry, 0, len(seasons))
	for _, seasonInfo := range seasons {
		// Filter files to only include this season's files
		seasonFiles := make(map[string]*storage.File)
		size := int64(0)
		for _, file := range seasonInfo.Files {
			fileCopy := *file
			if file.ByteRange != nil {
				byteRange := *file.ByteRange
				fileCopy.ByteRange = &byteRange
			}
			seasonFiles[file.Name] = &fileCopy
			size += file.Size
		}

		// Claim the child before queue publication, including the wait for mount
		// files. Startup recovery clears the claim if the parent is interrupted.
		seasonTorrent := &storage.Entry{
			Protocol:                torrent.Protocol,
			InfoHash:                seasonInfo.InfoHash,
			Name:                    seasonInfo.Name,
			OriginalFilename:        torrent.OriginalFilename,
			Size:                    size,
			Bytes:                   size,
			Magnet:                  torrent.Magnet,
			Category:                torrent.Category,
			SavePath:                torrent.SavePath,
			Status:                  debridTypes.TorrentStatusDownloading,
			State:                   storage.EntryStateDownloading,
			IsDownloading:           true,
			ActiveProvider:          torrent.ActiveProvider,
			Action:                  torrent.Action,
			DownloadUncached:        torrent.DownloadUncached,
			AdmissionUncachedPolicy: torrent.AdmissionUncachedPolicy,
			CreatedAt:               time.Now(),
			UpdatedAt:               time.Now(),
			AddedOn:                 time.Now(),
			Providers:               make(map[string]*storage.ProviderEntry),
			Files:                   seasonFiles,
			FileScopeRestricted:     true,
			SeasonSource:            &storage.SeasonSource{InfoHash: torrent.InfoHash, Season: seasonInfo.SeasonNumber},
			SkipMultiSeason:         true,
		}

		// Copy placement
		if torrent.IsTorrent() && torrent.OutputName != "" {
			seasonTorrent.OutputName = storage.NewTorrentOutputName(seasonTorrent.Name, seasonTorrent.InfoHash)
		}
		for name, placement := range torrent.Providers {
			if placement == nil {
				continue
			}
			copy := *placement
			copy.Files = make(map[string]*storage.ProviderFile, len(seasonFiles))
			for fileName := range seasonFiles {
				if file := placement.Files[fileName]; file != nil {
					fileCopy := *file
					copy.Files[fileName] = &fileCopy
				}
			}
			seasonTorrent.Providers[name] = &copy
		}
		seasonResults = append(seasonResults, seasonTorrent)
	}
	return seasonResults
}

func replaceMultiSeasonPattern(name string, targetSeason int) string {
	result := name

	// Apply each pre-compiled pattern replacement
	for _, msp := range multiSeasonReplacements {
		if msp.pattern.MatchString(result) {
			replacement := fmt.Sprintf(msp.replacement, targetSeason)
			result = msp.pattern.ReplaceAllString(result, replacement)
			return result
		}
	}

	// If no multi-season pattern found, try to insert season info intelligently
	return insertSeasonIntoName(result, targetSeason)
}

func insertSeasonIntoName(name string, seasonNum int) string {
	// A season job must not retain the parent's single-episode title.
	if episodePattern.MatchString(name) {
		return episodePattern.ReplaceAllString(name, fmt.Sprintf("S%02d", seasonNum))
	}
	if seasonPattern.MatchString(name) {
		return seasonPattern.ReplaceAllStringFunc(name, func(match string) string {
			if strings.HasPrefix(strings.ToLower(match), "season") {
				return fmt.Sprintf("Season %02d", seasonNum)
			}
			return fmt.Sprintf("S%02d", seasonNum)
		})
	}

	// Try to find a good insertion point (before quality indicators)
	if loc := qualityIndicators.FindStringIndex(name); loc != nil {
		// Insert season before quality info
		before := strings.TrimSpace(name[:loc[0]])
		after := name[loc[0]:]
		return fmt.Sprintf("%s S%02d %s", before, seasonNum, after)
	}

	// If no quality indicators found, append at the end
	return fmt.Sprintf("%s S%02d", name, seasonNum)
}

func findAllSeasons(files []*storage.File) map[int]bool {
	seasons := make(map[int]bool)

	for _, file := range files {
		// Check filename first
		if season := extractSeason(file.Name); season > 0 {
			seasons[season] = true
			continue
		}

		// Check full path
		if season := extractSeason(file.Path); season > 0 {
			seasons[season] = true
		}
	}

	return seasons
}

// extractSeason pulls season number from a string
func extractSeason(text string) int {
	matches := seasonPattern.FindStringSubmatch(text)
	if len(matches) > 1 {
		if num, err := strconv.Atoi(matches[1]); err == nil && num > 0 && num < 100 {
			return num
		}
	}
	return 0
}

func hasMultiSeasonIndicators(torrentName string) bool {
	for _, pattern := range multiSeasonIndicators {
		if pattern.MatchString(torrentName) {
			return true
		}
	}
	return false
}

// groupFilesBySeason puts files into season buckets
func groupFilesBySeason(files []*storage.File, knownSeasons map[int]bool) map[int][]*storage.File {
	groups := make(map[int][]*storage.File)

	// Initialize groups
	for season := range knownSeasons {
		groups[season] = []*storage.File{}
	}

	for _, file := range files {
		// Try to find season from filename or path
		season := extractSeason(file.Name)
		if season == 0 {
			season = extractSeason(file.Path)
		}

		// If we found a season and it's known, add the file
		if season > 0 && knownSeasons[season] {
			groups[season] = append(groups[season], file)
		} else {
			// If no season found, try path-based inference
			inferredSeason := inferSeasonFromPath(file.Path, knownSeasons)
			if inferredSeason > 0 {
				groups[inferredSeason] = append(groups[inferredSeason], file)
			} else if len(knownSeasons) == 1 {
				// If only one season exists, default to it
				for season := range knownSeasons {
					groups[season] = append(groups[season], file)
				}
			}
		}
	}

	return groups
}

func inferSeasonFromPath(path string, knownSeasons map[int]bool) int {
	pathParts := strings.SplitSeq(path, "/")

	for part := range pathParts {
		if season := extractSeason(part); season > 0 && knownSeasons[season] {
			return season
		}
	}

	return 0
}

// Helper to get sorted season list for logging
func getSortedSeasons(seasons map[int]bool) []int {
	result := make([]int, 0, len(seasons))
	for season := range seasons {
		result = append(result, season)
	}
	sort.Ints(result)
	return result
}

// generateSeasonHash creates a unique hash for a season based on original hash
func generateSeasonHash(originalHash string, seasonNumber int) string {
	source := fmt.Sprintf("%s-%d", originalHash, seasonNumber)
	hash := md5.Sum([]byte(source))
	return fmt.Sprintf("%x", hash)
}

// seasonSource loads the real mounted/provider entry and proves that a child
// still describes exactly its original season. Never infer this relationship
// from a display title, a provider ID, or an arbitrary file's InfoHash.
func (m *Manager) seasonSource(child *storage.Entry) (*storage.Entry, error) {
	if child == nil || !child.IsTorrent() || !child.FileScopeRestricted || !child.SkipMultiSeason ||
		child.SeasonSource == nil || child.SeasonSource.InfoHash == "" ||
		child.SeasonSource.Season <= 0 || child.SeasonSource.Season >= 100 ||
		child.InfoHash != generateSeasonHash(child.SeasonSource.InfoHash, child.SeasonSource.Season) {
		return nil, fmt.Errorf("invalid or missing season source reference; re-import the parent torrent")
	}
	if m == nil || m.storage == nil {
		return nil, fmt.Errorf("season source storage unavailable")
	}
	source, err := m.GetEntry(child.SeasonSource.InfoHash)
	if err != nil {
		return nil, fmt.Errorf("load season source: %w", err)
	}
	if source == nil || !source.IsTorrent() || source.FileScopeRestricted || source.SeasonSource != nil {
		return nil, fmt.Errorf("invalid season source entry")
	}
	placement, scoped := source.GetActiveProvider(), child.GetActiveProvider()
	if placement == nil || scoped == nil || source.ActiveProvider != child.ActiveProvider ||
		placement.Provider != scoped.Provider || placement.ID != scoped.ID ||
		placement.Status != debridTypes.TorrentStatusDownloaded {
		return nil, fmt.Errorf("season source provider placement changed")
	}
	files := source.GetActiveFiles()
	expected := groupFilesBySeason(files, findAllSeasons(files))[child.SeasonSource.Season]
	if len(expected) == 0 || len(child.Files) != len(expected) || len(scoped.Files) != len(expected) {
		return nil, fmt.Errorf("season source file scope changed")
	}
	for _, original := range expected {
		file := child.Files[original.Name]
		if file == nil || file.Deleted || file.ID != original.ID || file.Name != original.Name ||
			file.Path != original.Path || file.Size != original.Size ||
			(file.ByteRange == nil) != (original.ByteRange == nil) ||
			(file.ByteRange != nil && *file.ByteRange != *original.ByteRange) {
			return nil, fmt.Errorf("season source canonical file changed")
		}
		providerFile, childFile := placement.Files[original.Name], scoped.Files[original.Name]
		if providerFile == nil || childFile == nil || providerFile.Id != childFile.Id ||
			providerFile.Path != childFile.Path || providerFile.Link != childFile.Link {
			return nil, fmt.Errorf("season source provider file changed")
		}
	}
	return source, nil
}
