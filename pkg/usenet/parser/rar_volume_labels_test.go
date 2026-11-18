package parser

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestRARMixedVolumeNamingFirstPostedLast(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run(fmt.Sprintf("RAR%d", version), func(t *testing.T) {
			volumes, want := rarGroupDiagnosticVolumes(t, version, false, 3, 'A')
			p := rarGroupDiagnosticParser(t, volumes)
			controlLabels := []string{"Demo.part01.rar", "Demo.part02.rar", "Demo.part03.rar"}
			entry, groups, err := p.Parse(t.Context(), "Control.nzb", rarGroupDiagnosticNZB(volumes, controlLabels))
			if err != nil || len(groups) != 1 {
				t.Fatalf("control grouping failed: groups=%d err=%v", len(groups), err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 1 {
				t.Fatalf("valid archive control rejected: result=%v err=%v", parsed, err)
			}
			assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
			t.Log("consistent-name control passed full/random readback, first volume last in NZB order")
			entry, groups, err = p.Parse(t.Context(), "Mixed.nzb", rarGroupDiagnosticNZB(volumes, []string{"Demo.part01.rar", "Demo.r00", "Demo.r01"}))
			if err != nil || len(groups) != 1 {
				t.Fatalf("mixed naming changed grouping: groups=%d err=%v", len(groups), err)
			}
			parsed, err = p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 1 {
				t.Fatalf("complete single-group archive rejected after naming change: %v", err)
			}
			assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
			t.Log("mixed-name archive passed full/random readback")
		})
	}
}

func TestRARVolumeLabelPositions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order int
	}{
		{"Demo.rar", 0}, {"Demo.part01.rar", 0}, {"Demo.part1.rar", 0},
		{"Demo.r00", 1}, {"Demo.part02.rar", 1}, {"Demo.r01", 2}, {"Demo.part03.rar", 2},
		{"DEMO.PART0004.RAR", 3}, {"Demo.r99", 100}, {"Demo.part101.rar", 100},
		{"Demo.part" + strconv.Itoa(math.MaxInt) + ".rar", math.MaxInt - 1},
		{"opaque", math.MaxInt}, {"Demo.part00.rar", math.MaxInt},
		{"Demo.part" + strings.Repeat("9", 100) + ".rar", math.MaxInt},
		{"Demo.r-1", math.MaxInt}, {"Demo.r+1", math.MaxInt}, {"Demo.rx1", math.MaxInt}, {"Demo.r100", math.MaxInt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := getRARVolumeOrder(tc.name); got != tc.order {
				t.Fatalf("position of %q = %d, want %d", tc.name, got, tc.order)
			}
		})
	}
}

func TestRARMixedVolumeSortAllPermutations(t *testing.T) {
	want := []string{"Demo.part01.rar", "Demo.r00", "Demo.part03.rar", "Demo.r02"}
	var check func([]string, int)
	checked := 0
	check = func(names []string, at int) {
		if at == len(names) {
			got := append([]string(nil), names...)
			sort.Slice(got, func(i, j int) bool { return getRARVolumeOrder(got[i]) < getRARVolumeOrder(got[j]) })
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("wrong sort for %v: %v", names, got)
			}
			checked++
			return
		}
		for next := at; next < len(names); next++ {
			names[at], names[next] = names[next], names[at]
			check(names, at+1)
			names[at], names[next] = names[next], names[at]
		}
	}
	check(append([]string(nil), want...), 0)
	if checked != 24 {
		t.Fatalf("checked %d permutations", checked)
	}
}

func TestRARMixedVolumeLabelsReadback(t *testing.T) {
	styles := []struct {
		name   string
		labels []string
	}{
		{"new style", []string{"Demo.part01.rar", "Demo.part02.rar", "Demo.part03.rar", "Demo.part04.rar"}},
		{"old style", []string{"Demo.rar", "Demo.r00", "Demo.r01", "Demo.r02"}},
		{"mixed first", []string{"Demo.part01.rar", "Demo.r00", "Demo.r01", "Demo.r02"}},
		{"mixed continuations", []string{"Demo.rar", "Demo.r00", "Demo.part03.rar", "Demo.r02"}},
		{"alternating", []string{"Demo.part1.rar", "Demo.part02.rar", "Demo.r01", "Demo.part0004.rar"}},
		{"uppercase", []string{"DEMO.PART01.RAR", "DEMO.R00", "DEMO.PART03.RAR", "DEMO.R02"}},
	}
	for _, version := range []int{4, 5} {
		for _, encrypted := range []bool{false, true} {
			if version == 4 && encrypted {
				continue
			}
			for _, style := range styles {
				t.Run(fmt.Sprintf("RAR%d/encrypted=%v/%s", version, encrypted, style.name), func(t *testing.T) {
					volumes, want := rarAssemblyVolumes(t, version, encrypted, "video.mkv", 4, 'A')
					for _, order := range [][]int{{3, 2, 1, 0}, {1, 3, 0, 2}} {
						t.Run(fmt.Sprint(order), func(t *testing.T) {
							p, xml := rarAssemblyFixture(t, volumes, style.labels, order)
							entry, groups, err := p.Parse(t.Context(), "Mixed.nzb", xml)
							if err != nil || len(groups) != 1 {
								t.Fatalf("mixed grouping: groups=%d err=%v", len(groups), err)
							}
							// Repeated counters cannot influence recognized positions.
							for _, group := range groups {
								for index := range group.Files {
									group.Files[index].Number = 7
								}
							}
							if encrypted {
								entry.Password = rar5HeaderFixturePassword
							}
							parsed, err := p.Process(t.Context(), entry, groups)
							if err != nil || parsed == nil || len(parsed.Files) != 1 {
								t.Fatalf("mixed archive rejected for document order %v: %v", order, err)
							}
							assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
						})
					}
				})
			}
		}
	}
}

