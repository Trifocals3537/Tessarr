package parser

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Trifocals3537/tessarr/pkg/usenet/types"
)

var rarPartNumberSuffix = regexp.MustCompile(`\.part(\d+)$`)

// Both naming schemes use the same zero-based position: .rar/.part01.rar = 0,
// .r00/.part02.rar = 1, .r01/.part03.rar = 2. Unknown labels sort last and retain
// the caller's NZB-number fallback. No untrusted label is used as an allocation.
func getRARVolumeOrder(filename string) int {
	_, order, known := rarVolumeLabel(filename)
	if known {
		return order
	}
	return math.MaxInt
}

func rarVolumeLabel(filename string) (base string, order int, known bool) {
	lower := strings.ToLower(filename)
	ext := filepath.Ext(lower)
	stem := strings.TrimSuffix(lower, ext)
	if ext == ".rar" {
		if match := rarPartNumberSuffix.FindStringSubmatchIndex(stem); match != nil {
			number, err := strconv.Atoi(stem[match[2]:match[3]])
			if err != nil || number <= 0 {
				return "", 0, false
			}
			return stem[:match[0]], number - 1, true
		}
		return stem, 0, true
	}
	if len(ext) == 4 && ext[:2] == ".r" && ext[2] >= '0' && ext[2] <= '9' && ext[3] >= '0' && ext[3] <= '9' {
		return stem, int(ext[2]-'0')*10 + int(ext[3]-'0') + 1, true
	}
	return "", 0, false
}

// RAR4 has no validated embedded ordering here. Distinct aliases for the same
// basename/position cannot be ordered safely by NZB number. Identical opaque
// labels and different basenames retain their existing upload-order behavior.
// RAR5 instead validates embedded indices and never calls this check.
func validateRAR4VolumeLabels(volumes []*types.Volume) error {
	type position struct {
		base  string
		order int
	}
	seen := make(map[position]string, len(volumes))
	for _, volume := range volumes {
		base, order, known := rarVolumeLabel(volume.Name)
		if !known {
			continue
		}
		key := position{base, order}
		label := strings.ToLower(volume.Name)
		if previous, exists := seen[key]; exists && previous != label {
			return fmt.Errorf("ambiguous RAR4 volume aliases at position %d", order)
		}
		seen[key] = label
	}
	return nil
}