func TestRARVolumeAliasAmbiguity(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, labels := range [][]string{
			{"Demo.part01.rar", "Demo.r00", "Demo.part02.rar", "Demo.r02"},
			{"Demo.rar", "Demo.part01.rar", "Demo.r01", "Demo.r02"},
			{"Demo.part01.rar", "Demo.part02.rar", "Demo.part2.rar", "Demo.part04.rar"},
		} {
			t.Run(fmt.Sprintf("RAR%d/%s/%s", version, labels[1], labels[2]), func(t *testing.T) {
				volumes, want := rarAssemblyVolumes(t, version, false, "video.mkv", 4, 'A')
				p, xml := rarAssemblyFixture(t, volumes, labels, []int{0, 2, 1, 3})
				entry, groups, err := p.Parse(t.Context(), "Aliases.nzb", xml)
				if err != nil || len(groups) != 1 {
					t.Fatalf("alias grouping: groups=%d err=%v", len(groups), err)
				}
				parsed, err := p.Process(t.Context(), entry, groups)
				if version == 4 {
					if err == nil || parsed != nil || !strings.Contains(err.Error(), "ambiguous RAR4 volume aliases") {
						t.Fatalf("ambiguous legacy labels were guessed: result=%v err=%v", parsed, err)
					}
					return
				}
				if err != nil || parsed == nil || len(parsed.Files) != 1 {
					t.Fatalf("RAR5 embedded order was overridden by labels: %v", err)
				}
				assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
			})
		}
	}
}

func TestRAR4OpaqueLabelsRetainUploadOrder(t *testing.T) {
	for _, labels := range [][]string{
		{"opaque", "opaque", "opaque", "opaque"},
		{"Demo.rar", "Demo.rar", "Demo.rar", "Demo.rar"},
		{"opaque0.rar", "opaque1.rar", "opaque2.rar", "opaque3.rar"},
	} {
		t.Run(labels[0], func(t *testing.T) {
			volumes, want := rarAssemblyVolumes(t, 4, false, "video.mkv", 4, 'A')
			p, xml := rarAssemblyFixture(t, volumes, labels, nil)
			if labels[0] == labels[1] {
				// Distinct subject names prevent the NZB library from coalescing
				// the entries. Content detection still discovers identical yEnc
				// names, reproducing the real opaque-label fallback path.
				text := string(xml)
				for index := range labels {
					text = strings.Replace(text, fmt.Sprintf(`subject="&quot;%s&quot;`, labels[0]), fmt.Sprintf(`subject="&quot;subject%d&quot;`, index), 1)
				}
				xml = []byte(text)
			}
			entry, groups, err := p.Parse(t.Context(), "Opaque.nzb", xml)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := p.Process(t.Context(), entry, groups)
			if err != nil || parsed == nil || len(parsed.Files) != 1 {
				t.Fatalf("opaque upload order no longer supported: %v", err)
			}
			assertRAR5OrderedReadback(t, p, &parsed.Files[0], want)
		})
	}
}

func TestRARMixedVolumeLabelsInvalidSets(t *testing.T) {
	for _, version := range []int{4, 5} {
		for _, problem := range []string{"missing first", "missing final", "missing interior", "duplicate first", "duplicate continuation", "conflicting size", "corrupt header"} {
			t.Run(fmt.Sprintf("RAR%d/%s", version, problem), func(t *testing.T) {
				volumes, want := rarAssemblyVolumes(t, version, false, "video.mkv", 4, 'A')
				labels := []string{"Demo.part01.rar", "Demo.r00", "Demo.r01", "Demo.r02"}
				order := []int{3, 2, 1, 0}
				switch problem {
				case "missing first":
					order = []int{3, 2, 1}
				case "missing final":
					order = []int{2, 1, 0}
				case "missing interior":
					order = []int{3, 2, 0}
				case "duplicate first", "duplicate continuation":
					original := 0
					if problem == "duplicate continuation" {
						original = 1
					}
					volumes = append(volumes, bytes.Clone(volumes[original]))
					labels = append(labels, "Demo.r03")
					order = append(order, 4)
				case "conflicting size":
					payload := want[69+72 : 69+72+75]
					volumes[2] = rarAssemblyVolume(t, version, false, "video.mkv", payload, len(want)+1, 2, 4, 24, crc32.ChecksumIEEE(payload))
				case "corrupt header":
					start := len(RAR4Signature)
					if version == 5 {
						start = len(RAR5Signature)
					}
					volumes[2][start] ^= 1
				}
				p, xml := rarAssemblyFixture(t, volumes, labels, order)
				entry, groups, err := p.Parse(t.Context(), "Invalid.nzb", xml)
				if err != nil {
					t.Fatal(err)
				}
				if parsed, err := p.Process(t.Context(), entry, groups); err == nil || parsed != nil {
					t.Fatal("invalid archive accepted after label normalization")
				}
			})
		}
	}
}
